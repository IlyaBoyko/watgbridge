package lzcustomer_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"watgbridge/agentlink"
	. "watgbridge/lzcustomer"
	"watgbridge/lzcustomer/lzfake"
)

/* -------------------------------------------------------------- splitting -- */

func TestSplitTextBoundaryPreference(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		limit int
		want  []string
	}{
		{"fits", "short", 10, []string{"short"}},
		{"exactly the limit", "0123456789", 10, []string{"0123456789"}},
		{"paragraph before line, sentence and space", "aa bb. cc\ndd\n\nee ff gg", 14, []string{"aa bb. cc\ndd", "ee ff gg"}},
		{"line before sentence and space", "aa bb. cc\ndd ee ff", 12, []string{"aa bb. cc", "dd ee ff"}},
		{"sentence before space", "aa bb. cc dd ee", 12, []string{"aa bb.", "cc dd ee"}},
		{"question mark and bang end a sentence", "Really? Yes! ok ok ok ok", 14, []string{"Really? Yes!", "ok ok ok ok"}},
		{"a dot inside a word is no sentence end", "v1.2 aaaa bbbb cccc", 12, []string{"v1.2 aaaa", "bbbb cccc"}},
		{"space", "aaaa bbbb cccc dddd", 11, []string{"aaaa bbbb", "cccc dddd"}},
		{"hard cut", "abcdefghijklmnopqrstuvwxy", 10, []string{"abcdefghij", "klmnopqrst", "uvwxy"}},
		{"break right after a full part", "0123456789 abc", 10, []string{"0123456789", "abc"}},
		{"CRLF is a line break", "aaaa\r\n\r\nbbbb cccc", 12, []string{"aaaa", "bbbb cccc"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SplitText(c.text, c.limit)
			if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", c.want) {
				t.Errorf("SplitText(%q, %d) = %q, want %q", c.text, c.limit, got, c.want)
			}
		})
	}
}

// Parts are measured in runes, so multi-byte text and emoji are not cut early,
// and a cut never lands inside a character.
func TestSplitTextCountsRunes(t *testing.T) {
	// 5 characters of 2 bytes each fit a limit of 5.
	if got := SplitText("ñandú", 5); len(got) != 1 {
		t.Errorf("5 runes in a limit of 5 split into %q", got)
	}
	got := SplitText("😀😀😀😀😀😀😀", 3)
	if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", []string{"😀😀😀", "😀😀😀", "😀"}) {
		t.Errorf("emoji split = %q", got)
	}
	for _, p := range SplitText(strings.Repeat("é", 25), 10) {
		if r := []rune(p); len(r) > 10 {
			t.Errorf("part of %d runes over the limit of 10", len(r))
		}
	}
}

func TestSplitTextNeverMakesAnEmptyPart(t *testing.T) {
	for _, text := range []string{
		"", "   ", "\n\n", "aaa\n\n\n\n   \nbbb", "aaa      bbb", "\n\n\naaa bbb ccc\n\n\n", "aaaa. . . . bbbb",
		strings.Repeat("a b\n\n", 50), strings.Repeat(" ", 30) + "x" + strings.Repeat(" ", 30) + "y",
	} {
		for limit := 1; limit <= 12; limit++ {
			parts := SplitText(text, limit)
			total := 0
			for _, p := range parts {
				if p == "" || p != strings.TrimSpace(p) {
					t.Fatalf("SplitText(%q, %d) has the part %q", text, limit, p)
				}
				if n := len([]rune(p)); n > limit {
					t.Fatalf("SplitText(%q, %d) has a part of %d runes", text, limit, n)
				}
				total += len(strings.ReplaceAll(strings.ReplaceAll(p, " ", ""), "\n", ""))
			}
			if want := len(strings.ReplaceAll(strings.ReplaceAll(text, " ", ""), "\n", "")); total != want {
				t.Fatalf("SplitText(%q, %d) lost text: %q", text, limit, parts)
			}
		}
	}
	if got := SplitText("", 10); got != nil {
		t.Errorf("empty text = %q", got)
	}
}

func TestSplitTextDefaultsTheLimit(t *testing.T) {
	text := strings.Repeat("a", 2500)
	if got := SplitText(text, 0); len(got) != 3 || len([]rune(got[0])) != 1000 {
		t.Errorf("no limit gave %d parts", len(got))
	}
}

/* ----------------------------------------------------------- the channel -- */

const sess = "S1"

func sendWorld(t *testing.T, mod ...func(*worldOpts)) *world {
	t.Helper()
	w := newWorld(t, mod...)
	w.lz.AddSession(sess, "", "Aisyah")
	return w
}

