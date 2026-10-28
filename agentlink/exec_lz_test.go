package agentlink

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// lzCall is one SendText the Executor made.
type lzCall struct{ Session, Text, Origin string }

// lzSender scripts the Lazada channel's SendText. It numbers message ids
// LZ1, LZ2, ... and can fail from a given part on.
type lzSender struct {
	fakeLazada
	cmu   sync.Mutex
	calls []lzCall
	// Failing, when set, is returned by every call. With PartsOK > 0 the call
	// first "sends" that many parts and then fails with a *LazadaPartialError.
	Failing error
	PartsOK int
	Total   int
	seq     int
}

func (l *lzSender) SendText(_ context.Context, session, text, origin string) ([]string, error) {
	l.cmu.Lock()
	defer l.cmu.Unlock()
	l.calls = append(l.calls, lzCall{session, text, origin})
	if l.Failing != nil && l.PartsOK == 0 {
		return nil, l.Failing
	}
	n := 1
	if l.PartsOK > 0 {
		n = l.PartsOK
	}
	var ids []string
	for range n {
		l.seq++
		ids = append(ids, fmt.Sprintf("LZ%d", l.seq))
	}
	if l.Failing != nil {
		return ids, &LazadaPartialError{Sent: n, Total: l.Total, SentText: "first part", Err: l.Failing}
	}
	return ids, nil
}

func (l *lzSender) Calls() []lzCall {
	l.cmu.Lock()
	defer l.cmu.Unlock()
	return append([]lzCall(nil), l.calls...)
}

// SendText on the plain fake (used by the older tests) sends nothing.
func (f *fakeLazada) SendText(_ context.Context, session, text, origin string) ([]string, error) {
	if f.send != nil {
		return f.send(session, text, origin)
	}
	return nil, nil
}

func lzSend(t *testing.T, w *world, id string, p Send) Envelope {
	t.Helper()
	p.Conversation = lzConv
	return sendEnv(t, w, id, p)
}

func lzExec(t *testing.T) (*world, *Executor, *lzSender) {
	t.Helper()
	w := lzWorld(t)
	e := w.executor()
	lz := &lzSender{}
	e.Lazada = lz
	return w, e, lz
}

// A reply goes to Lazada with origin "agent", is mirrored with the robot and
// the signature, and the result carries the first Lazada id.
func TestLazadaReplySendsMirrorsAndReturnsTheFirstID(t *testing.T) {
	w, e, lz := lzExec(t)

	res := e.Handle(context.Background(), lzSend(t, w, "CMD1", Send{Kind: "reply", Text: "  Hello <b>there</b>  ", Signature: "Sent by Loki", ReplyTo: "m77", Title: "ignored"}))
	if !res.OK || res.Error != "" || res.HubMsgID != "LZ1" || res.DeliveredAt == "" {
		t.Fatalf("result = %+v", res)
	}
	if got := lz.Calls(); len(got) != 1 || got[0] != (lzCall{"100094063_2_1011822749_1_103", "Hello <b>there</b>", "agent"}) {
		t.Errorf("sends = %+v", got)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].Thread != 1300 || !posts[0].HTML ||
		posts[0].Text != "🤖 Hello &lt;b&gt;there&lt;/b&gt;\n<i>✓ Sent by Loki</i>" {
		t.Errorf("mirror = %+v", posts)
	}
	if !e.Guard.Has("LZ1") {
		t.Error("the sent message is not remembered as the Agent's own")
	}
	if n := len(w.wa.Calls()) + len(w.customer.Sends()); n != 0 {
		t.Errorf("%d WhatsApp/Telegram sends for a Lazada conversation", n)
	}
}

func TestLazadaReplyReplacesTheCard(t *testing.T) {
	w, e, _ := lzExec(t)
	chatID, msgID := withDraft(t, w, e, lzConv)

	res := e.Handle(context.Background(), lzSend(t, w, "R1", Send{Kind: "reply", Text: "Boleh!", Signature: "Loki", ReplacesCard: draftID}))
	if !res.OK || res.HubMsgID != "LZ1" {
		t.Fatalf("result = %+v", res)
	}
	if got := w.topics.Deleted(); len(got) != 1 || got[0] != [2]int64{chatID, msgID} || cardKnown(t, w) {
		t.Errorf("deleted = %v, card known = %v", got, cardKnown(t, w))
	}
	if len(w.topics.Posts()) != 1 {
		t.Errorf("posts = %+v, want only the signed mirror", w.topics.Posts())
	}
}

// A4: Lazada takes plain text only. Nothing is sent, nothing mirrored.
func TestLazadaReplyRefusesWhatLazadaCannotCarry(t *testing.T) {
	copyables := Copyables{{Label: "Alias", Value: "my.alias"}}
	for name, p := range map[string]Send{
		"media":      {Kind: "reply", Text: "x", Media: []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}}},
		"copyables":  {Kind: "reply", Text: "x", Copyables: &copyables},
		"link":       {Kind: "reply", Text: "x", Link: orderLink},
		"empty":      {Kind: "reply", Text: ""},
		"spaces":     {Kind: "reply", Text: " \n\t "},
		"empty html": {Kind: "reply", Text: "<b> </b>", Format: FormatHTML},
	} {
		t.Run(name, func(t *testing.T) {
			w, e, lz := lzExec(t)
			res := e.Handle(context.Background(), lzSend(t, w, "CMD", p))
			if res.OK || res.Error != ErrInvalid {
				t.Errorf("result = %+v", res)
			}
			if len(lz.Calls()) != 0 || len(w.topics.Posts()) != 0 {
				t.Errorf("sent %+v, posted %+v", lz.Calls(), w.topics.Posts())
			}
		})
	}
}

