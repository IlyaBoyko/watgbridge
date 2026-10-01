package agentlink

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func sendEnv(t *testing.T, w *world, id string, p Send) Envelope {
	t.Helper()
	if p.ExpiresAt == "" {
		p.ExpiresAt = FormatTS(w.clock.Now().Add(10 * time.Minute))
	}
	if p.Media == nil {
		p.Media = []Media{}
	}
	if p.Conversation == "" {
		p.Conversation = "wa:60123456789@s.whatsapp.net"
	}
	env, err := NewEnvelope(id, TypeSend, w.clock.Now(), p)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// A8: send reply — WhatsApp first, then the robot mirror and the stored pair.
func TestSendReplyMirrorsIntoTopicAndRecordsPair(t *testing.T) {
	w := newWorld(t)
	w.bridge.participants["3EB0A1B2C3D4E5F6"] = "60123456789@s.whatsapp.net"
	e := w.executor()

	res := e.Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "Boleh! 2-3 hari bekerja <b>&</b>", ReplyTo: "3EB0A1B2C3D4E5F6"}))
	if !res.OK || res.Error != "" || res.CommandID != "CMD1" || res.HubMsgID != "WA001" || res.DeliveredAt != "2026-10-01T09:30:00.000Z" {
		t.Fatalf("result = %+v", res)
	}

	calls := w.wa.Calls()
	if len(calls) != 1 || calls[0].Kind != "text" || calls[0].To != "60123456789@s.whatsapp.net" || calls[0].Text != "Boleh! 2-3 hari bekerja <b>&</b>" {
		t.Fatalf("whatsapp calls = %+v", calls)
	}
	if q := calls[0].Quote; q == nil || q.StanzaID != "3EB0A1B2C3D4E5F6" || q.Participant != "60123456789@s.whatsapp.net" {
		t.Errorf("quote = %+v", q)
	}

	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].Thread != 1234 || posts[0].Text != "🤖 Boleh! 2-3 hari bekerja <b>&</b>" {
		t.Fatalf("topic posts = %+v", posts)
	}
	pairs := w.bridge.Pairs()
	if len(pairs) != 1 || pairs[0] != (pairRecord{"WA001", "60123456789@s.whatsapp.net", 9001, 1234}) {
		t.Errorf("pairs = %+v", pairs)
	}
	if !e.Guard.Has("WA001") {
		t.Error("the sent WhatsApp id is not remembered as the Agent's own")
	}
}

func TestSendReplyQuotesStoredParticipant(t *testing.T) {
	w := newWorld(t)
	w.bridge.participants["OURS"] = "60199999999:3@s.whatsapp.net"
	w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "hi", ReplyTo: "OURS"}))
	if q := w.wa.Calls()[0].Quote; q == nil || q.Participant != "60199999999:3@s.whatsapp.net" {
		t.Errorf("quote = %+v", q)
	}
}

func TestSendReplyWithMedia(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	res := e.Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Text: "DuitNow QR",
		Media: []Media{{Kind: "image", Mime: "image/png", Filename: "duitnow.png", Size: 70, Data: pngB64}},
	}))
	if !res.OK || res.HubMsgID != "WA001" {
		t.Fatalf("result = %+v", res)
	}
	raw, _ := base64.StdEncoding.DecodeString(pngB64)
	calls := w.wa.Calls()
	if len(calls) != 1 || calls[0].Kind != "image" || string(calls[0].Data) != string(raw) || calls[0].Text != "DuitNow QR" || calls[0].Mime != "image/png" {
		t.Fatalf("calls = %+v", calls)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].Media == nil || posts[0].Media.Kind != "image" || posts[0].Text != "🤖 DuitNow QR" {
		t.Fatalf("posts = %+v", posts)
	}
	if len(w.bridge.Pairs()) != 1 {
		t.Error("the media mirror was not recorded as a pair")
	}

	// A document keeps its file name.
	e.Handle(context.Background(), sendEnv(t, w, "CMD2", Send{
		Kind: "reply", Media: []Media{{Kind: "document", Mime: "application/pdf", Filename: "invoice.pdf", Size: 70, Data: pngB64, Caption: "Invoice"}},
	}))
	calls = w.wa.Calls()
	if len(calls) != 2 || calls[1].Kind != "document" || calls[1].Filename != "invoice.pdf" || calls[1].Text != "Invoice" {
		t.Fatalf("document call = %+v", calls)
	}
}