func withTextLimit(n int) func(*worldOpts) {
	return func(o *worldOpts) { o.textLimit = n }
}

func (w *world) threadOf() int64 {
	w.t.Helper()
	id, _, _ := w.threads.Find(agentlink.LzChatKey(sess))
	return id
}

func (w *world) sentRows() map[string]string {
	w.t.Helper()
	var rows []SentRow
	if err := w.db.Order("id").Find(&rows).Error; err != nil {
		w.t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		if r.SessionID != sess {
			w.t.Errorf("sent row of session %q", r.SessionID)
		}
		out[r.MessageID] = r.Origin
	}
	return out
}

func lzTexts(sent []lzfake.Sent) []string {
	var out []string
	for _, s := range sent {
		out = append(out, s.Text)
	}
	return out
}

func TestSendTextSendsPartsInOrderAndRecordsEachWithItsOrigin(t *testing.T) {
	w := sendWorld(t, withTextLimit(12))
	w.start()

	ids, err := w.svc.SendText(context.Background(), sess, "first part.\n\nsecond one.\n\nthird", "agent")
	mustNoErr(t, err)
	if want := []string{"first part.", "second one.", "third"}; fmt.Sprint(lzTexts(w.lz.Sent())) != fmt.Sprint(want) {
		t.Errorf("sent = %q, want %q", lzTexts(w.lz.Sent()), want)
	}
	if len(ids) != 3 {
		t.Fatalf("ids = %v", ids)
	}
	for i, s := range w.lz.Sent() {
		if s.MessageID != ids[i] || s.TemplateID != "1" || s.SessionID != sess {
			t.Errorf("part %d: lazada got %+v, returned id %s", i, s, ids[i])
		}
	}
	rows := w.sentRows()
	if len(rows) != 3 {
		t.Fatalf("lazada_sent = %v", rows)
	}
	for _, id := range ids {
		if rows[id] != OriginAgent {
			t.Errorf("origin of %s = %q", id, rows[id])
		}
	}

	ids2, err := w.svc.SendText(context.Background(), sess, "from staff", "topic")
	mustNoErr(t, err)
	if w.sentRows()[ids2[0]] != OriginTopic {
		t.Errorf("origin = %q, want topic", w.sentRows()[ids2[0]])
	}
}

func TestSendTextUsesTheConfiguredLimit(t *testing.T) {
	text := strings.Repeat("word ", 30) // 149 runes trimmed
	for limit, parts := range map[int]int{0: 1, 1000: 1, 80: 2, 40: 4} {
		w := sendWorld(t, withTextLimit(limit))
		w.start()
		ids, err := w.svc.SendText(context.Background(), sess, text, "agent")
		mustNoErr(t, err)
		if len(ids) != parts {
			t.Errorf("limit %d: %d parts, want %d", limit, len(ids), parts)
		}
	}
}

func TestSendTextMarksTheSessionReadAfterTheLastPart(t *testing.T) {
	w := sendWorld(t, withTextLimit(10))
	w.start()
	w.buyer(sess, "hello?")

	ids, err := w.svc.SendText(context.Background(), sess, "aaaa bbbb cccc dddd", "agent")
	mustNoErr(t, err)
	reads := w.lz.Reads()
	if len(reads) != 1 || reads[0].SessionID != sess || reads[0].LastReadMessageID != ids[len(ids)-1] {
		t.Errorf("reads = %+v, want one at %s", reads, ids[len(ids)-1])
	}
	// Both calls were made in the right order: every send before the read.
	var order []string
	for _, r := range w.lz.Requests() {
		if r.Path == "/im/message/send" || r.Path == "/im/session/read" {
			order = append(order, r.Path)
		}
	}
	if want := "/im/message/send /im/message/send /im/session/read"; fmt.Sprint(strings.Join(order, " ")) != want {
		t.Errorf("order = %v", order)
	}
}

func TestAReadFailureDoesNotFailTheSend(t *testing.T) {
	w := sendWorld(t)
	w.start()
	w.lz.FailNext("/im/session/read", "ApiCallLimit")
	ids, err := w.svc.SendText(context.Background(), sess, "hi", "agent")
	if err != nil || len(ids) != 1 {
		t.Fatalf("ids %v, err %v", ids, err)
	}
	if !contains(w.logged("lazada: could not mark the session read"), "read") {
		t.Errorf("the read failure was not logged: %v", w.logs.All())
	}
	for _, e := range w.logs.All() {
		if e.Message == "lazada: could not mark the session read" && e.Level.String() != "debug" {
			t.Errorf("read failure logged at %s", e.Level)
		}
	}
}