// §5c: Lazada shows plain text, so html is stripped before sending, and the
// mirror shows what was sent.
func TestLazadaReplyStripsHTML(t *testing.T) {
	w, e, lz := lzExec(t)
	res := e.Handle(context.Background(), lzSend(t, w, "CMD1", Send{
		Kind: "reply", Format: FormatHTML, Text: "<b>Price</b> &amp; <s>old</s> <a href=\"https://x.test/a\">page</a>",
	}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	want := "Price & old page (https://x.test/a)"
	if got := lz.Calls(); len(got) != 1 || got[0].Text != want {
		t.Errorf("sent %+v, want %q", got, want)
	}
	if posts := w.topics.Posts(); len(posts) != 1 || posts[0].Text != "🤖 Price &amp; old page (https://x.test/a)" {
		t.Errorf("mirror = %+v", posts)
	}
}

func TestLazadaReplyExpired(t *testing.T) {
	w, e, lz := lzExec(t)
	env := lzSend(t, w, "CMD1", Send{Kind: "reply", Text: "hi"})
	w.clock.Advance(11 * time.Minute)
	res := e.Handle(context.Background(), env)
	if res.OK || res.Error != ErrExpired {
		t.Errorf("result = %+v", res)
	}
	if len(lz.Calls()) != 0 || len(w.topics.Posts()) != 0 {
		t.Error("an expired command sent something")
	}
}

func TestLazadaReplyWithoutChannelOrTopicIsUnknownConversation(t *testing.T) {
	w := lzWorld(t)
	e := w.executor() // no Lazada channel
	res := e.Handle(context.Background(), lzSend(t, w, "CMD1", Send{Kind: "reply", Text: "hi"}))
	if res.OK || res.Error != ErrUnknownConversation {
		t.Errorf("no channel: %+v", res)
	}

	e.Lazada = &lzSender{}
	delete(w.bridge.threads, lzConv)
	res = e.Handle(context.Background(), lzSend(t, w, "CMD2", Send{Kind: "reply", Text: "hi"}))
	if res.OK || res.Error != ErrUnknownConversation {
		t.Errorf("no topic: %+v", res)
	}
	if n := len(e.Lazada.(*lzSender).Calls()); n != 0 {
		t.Errorf("%d sends without a topic to mirror to", n)
	}
}

func TestLazadaReplyFailuresMapToResultCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"rate limited":   {fmt.Errorf("wrapped: %w", ErrCustomerRateLimited), ErrRateLimited},
		"not authorized": {fmt.Errorf("wrapped: %w", ErrCustomerBlocked), ErrCustomerUnreachable},
		"anything else":  {errors.New("boom"), ErrInternal},
	} {
		t.Run(name, func(t *testing.T) {
			w, e, lz := lzExec(t)
			lz.Failing = tc.err
			withDraft(t, w, e, lzConv)
			res := e.Handle(context.Background(), lzSend(t, w, "CMD1", Send{Kind: "reply", Text: "hi", ReplacesCard: draftID}))
			if res.OK || res.Error != tc.want || res.HubMsgID != "" {
				t.Errorf("result = %+v, want %s", res, tc.want)
			}
			if len(w.topics.Posts()) != 0 || len(w.topics.Deleted()) != 0 || !cardKnown(t, w) {
				t.Errorf("a failed send mirrored or touched the card: %+v", w.topics.Posts())
			}
		})
	}
}

// Part of the answer reached the buyer: it is mirrored, staff are told how much
// went out, the card stays, and the result keeps the first id.
func TestLazadaPartialReplyIsMirroredAndNoted(t *testing.T) {
	w, e, lz := lzExec(t)
	lz.Failing, lz.PartsOK, lz.Total = fmt.Errorf("%w: slow down", ErrCustomerRateLimited), 2, 3
	withDraft(t, w, e, lzConv)

	res := e.Handle(context.Background(), lzSend(t, w, "CMD1", Send{Kind: "reply", Text: "a long answer", Signature: "Loki", ReplacesCard: draftID}))
	if res.OK || res.Error != ErrRateLimited || res.HubMsgID != "LZ1" {
		t.Fatalf("result = %+v", res)
	}
	posts := w.topics.Posts()
	if len(posts) != 2 {
		t.Fatalf("posts = %+v, want the mirror and the note", posts)
	}
	if posts[0].Text != "🤖 first part\n<i>✓ Loki</i>" {
		t.Errorf("mirror = %q, want what went out", posts[0].Text)
	}
	if !strings.HasPrefix(posts[1].Text, "ℹ️ Only 2 of 3 parts reached Lazada: ") || posts[1].HTML {
		t.Errorf("note = %+v", posts[1])
	}
	if !e.Guard.Has("LZ1") || !e.Guard.Has("LZ2") {
		t.Error("the parts that went out are not remembered as the Agent's own")
	}
	if len(w.topics.Deleted()) != 0 || !cardKnown(t, w) {
		t.Error("a partial reply retired the card")
	}
}

// The Hub remembers command ids for 24 h: a retry returns the first result and
// sends nothing.
func TestLazadaRetryOfACommandDoesNotSendAgain(t *testing.T) {
	w, e, lz := lzExec(t)
	env := lzSend(t, w, "CMD1", Send{Kind: "reply", Text: "hi"})
	first := e.Handle(context.Background(), env)
	second := e.Handle(context.Background(), env)
	if !first.OK || first != second {
		t.Fatalf("first %+v, second %+v", first, second)
	}
	if n := len(lz.Calls()); n != 1 {
		t.Errorf("%d sends for a retried command", n)
	}
	if n := len(w.topics.Posts()); n != 1 {
		t.Errorf("%d mirrors for a retried command", n)
	}
}