func TestSendReplyCustomerUnreachable(t *testing.T) {
	w := newWorld(t)
	w.wa.fail = errors.New("not on whatsapp")
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "hi"}))
	if res.OK || res.Error != ErrCustomerUnreachable || res.HubMsgID != "" {
		t.Fatalf("result = %+v", res)
	}
	if n := len(w.topics.Posts()); n != 0 {
		t.Errorf("%d topic posts after a failed WhatsApp send, want none", n)
	}
	if n := len(w.bridge.Pairs()); n != 0 {
		t.Errorf("%d pairs recorded, want none", n)
	}
}

func TestSendReplySucceedsEvenIfMirrorFails(t *testing.T) {
	w := newWorld(t)
	w.topics.fail = errors.New("telegram down")
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "hi"}))
	if !res.OK || res.HubMsgID != "WA001" {
		t.Fatalf("the customer got the message; result = %+v", res)
	}
	if n := len(w.bridge.Pairs()); n != 0 {
		t.Errorf("a pair was recorded for a mirror that was never posted")
	}
}

func TestSendNotePostsOnlyInTopic(t *testing.T) {
	w := newWorld(t)
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "note", Text: "Slip received for CM-261001-AB12 (RM 240)."}))
	if !res.OK || res.HubMsgID != "" {
		t.Fatalf("result = %+v", res)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].Thread != 1234 || posts[0].Text != "ℹ️ Slip received for CM-261001-AB12 (RM 240)." {
		t.Fatalf("posts = %+v", posts)
	}
	if n := len(w.wa.Calls()); n != 0 {
		t.Errorf("%d WhatsApp sends for a note, want 0", n)
	}
	if n := len(w.bridge.Pairs()); n != 0 {
		t.Errorf("%d pairs for a note, want 0", n)
	}
}

// A7: the Agent can never send to anything but a one-to-one chat.
func TestSendRejectsNonOneToOneConversations(t *testing.T) {
	w := newWorld(t)
	w.bridge.threads["120363025246125486@g.us"] = 77
	w.bridge.threads["60199999999@s.whatsapp.net"] = 78
	e := w.executor()
	for i, conv := range []string{
		"wa:120363025246125486@g.us", "wa:status@broadcast", "wa:120363012345678901@newsletter",
		"wa:60199999999@s.whatsapp.net", "tg:5550001111", "lz:abc", "wa:60111111111@s.whatsapp.net", // last: no topic
	} {
		for _, kind := range []string{"reply", "note"} {
			res := e.Handle(context.Background(), sendEnv(t, w, "CMD-"+kind+string(rune('a'+i)), Send{Kind: kind, Text: "hi", Conversation: conv}))
			if res.OK || res.Error != ErrUnknownConversation {
				t.Errorf("%s %s: result = %+v", kind, conv, res)
			}
		}
	}
	if len(w.wa.Calls()) != 0 || len(w.topics.Posts()) != 0 {
		t.Error("something was sent")
	}
}

func TestSendInvalidPayloads(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	for name, p := range map[string]Send{
		"reply with nothing":     {Kind: "reply"},
		"note with nothing":      {Kind: "note"},
		"reply with video":       {Kind: "reply", Media: []Media{{Kind: "video", Mime: "video/mp4", Size: 1, Data: "AA=="}}},
		"reply media no data":    {Kind: "reply", Media: []Media{{Kind: "image", Mime: "image/png", Size: 1}}},
		"reply media too large":  {Kind: "reply", Media: []Media{{Kind: "image", Mime: "image/png", Size: 9 << 20, TooLarge: true}}},
		"reply media bad base64": {Kind: "reply", Media: []Media{{Kind: "image", Mime: "image/png", Size: 3, Data: "!!!"}}},
	} {
		if res := e.Handle(context.Background(), sendEnv(t, w, "CMD-"+name, p)); res.OK || res.Error != ErrInvalid {
			t.Errorf("%s: result = %+v", name, res)
		}
	}
	// An envelope whose payload does not validate at all.
	bad := Envelope{V: 1, ID: "BAD", Type: TypeSend, TS: FormatTS(w.clock.Now()), Payload: []byte(`{"kind":"reply"}`)}
	if res := e.Handle(context.Background(), bad); res.OK || res.Error != ErrInvalid || res.CommandID != "BAD" {
		t.Errorf("bad payload: %+v", res)
	}
	if len(w.wa.Calls()) != 0 {
		t.Error("something was sent")
	}
}