// A6: Lazada's filter judges a message after the send call returned, so the
// session is fetched once about five seconds later and the warning appears.
func TestSendTextPollsTheSessionAboutFiveSecondsLater(t *testing.T) {
	w := sendWorld(t)
	w.start()
	w.buyer(sess, "hello")
	w.run()
	w.waitRounds(1)
	listCalls := func() int { return w.lz.Calls("/im/message/list") }
	before := listCalls()

	w.lz.BlockNextSend("contains a phone number")
	_, err := w.svc.SendText(context.Background(), sess, "call 0123456789", "agent")
	mustNoErr(t, err)
	waitFor(t, "the delayed poll to be scheduled", func() bool {
		for _, d := range w.clock.Afters() {
			if d == 5*time.Second {
				return true
			}
		}
		return false
	})
	w.clock.Advance(4 * time.Second)
	time.Sleep(30 * time.Millisecond)
	if listCalls() != before {
		t.Fatal("the session was fetched before the five seconds were up")
	}

	w.clock.Advance(time.Second)
	w.waitRounds(2)
	if listCalls() == before {
		t.Error("the session was not fetched after five seconds")
	}
	// H13's path posts the verdict; the message is ours, so no staff event.
	thread := w.threadOf()
	if !contains(w.topics.texts(thread), "⚠️ Lazada blocked this message: contains a phone number") {
		t.Errorf("topic = %q", w.topics.texts(thread))
	}
	if n := len(w.events.Staffs()); n != 0 {
		t.Errorf("%d staff events for the Hub's own message", n)
	}
}

func TestNoPollAfterSendWhenSwitchedOff(t *testing.T) {
	w := sendWorld(t, func(o *worldOpts) { o.pollAfterSend = -1 })
	w.start()
	_, err := w.svc.SendText(context.Background(), sess, "hi", "agent")
	mustNoErr(t, err)
	if len(w.clock.Afters()) != 0 {
		t.Errorf("waits = %v", w.clock.Afters())
	}
}

// A later fetch that sees our own message with a verdict warns once and never
// reports it as staff speech.
func TestAFetchThatSeesOurBlockedMessagePostsTheWarning(t *testing.T) {
	w := sendWorld(t)
	w.start()
	w.buyer(sess, "hello")
	mustNoErr(t, w.scan())
	thread := w.threadOf()

	w.lz.BlockNextSend("links are not allowed")
	ids, err := w.svc.SendText(context.Background(), sess, "see https://example.test", "agent")
	mustNoErr(t, err)
	mustNoErr(t, w.fetchOne(sess))
	mustNoErr(t, w.fetchOne(sess)) // and again: nothing twice

	warnings := 0
	for _, txt := range w.topics.texts(thread) {
		if strings.Contains(txt, "⚠️ Lazada blocked this message: links are not allowed") {
			warnings++
		}
		if strings.Contains(txt, "see https://example.test") {
			t.Errorf("our own message was mirrored by the fetcher: %q", txt)
		}
	}
	if warnings != 1 {
		t.Errorf("%d warnings in %q", warnings, w.topics.texts(thread))
	}
	if n := len(w.events.Staffs()); n != 0 {
		t.Errorf("staff events for %s: %+v", ids[0], w.events.Staffs())
	}
}

// failingSendAPI lets the first n sends through and then fails the rest.
type failingSendAPI struct {
	API
	mu   sync.Mutex
	pass int
	err  error
}

func (f *failingSendAPI) SendText(ctx context.Context, session, text string) (string, error) {
	f.mu.Lock()
	ok := f.pass > 0
	f.pass--
	f.mu.Unlock()
	if !ok {
		return "", f.err
	}
	return f.API.SendText(ctx, session, text)
}

func TestSendTextPartialFailureKeepsWhatWentOut(t *testing.T) {
	var failing *failingSendAPI
	w := sendWorld(t, withTextLimit(10), func(o *worldOpts) {
		o.api = func(a API) API {
			failing = &failingSendAPI{API: a, pass: 2, err: errors.New("lazada: Timeout: gateway")}
			return failing
		}
	})
	w.start()
	w.buyer(sess, "hello")

	ids, err := w.svc.SendText(context.Background(), sess, "aaaa bbbb\n\ncccc dddd\n\neeee ffff\n\ngggg", "agent")
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want the two parts that went out", ids)
	}
	var partial *agentlink.LazadaPartialError
	if !errors.As(err, &partial) || partial.Sent != 2 || partial.Total != 4 || partial.SentText != "aaaa bbbb\n\ncccc dddd" ||
		!strings.Contains(partial.Err.Error(), "gateway") {
		t.Fatalf("err = %#v", err)
	}
	if rows := w.sentRows(); len(rows) != 2 || rows[ids[0]] == "" || rows[ids[1]] == "" {
		t.Errorf("lazada_sent = %v, want both sent parts", rows)
	}
	if n := len(w.lz.Reads()); n != 0 {
		t.Errorf("a half-sent reply marked the session read (%d reads)", n)
	}
	// The sent parts are ours: a fetch reports nothing about them.
	mustNoErr(t, w.scan())
	if n := len(w.events.Staffs()); n != 0 {
		t.Errorf("staff events: %+v", w.events.Staffs())
	}
}