// A5: a repeated command id returns the stored result without executing again.
func TestRepeatedCommandReturnsStoredResult(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	env := sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "hi"})

	first := e.Handle(context.Background(), env)
	second := e.Handle(context.Background(), env)
	if first != second || !first.OK {
		t.Fatalf("first %+v, second %+v", first, second)
	}
	if n := len(w.wa.Calls()); n != 1 {
		t.Errorf("WhatsApp was called %d times, want 1", n)
	}
	if n := len(w.topics.Posts()); n != 1 {
		t.Errorf("%d topic posts, want 1", n)
	}

	// A failed command is remembered too: the Agent's retry must not suddenly send.
	w.wa.fail = errors.New("down")
	failed := e.Handle(context.Background(), sendEnv(t, w, "CMD2", Send{Kind: "reply", Text: "again"}))
	w.wa.fail = nil
	if again := e.Handle(context.Background(), sendEnv(t, w, "CMD2", Send{Kind: "reply", Text: "again"})); again != failed {
		t.Errorf("retry of a failed command: %+v, want %+v", again, failed)
	}
	if n := len(w.wa.Calls()); n != 1 {
		t.Errorf("WhatsApp was called %d times, want still 1", n)
	}
}

// A5: results are pruned after 24 hours.
func TestCommandMemoryPrunesAfter24Hours(t *testing.T) {
	w := newWorld(t)
	if err := Migrate(w.db); err != nil {
		t.Fatal(err)
	}
	mem := NewCommandMemory(w.db, w.clock)
	if err := mem.Put(Result{CommandID: "OLD", OK: true, HubMsgID: "X"}); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(23 * time.Hour)
	if err := mem.Put(Result{CommandID: "NEW", OK: true}); err != nil {
		t.Fatal(err)
	}
	if n, err := mem.Prune(); err != nil || n != 0 {
		t.Fatalf("pruned %d (%v) after 23h, want 0", n, err)
	}
	w.clock.Advance(2 * time.Hour) // OLD is now 25 h old, NEW 2 h
	if n, err := mem.Prune(); err != nil || n != 1 {
		t.Fatalf("pruned %d (%v) after 25h, want 1", n, err)
	}
	if _, ok, _ := mem.Get("OLD"); ok {
		t.Error("OLD survived the prune")
	}
	if res, ok, _ := mem.Get("NEW"); !ok || !res.OK {
		t.Error("NEW was pruned")
	}

	// After pruning, the same id executes again.
	e := w.executor()
	e.Memory = mem
	env := sendEnv(t, w, "OLD", Send{Kind: "reply", Text: "hi"})
	e.Handle(context.Background(), env)
	if n := len(w.wa.Calls()); n != 1 {
		t.Errorf("calls = %d, want 1 (the pruned id runs again)", n)
	}
}

// A5: an expired command does nothing and answers "expired".
func TestExpiredCommandDoesNothing(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	env := sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "hi", ExpiresAt: FormatTS(w.clock.Now().Add(-time.Second))})
	res := e.Handle(context.Background(), env)
	if res.OK || res.Error != ErrExpired || res.CommandID != "CMD1" {
		t.Fatalf("result = %+v", res)
	}
	if len(w.wa.Calls()) != 0 || len(w.topics.Posts()) != 0 {
		t.Error("an expired command sent something")
	}

	// Expiry is judged when the command runs: valid now, expired a minute later.
	note := sendEnv(t, w, "CMD2", Send{Kind: "note", Text: "n", ExpiresAt: FormatTS(w.clock.Now().Add(time.Minute))})
	w.clock.Advance(time.Minute) // exactly at expires_at counts as expired
	if res := e.Handle(context.Background(), note); res.Error != ErrExpired {
		t.Errorf("at the expiry instant: %+v", res)
	}
	if len(w.topics.Posts()) != 0 {
		t.Error("an expired note was posted")
	}
}