func TestSendTextFirstPartFailureReturnsNoIDs(t *testing.T) {
	w := sendWorld(t)
	w.start()
	w.lz.FailNextHTTP("/im/message/send", 500)
	ids, err := w.svc.SendText(context.Background(), sess, "hi", "agent")
	var partial *agentlink.LazadaPartialError
	if len(ids) != 0 || err == nil || errors.As(err, &partial) {
		t.Errorf("ids %v, err %v", ids, err)
	}
	if len(w.sentRows()) != 0 {
		t.Error("a failed send was recorded")
	}
}

func TestSendTextMapsErrors(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(w *world)
		limited  bool
		blocked  bool
		wantText string
	}{
		{"rate limited", func(w *world) { w.lz.FailNext("/im/message/send", "ApiCallLimit") }, true, false, "ApiCallLimit"},
		{"http 429", func(w *world) { w.lz.FailNextHTTP("/im/message/send", 429) }, true, false, ""},
		{"token refused", func(w *world) { w.lz.FailNext("/im/message/send", "IllegalAccessToken") }, false, true, "IllegalAccessToken"},
		{"authorization revoked", func(w *world) { w.lz.RevokeTokens() }, false, true, ""},
		{"anything else", func(w *world) { w.lz.FailNext("/im/message/send", "InvalidParameter") }, false, false, "InvalidParameter"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := sendWorld(t)
			w.start()
			c.setup(w)
			ids, err := w.svc.SendText(context.Background(), sess, "hi", "agent")
			if len(ids) != 0 || err == nil {
				t.Fatalf("ids %v, err %v", ids, err)
			}
			if got := errors.Is(err, agentlink.ErrCustomerRateLimited); got != c.limited {
				t.Errorf("rate limited = %v for %v", got, err)
			}
			if got := errors.Is(err, agentlink.ErrCustomerBlocked); got != c.blocked {
				t.Errorf("blocked = %v for %v", got, err)
			}
			if !strings.Contains(err.Error(), c.wantText) {
				t.Errorf("err = %q, want it to say %q", err, c.wantText)
			}
		})
	}
}

func TestSendTextWithoutAStoredAuthorizationIsBlocked(t *testing.T) {
	w := sendWorld(t, func(o *worldOpts) { o.unauth = true })
	w.start()
	_, err := w.svc.SendText(context.Background(), sess, "hi", "agent")
	if !errors.Is(err, agentlink.ErrCustomerBlocked) {
		t.Errorf("err = %v", err)
	}
}

func TestSendTextAfterStopIsBlocked(t *testing.T) {
	w := sendWorld(t)
	w.start()
	w.run()
	w.svc.Stop()
	ids, err := w.svc.SendText(context.Background(), sess, "hi", "agent")
	if len(ids) != 0 || !errors.Is(err, agentlink.ErrCustomerBlocked) {
		t.Errorf("ids %v, err %v", ids, err)
	}
	if len(w.lz.Sent()) != 0 {
		t.Error("a stopped channel sent")
	}
}

func TestSendTextRefusesEmptyText(t *testing.T) {
	w := sendWorld(t)
	w.start()
	if ids, err := w.svc.SendText(context.Background(), sess, " \n ", "agent"); err == nil || len(ids) != 0 {
		t.Errorf("ids %v, err %v", ids, err)
	}
	if w.lz.Calls("/im/message/send") != 0 {
		t.Error("an empty message was sent")
	}
}

/* ------------------------------------------------------ send versus fetch -- */

// sendGateAPI stops a send after Lazada has the message and before SendText can
// return, which is exactly the window in which a fetch would see a seller
// message that is not in lazada_sent yet.
type sendGateAPI struct {
	API
	onLazada chan string   // the id, once Lazada has the message
	release  chan struct{} // closed by the test to let the send go on
}

func (g *sendGateAPI) SendText(ctx context.Context, session, text string) (string, error) {
	id, err := g.API.SendText(ctx, session, text)
	if err == nil {
		g.onLazada <- id
		<-g.release
	}
	return id, err
}

// raceWorld has a session with a buyer message handled, and a send that is
// parked in the dangerous window.
func raceWorld(t *testing.T) (w *world, g *sendGateAPI, sendDone chan error) {
	t.Helper()
	g = &sendGateAPI{onLazada: make(chan string, 1), release: make(chan struct{})}
	w = sendWorld(t, func(o *worldOpts) {
		o.api = func(a API) API { g.API = a; return g }
	})
	w.start()
	w.buyer(sess, "is it in stock?")
	mustNoErr(t, w.scan())
	if n := len(w.events.Customers()); n != 1 {
		t.Fatalf("setup: %d customer events", n)
	}

	sendDone = make(chan error, 1)
	go func() {
		_, err := w.svc.SendText(context.Background(), sess, "Yes, in stock.", "agent")
		sendDone <- err
	}()
	select {
	case <-g.onLazada:
	case <-time.After(5 * time.Second):
		t.Fatal("the send never reached Lazada")
	}
	return w, g, sendDone
}

// The important one. While a send is in flight, Lazada already has the message
// but lazada_sent does not. A fetch that ran now would report the Hub's own
// reply as staff speaking from Seller Center, and the Agent would take it for a
// human takeover. The fetch must wait for the send to finish.
func TestAFetchDuringASendWaitsAndNeverReportsTheSentMessage(t *testing.T) {
	w, g, sendDone := raceWorld(t)

	fetchDone := make(chan error, 1)
	go func() { fetchDone <- w.fetchOne(sess) }()

	select {
	case err := <-fetchDone:
		t.Fatalf("a fetch ran while a send was in flight (err %v); staff events: %+v", err, w.events.Staffs())
	case <-time.After(150 * time.Millisecond):
	}
	if n := len(w.events.Staffs()); n != 0 {
		t.Fatalf("staff events during the send: %+v", w.events.Staffs())
	}

	close(g.release)
	mustNoErr(t, <-sendDone)
	mustNoErr(t, <-fetchDone)

	sent := w.lz.Sent()
	if len(sent) != 1 || w.sentRows()[sent[0].MessageID] != OriginAgent {
		t.Fatalf("sent %+v, lazada_sent %v", sent, w.sentRows())
	}
	if n := len(w.events.Staffs()); n != 0 {
		t.Errorf("the Hub's own reply came back as staff.message: %+v", w.events.Staffs())
	}
	if cur, ok, _ := w.svc.Cursor(sess); !ok || cur.LastMessageID != sent[0].MessageID {
		t.Errorf("the fetch that waited did not move past the sent message: %+v", cur)
	}
}

// The same through the worker: a poke (a push or a timer) during a send.
func TestAPokeDuringASendWaitsAndNeverReportsTheSentMessage(t *testing.T) {
	w, g, sendDone := raceWorld(t)
	w.run() // its first fetch is queued behind the send
	w.svc.Poll(sess)

	time.Sleep(150 * time.Millisecond)
	if n := w.svc.Rounds(); n != 0 {
		t.Fatalf("%d fetch rounds finished while a send was in flight", n)
	}
	if n := len(w.events.Staffs()); n != 0 {
		t.Fatalf("staff events during the send: %+v", w.events.Staffs())
	}

	close(g.release)
	mustNoErr(t, <-sendDone)
	sent := w.lz.Sent()[0].MessageID
	waitFor(t, "the fetch to move past the sent message", func() bool {
		cur, ok, _ := w.svc.Cursor(sess)
		return ok && cur.LastMessageID == sent
	})
	if n := len(w.events.Staffs()); n != 0 {
		t.Errorf("the Hub's own reply came back as staff.message: %+v", w.events.Staffs())
	}
}

// Seller Center speech that is not ours is still reported after our send.
func TestStaffMessagesStillComeThroughAfterASend(t *testing.T) {
	w := sendWorld(t)
	w.start()
	w.buyer(sess, "hello")
	mustNoErr(t, w.scan())
	_, err := w.svc.SendText(context.Background(), sess, "Hi!", "agent")
	mustNoErr(t, err)
	human := w.seller(sess, "I'll take it from here", lzfake.SellerOpts{})
	mustNoErr(t, w.scan())
	evs := w.events.Staffs()
	if len(evs) != 1 || evs[0].HubMsgID != human || evs[0].Source != "phone" {
		t.Errorf("staff events = %+v", evs)
	}
}