func TestSentGuardExpires(t *testing.T) {
	c := newFakeClock()
	g := NewSentGuard(c)
	g.Mark("A")
	if !g.Has("A") || g.Has("B") {
		t.Fatal("guard membership is wrong")
	}
	c.Advance(2 * time.Hour)
	if g.Has("A") {
		t.Error("guard entry outlived its TTL")
	}
}

/* ------------------------------------------------------------ copyables -- */

func copyablesOf(cs ...Copyable) *Copyables { c := Copyables(cs); return &c }

// Order: the main message first, then each copyable in list order, each as its
// own message holding only the value.
func TestCopyablesAreSeparateValueOnlyMessagesAfterTheMain(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	res := e.Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Text: "Please pay to this account",
		Copyables: copyablesOf(
			Copyable{Label: "Account number", Value: "5141 2345 6789"},
			Copyable{Label: "Wallet", Value: "TXyz123abc"},
			Copyable{Label: "Alias", Value: "clarus.pagos"},
		),
	}))
	if !res.OK || res.HubMsgID != "WA001" {
		t.Fatalf("result = %+v (hub_msg_id must be the MAIN message)", res)
	}
	calls := w.wa.Calls()
	if len(calls) != 4 {
		t.Fatalf("%d WhatsApp sends, want 4: %+v", len(calls), calls)
	}
	want := []string{"Please pay to this account", "5141 2345 6789", "TXyz123abc", "clarus.pagos"}
	for i, c := range calls {
		if c.Kind != "text" || c.Text != want[i] {
			t.Errorf("send %d = %+v, want text %q", i, c, want[i])
		}
	}
	// Only the main message quotes; values carry no label, backticks or quote.
	for _, c := range calls[1:] {
		if c.Quote != nil {
			t.Errorf("copyable %q carries a quote", c.Text)
		}
	}
	// Everything the Hub sent is the Agent's own and must never echo back as staff speech.
	for _, id := range []string{"WA001", "WA002", "WA003", "WA004"} {
		if !e.Guard.Has(id) {
			t.Errorf("%s is not remembered as the Agent's own", id)
		}
	}
}

func TestCopyablesWithMediaMain(t *testing.T) {
	w := newWorld(t)
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Text: "DuitNow",
		Media:     []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}},
		Copyables: copyablesOf(Copyable{Label: "Account number", Value: "5141 2345 6789"}),
	}))
	if !res.OK || res.HubMsgID != "WA001" {
		t.Fatalf("result = %+v", res)
	}
	calls := w.wa.Calls()
	if len(calls) != 2 || calls[0].Kind != "image" || calls[0].Text != "DuitNow" || calls[1].Kind != "text" || calls[1].Text != "5141 2345 6789" {
		t.Fatalf("calls = %+v", calls)
	}
}

// The topic gets ONE mirror post: the robot line, then label: value lines. The
// pair is recorded for the main message.
func TestCopyablesMirrorIsOnePostWithLabelValueLines(t *testing.T) {
	w := newWorld(t)
	w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Text: "Please pay to this account",
		Copyables: copyablesOf(
			Copyable{Label: "Account number", Value: "5141 2345 6789"},
			Copyable{Label: "Alias", Value: "clarus.pagos"},
		),
	}))
	posts := w.topics.Posts()
	if len(posts) != 1 {
		t.Fatalf("%d topic posts, want exactly 1: %+v", len(posts), posts)
	}
	if want := "🤖 Please pay to this account\nAccount number: 5141 2345 6789\nAlias: clarus.pagos"; posts[0].Text != want {
		t.Errorf("mirror = %q, want %q", posts[0].Text, want)
	}
	if pairs := w.bridge.Pairs(); len(pairs) != 1 || pairs[0].WaMsgID != "WA001" {
		t.Errorf("pairs = %+v, want one for the main message WA001", pairs)
	}

	// With a media main, the lines go on the media's caption.
	w2 := newWorld(t)
	w2.executor().Handle(context.Background(), sendEnv(t, w2, "CMD1", Send{
		Kind: "reply", Text: "DuitNow",
		Media:     []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}},
		Copyables: copyablesOf(Copyable{Label: "Account number", Value: "5141 2345 6789"}),
	}))
	posts = w2.topics.Posts()
	if len(posts) != 1 || posts[0].Media == nil || posts[0].Text != "🤖 DuitNow\nAccount number: 5141 2345 6789" {
		t.Fatalf("media mirror = %+v", posts)
	}
}

// Main send fails: customer_unreachable, no copyable sent, nothing mirrored.
func TestCopyablesNothingSentWhenMainFails(t *testing.T) {
	w := newWorld(t)
	w.wa.fail = errors.New("unreachable")
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Text: "pay", Copyables: copyablesOf(Copyable{Label: "Account", Value: "123"}),
	}))
	if res.OK || res.Error != ErrCustomerUnreachable {
		t.Fatalf("result = %+v", res)
	}
	if len(w.wa.Calls()) != 0 || len(w.topics.Posts()) != 0 || len(w.bridge.Pairs()) != 0 {
		t.Error("something was sent, posted or recorded")
	}
}

// A copyable fails after the main one went out: still ok, and staff get a note
// naming the value that did not arrive.
func TestCopyableFailureAfterMainStillOKWithNote(t *testing.T) {
	w := newWorld(t)
	w.wa.failIf = func(c waCall) error {
		if c.Text == "TXyz123abc" {
			return errors.New("send failed")
		}
		return nil
	}
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Text: "pay",
		Copyables: copyablesOf(
			Copyable{Label: "Account number", Value: "5141 2345 6789"},
			Copyable{Label: "Wallet", Value: "TXyz123abc"},
			Copyable{Label: "Alias", Value: "clarus.pagos"},
		),
	}))
	if !res.OK || res.Error != "" || res.HubMsgID != "WA001" {
		t.Fatalf("result = %+v, want ok with the main message id", res)
	}
	// The other copyables still went out, in order, around the failed one.
	var sent []string
	for _, c := range w.wa.Calls() {
		sent = append(sent, c.Text)
	}
	if len(sent) != 3 || sent[0] != "pay" || sent[1] != "5141 2345 6789" || sent[2] != "clarus.pagos" {
		t.Errorf("sent = %q", sent)
	}
	posts := w.topics.Posts()
	if len(posts) != 2 {
		t.Fatalf("%d topic posts, want the mirror plus one note: %+v", len(posts), posts)
	}
	if !strings.HasPrefix(posts[1].Text, "ℹ️ ") || !strings.Contains(posts[1].Text, "Wallet: TXyz123abc") ||
		strings.Contains(posts[1].Text, "Account number") || strings.Contains(posts[1].Text, "Alias") {
		t.Errorf("note = %q, want an ℹ️ note naming only the failed Wallet value", posts[1].Text)
	}
}

func TestCopyablesValidation(t *testing.T) {
	long := strings.Repeat("x", MaxCopyableValue+1)
	six := make(Copyables, 6)
	for i := range six {
		six[i] = Copyable{Label: "l", Value: "v"}
	}
	for name, c := range map[string]*Copyables{
		"six items":   &six,
		"long value":  copyablesOf(Copyable{Label: "l", Value: long}),
		"empty value": copyablesOf(Copyable{Label: "l", Value: ""}),
		"empty label": copyablesOf(Copyable{Label: "", Value: "v"}),
	} {
		s := Send{Conversation: "wa:1@s.whatsapp.net", Kind: "reply", Media: []Media{}, Copyables: c, ExpiresAt: "2026-10-01T09:40:12.345Z"}
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := Send{Conversation: "wa:1@s.whatsapp.net", Kind: "reply", Media: []Media{}, ExpiresAt: "2026-10-01T09:40:12.345Z",
		Copyables: copyablesOf(Copyable{Label: "l", Value: strings.Repeat("é", MaxCopyableValue)})}
	if err := ok.Validate(); err != nil {
		t.Errorf("256 characters (multi-byte) rejected: %v", err)
	}
}
