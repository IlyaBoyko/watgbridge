package lzcustomer_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	. "watgbridge/lzcustomer"

	"watgbridge/lzcustomer/lzfake"
)

// gateAPI wraps an API so a test can hold a call open and count calls.
type gateAPI struct {
	API
	mu          sync.Mutex
	sessionList int
	messageList int
	// hold, when non-nil, blocks the first SessionList until it is closed;
	// entered is closed when that call has arrived.
	hold    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (g *gateAPI) SessionList(ctx context.Context, start int64, size int, last string) (SessionPage, error) {
	g.mu.Lock()
	g.sessionList++
	n := g.sessionList
	g.mu.Unlock()
	if n == 1 && g.hold != nil {
		g.once.Do(func() { close(g.entered) })
		<-g.hold
	}
	return g.API.SessionList(ctx, start, size, last)
}

func (g *gateAPI) MessageList(ctx context.Context, id string, start int64, size int, last string) (MessagePage, error) {
	g.mu.Lock()
	g.messageList++
	g.mu.Unlock()
	return g.API.MessageList(ctx, id, start, size, last)
}

func (g *gateAPI) counts() (sessions, messages int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sessionList, g.messageList
}

func withGate(g *gateAPI) func(*worldOpts) {
	return func(o *worldOpts) {
		o.api = func(a API) API { g.API = a; return g }
	}
}

// The first run records when it started and never emits older messages (A8).
func TestFirstRunSetsStartedAtAndEmitsNothingOld(t *testing.T) {
	w := newWorld(t)
	w.buyer("s1", "old question")
	w.buyer("s2", "another old one")
	w.clock.Advance(time.Minute)
	startedAt := w.clock.Now().UnixMilli()

	w.run()
	w.waitRounds(1)

	meta, _, err := w.svc.Meta(0)
	mustNoErr(t, err)
	if meta.StartedAt != startedAt {
		t.Errorf("started_at = %d, want %d", meta.StartedAt, startedAt)
	}
	if got := w.j.all(); len(got) != 0 {
		t.Errorf("something was posted or emitted for old messages: %v", got)
	}

	// A later start does not move it.
	w.svc.Stop()
	w2 := w.svc
	w2.Run(context.Background())
	t.Cleanup(w2.Stop)
	meta, _, _ = w.svc.Meta(1)
	if meta.StartedAt != startedAt {
		t.Errorf("a second run moved started_at to %d", meta.StartedAt)
	}

	// A message after the start is new.
	w.clock.Advance(time.Second)
	w.buyer("s1", "hello again")
	w.svc.Poll("")
	waitFor(t, "the new message", func() bool { return len(w.events.Customers()) == 1 })
}

// A8: a new topic opens with the earlier messages as one note, then the message
// itself, then the event; the note emits nothing.
func TestNewBuyerMessageCreatesTopicNoteMessageThenEvent(t *testing.T) {
	w := newWorld(t)
	w.lz.AddSession("s1", "b1", "Aiman")
	w.buyer("s1", "do you ship to Penang?")
	w.seller("s1", "Yes we do", lzfake.SellerOpts{})
	w.clock.Advance(time.Minute)
	w.start()
	id := w.buyer("s1", "How much is BPC-157?")

	mustNoErr(t, w.scan())

	want := []string{
		"create-topic:Lazada · Aiman",
		"post:1250:text:ℹ️ Earlier in this chat:|Buyer: do you ship to Penang?|Shop: Yes we do",
		"post:1250:text:How much is BPC-157?",
		"customer:" + id,
	}
	if got := w.j.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal =\n%q\nwant\n%q", got, want)
	}
	ev := w.events.Customers()[0]
	if ev.ChatKey != "lz:s1" || ev.HubMsgID != id || ev.ContactName != "Aiman" || ev.Text != "How much is BPC-157?" ||
		ev.TopicID != 1250 || ev.Username != "" || len(ev.Media) != 0 || ev.ReplyTo != "" {
		t.Errorf("customer event = %+v", ev)
	}
	if id, found, _ := w.threads.Find("lz:s1"); !found || id != 1250 {
		t.Errorf("the topic is stored as %d (found %v)", id, found)
	}

	// The topic exists now: another message makes no topic and no note.
	w.j.lines = nil
	id2 := w.buyer("s1", "and the 10mg?")
	mustNoErr(t, w.scan())
	if got := w.j.all(); !reflect.DeepEqual(got, []string{"post:1250:text:and the 10mg?", "customer:" + id2}) {
		t.Errorf("journal = %q", got)
	}
}

// A session with nothing before its first message gets no note.
func TestNoContextNoteWhenThereIsNothingEarlier(t *testing.T) {
	w := newWorld(t)
	w.start()
	id := w.buyer("s1", "hi")
	mustNoErr(t, w.scan())
	want := []string{"create-topic:Lazada · Buyer s1", "post:1250:text:hi", "customer:" + id}
	if got := w.j.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal = %q", got)
	}
}

// The context note holds at most 10 messages, the newest of those before the
// first one handled.
func TestContextNoteIsCappedAtTen(t *testing.T) {
	w := newWorld(t)
	for i := 1; i <= 14; i++ {
		w.buyer("s1", fmt.Sprintf("old %d", i))
	}
	w.clock.Advance(time.Minute)
	w.start()
	w.buyer("s1", "new")
	mustNoErr(t, w.scan())
	note := w.topics.Posts()[0].Text
	if !strings.HasPrefix(note, "Earlier in this chat:\nBuyer: old 5\n") || !strings.HasSuffix(note, "Buyer: old 14") ||
		strings.Count(note, "\n") != 10 {
		t.Errorf("note = %q", note)
	}
}

// Several messages come out in the order they were written.
func TestThreeNewMessagesInOrderOldestFirst(t *testing.T) {
	w := newWorld(t)
	w.start()
	a := w.buyer("s1", "one")
	b := w.buyer("s1", "two")
	c := w.buyer("s1", "three")
	mustNoErr(t, w.scan())

	var got []string
	for _, e := range w.events.Customers() {
		got = append(got, e.HubMsgID)
	}
	if !reflect.DeepEqual(got, []string{a, b, c}) {
		t.Errorf("events = %v, want %v", got, []string{a, b, c})
	}
	if texts := w.topics.texts(1250); !reflect.DeepEqual(texts, []string{"one", "two", "three"}) {
		t.Errorf("topic = %q", texts)
	}
	cur, ok, _ := w.svc.Cursor("s1")
	if !ok || cur.LastMessageID != c || cur.Title != "Buyer s1" {
		t.Errorf("cursor = %+v", cur)
	}
}

// Both lists page: 45 messages in one session come over three pages, and 45
// sessions over three more, with nothing lost or doubled.
func TestPagingAcrossMessageAndSessionPages(t *testing.T) {
	w := newWorld(t)
	w.start()
	var want []string
	for i := 0; i < 45; i++ {
		want = append(want, w.buyer("big", fmt.Sprintf("m%d", i)))
	}
	mustNoErr(t, w.scan())
	var got []string
	for _, e := range w.events.Customers() {
		got = append(got, e.HubMsgID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("message pages: got %d events, want %d in order", len(got), len(want))
	}

	w2 := newWorld(t)
	w2.start()
	seen := map[string]bool{}
	for i := 0; i < 45; i++ {
		seen[w2.buyer(fmt.Sprintf("s%02d", i), "hi")] = true
	}
	mustNoErr(t, w2.scan())
	evs := w2.events.Customers()
	if len(evs) != 45 {
		t.Fatalf("session pages: %d events, want 45", len(evs))
	}
	for _, e := range evs {
		if !seen[e.HubMsgID] {
			t.Errorf("unexpected event %q", e.HubMsgID)
		}
		delete(seen, e.HubMsgID)
	}
	if calls := w2.lz.Calls("/im/session/list"); calls != 3 {
		t.Errorf("%d session list calls, want 3 pages", calls)
	}
}

// A failed event write stops the batch at that message; the retry carries on
// from it, with no duplicate event and no second topic post.
func TestCrashMidBatchResumesWithoutDuplicates(t *testing.T) {
	w := newWorld(t)
	w.start()
	a := w.buyer("s1", "one")
	b := w.buyer("s1", "two")
	c := w.buyer("s1", "three")
	w.events.failOn[b] = true

	if err := w.scan(); err == nil {
		t.Fatal("the failed event write was not reported")
	}
	if got := w.events.Customers(); len(got) != 1 || got[0].HubMsgID != a {
		t.Fatalf("after the crash: events = %+v", got)
	}
	cur, _, _ := w.svc.Cursor("s1")
	if cur.LastMessageID != a {
		t.Errorf("cursor moved past the failed message: %+v", cur)
	}

	mustNoErr(t, w.scan())
	var ids []string
	for _, e := range w.events.Customers() {
		ids = append(ids, e.HubMsgID)
	}
	if !reflect.DeepEqual(ids, []string{a, b, c}) {
		t.Errorf("events = %v", ids)
	}
	if texts := w.topics.texts(1250); !reflect.DeepEqual(texts, []string{"one", "two", "three"}) {
		t.Errorf("topic = %q (message two must be posted once)", texts)
	}
}

// If the topic cannot be created nothing is lost: the messages stay in Lazada
// and the next fetch brings them.
func TestTopicCreationFailureLosesNothing(t *testing.T) {
	w := newWorld(t)
	w.start()
	a := w.buyer("s1", "one")
	w.topics.createErr = fmt.Errorf("telegram is down")
	if err := w.scan(); err == nil {
		t.Fatal("no error")
	}
	if len(w.events.Customers()) != 0 {
		t.Fatal("an event was emitted without a topic")
	}
	w.topics.createErr = nil
	mustNoErr(t, w.scan())
	if got := w.events.Customers(); len(got) != 1 || got[0].HubMsgID != a {
		t.Errorf("events = %+v", got)
	}
}

// Poll(session) fetches that session and no other, and does not scan.
func TestPollOneSessionFetchesOnlyThatSession(t *testing.T) {
	g := &gateAPI{}
	w := newWorld(t, withGate(g))
	w.start()
	w.buyer("s1", "a1")
	w.buyer("s2", "b1")
	mustNoErr(t, w.scan())
	w.run()
	w.waitRounds(1) // Run's own first round: a full scan, nothing new

	idA := w.buyer("s1", "a2")
	w.buyer("s2", "b2")
	slBefore, mlBefore := g.counts()
	nBefore := len(w.events.Customers())
	w.svc.Poll("s1")
	w.waitRounds(2)

	sl, ml := g.counts()
	if sl != slBefore || ml-mlBefore != 1 {
		t.Errorf("a poll of one session made %d session list and %d message list calls, want 0 and 1", sl-slBefore, ml-mlBefore)
	}
	evs := w.events.Customers()
	if len(evs) != nBefore+1 || evs[len(evs)-1].HubMsgID != idA {
		t.Errorf("events = %+v: only s1's new message should have come", evs)
	}
}

// A poll of a session nobody has seen asks the list once for its name.
func TestPollOfANewSessionLooksUpItsTitle(t *testing.T) {
	g := &gateAPI{}
	w := newWorld(t, withGate(g))
	w.start()
	w.lz.AddSession("s9", "b9", "Siti N.")
	w.buyer("s9", "hello")
	mustNoErr(t, w.fetchOne("s9"))
	if got := w.topics.Created(); !reflect.DeepEqual(got, []string{"Lazada · Siti N."}) {
		t.Errorf("topics = %q", got)
	}
	if ev := w.events.Customers(); len(ev) != 1 || ev[0].ContactName != "Siti N." {
		t.Errorf("events = %+v", ev)
	}
}

// A full scan reads sessions only down to the last scan's watermark, and
// fetches only those whose newest message differs from their cursor.
func TestFullScansStopAtTheWatermarkAndSkipUnchangedSessions(t *testing.T) {
	g := &gateAPI{}
	w := newWorld(t, withGate(g))
	w.start()
	w.buyer("s1", "x")
	w.buyer("s2", "y")
	w.buyer("s3", "z")
	mustNoErr(t, w.scan())
	meta, _, _ := w.svc.Meta(0)
	newest := int64(0)
	for _, s := range []string{"s1", "s2", "s3"} {
		c, _, _ := w.svc.Cursor(s)
		if c.LastSendTime > newest {
			newest = c.LastSendTime
		}
	}
	if meta.FullScanWatermark != newest {
		t.Errorf("watermark = %d, want the newest activity %d", meta.FullScanWatermark, newest)
	}

	_, mlBefore := g.counts()
	w.clock.Advance(time.Minute)
	idB := w.buyer("s2", "again")
	mustNoErr(t, w.scan())
	_, ml := g.counts()
	if ml-mlBefore != 1 {
		t.Errorf("%d message fetches for one changed session, want 1", ml-mlBefore)
	}
	if evs := w.events.Customers(); len(evs) != 4 || evs[3].HubMsgID != idB {
		t.Errorf("events = %+v", evs)
	}
	meta, _, _ = w.svc.Meta(0)
	if c, _, _ := w.svc.Cursor("s2"); meta.FullScanWatermark != c.LastSendTime {
		t.Errorf("watermark = %d, want %d", meta.FullScanWatermark, c.LastSendTime)
	}

	// A session whose activity is older than the watermark less a minute is
	// not read at all: only the page that holds it is fetched.
	w.clock.Advance(10 * time.Minute)
	w.buyer("s4", "newest")
	slBefore, _ := g.counts()
	mustNoErr(t, w.scan())
	sl, _ := g.counts()
	if sl-slBefore != 1 {
		t.Errorf("%d session list calls, want one page", sl-slBefore)
	}
}

// The safety ticker runs a full scan by itself.
func TestSafetyTickerScansEverySession(t *testing.T) {
	w := newWorld(t, func(o *worldOpts) { o.safety = 3 * time.Minute })
	w.run()
	w.waitRounds(1)
	w.clock.Advance(time.Second)
	id := w.buyer("s1", "ping")
	w.clock.Tick(t, 3*time.Minute)
	waitFor(t, "the event", func() bool { return len(w.events.Customers()) == 1 })
	if w.events.Customers()[0].HubMsgID != id {
		t.Errorf("events = %+v", w.events.Customers())
	}
}

// Five pokes during a fetch cost exactly one more round.
func TestPollsDuringAFetchCoalesceIntoOneMoreRound(t *testing.T) {
	g := &gateAPI{hold: make(chan struct{}), entered: make(chan struct{})}
	w := newWorld(t, withGate(g))
	w.start()
	w.svc.Run(context.Background())
	t.Cleanup(w.svc.Stop)
	<-g.entered // the first scan is under way
	for i := 0; i < 5; i++ {
		w.svc.Poll("")
	}
	close(g.hold)
	w.waitRounds(2)
	time.Sleep(30 * time.Millisecond)
	if n := w.svc.Rounds(); n != 2 {
		t.Errorf("%d rounds, want 2 (the running one and one more)", n)
	}
	if sl, _ := g.counts(); sl != 2 {
		t.Errorf("%d session scans, want 2", sl)
	}
}

// The active ticker fetches only while a chat has had a message in the last
// ten minutes.
func TestActiveTickerRunsOnlyWhileAChatIsRecent(t *testing.T) {
	g := &gateAPI{}
	w := newWorld(t, withGate(g), func(o *worldOpts) { o.active = 15 * time.Second })
	w.start()
	w.buyer("s1", "hello")
	mustNoErr(t, w.scan()) // the cursor now knows the chat's newest message
	w.run()
	w.waitRounds(1)
	base, _ := g.counts()

	w.clock.Advance(5 * time.Minute) // recent: under ten minutes
	w.clock.Tick(t, 15*time.Second)
	w.waitRounds(2)
	if sl, _ := g.counts(); sl != base+1 {
		t.Errorf("while recent: %d scans after the tick, want 1", sl-base)
	}

	w.clock.Advance(10 * time.Minute) // now stale
	w.clock.Tick(t, 15*time.Second)
	w.clock.Tick(t, 15*time.Second)
	// A marker poll proves the stale tick queued nothing before it.
	w.svc.Poll("")
	w.waitRounds(3)
	time.Sleep(30 * time.Millisecond)
	if n := w.svc.Rounds(); n != 3 {
		t.Errorf("%d rounds, want 3: the stale tick must not fetch", n)
	}
}

// ApiCallLimit backs off 30 s, doubling; triggers during it are remembered
// and run once afterwards.
func TestRateLimitBacksOffAndRemembersTriggers(t *testing.T) {
	w := newWorld(t)
	w.start()
	id := w.buyer("s1", "hi")
	w.lz.FailNext("/im/session/list", "ApiCallLimit")
	w.lz.FailNext("/im/session/list", "ApiCallLimit")

	w.run()
	w.waitRounds(1)
	waitFor(t, "the first backoff", func() bool { return len(w.clock.Afters()) == 1 })
	if got := w.clock.Afters(); got[0] != 30*time.Second {
		t.Fatalf("first backoff = %v, want 30s", got[0])
	}
	for i := 0; i < 3; i++ {
		w.svc.Poll("") // remembered, not run
	}
	time.Sleep(20 * time.Millisecond)
	if n := w.lz.Calls("/im/session/list"); n != 1 {
		t.Fatalf("%d list calls during the backoff, want 1", n)
	}

	w.clock.Advance(30 * time.Second)
	w.waitRounds(2) // fails again
	waitFor(t, "the second backoff", func() bool { return len(w.clock.Afters()) == 2 })
	if got := w.clock.Afters(); got[1] != 60*time.Second {
		t.Fatalf("second backoff = %v, want 60s", got[1])
	}
	if len(w.events.Customers()) != 0 {
		t.Fatal("an event came out of a failed round")
	}

	w.clock.Advance(60 * time.Second)
	waitFor(t, "the event", func() bool { return len(w.events.Customers()) == 1 })
	if w.events.Customers()[0].HubMsgID != id {
		t.Errorf("events = %+v", w.events.Customers())
	}
	// Success resets the backoff.
	w.lz.FailNext("/im/session/list", "ApiCallLimit")
	w.svc.Poll("")
	waitFor(t, "a backoff after the reset", func() bool { return len(w.clock.Afters()) == 3 })
	if got := w.clock.Afters(); got[2] != 30*time.Second {
		t.Errorf("backoff after a success = %v, want 30s again", got[2])
	}
}

// The backoff doubles up to ten minutes and stops there.
func TestBackoffIsCappedAtTenMinutes(t *testing.T) {
	w := newWorld(t)
	w.start()
	var seq []time.Duration
	for i := 0; i < 7; i++ {
		w.lz.FailNext("/im/session/list", "ApiCallLimit")
		w.svc.RunRound(context.Background())
		seq = append(seq, w.svc.Backoff())
		w.svc.DropPending() // drop what the failed round put back
	}
	want := []time.Duration{30 * time.Second, 60 * time.Second, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	if !reflect.DeepEqual(seq, want) {
		t.Errorf("backoff sequence = %v, want %v", seq, want)
	}
}

// An authorization failure posts one note in General and stops; a good
// /lazada_auth starts fetching again.
func TestAuthFailureStopsFetchingUntilLazadaAuthSucceeds(t *testing.T) {
	w := newWorld(t, func(o *worldOpts) { o.unauth = true })
	w.start()
	id := w.buyer("s1", "hello")

	w.run()
	w.waitRounds(1)
	general := w.topics.texts(0)
	if len(general) != 1 || !strings.HasPrefix(general[0], "Lazada is not authorized. Approve the app at https://auth.lazada.com/oauth/authorize?") ||
		!strings.HasSuffix(general[0], ", then send /lazada_auth <code> here.") {
		t.Fatalf("General topic = %q", general)
	}

	callsBefore := w.lz.Calls("/im/session/list") + w.lz.Calls("/im/message/list")
	for i := 0; i < 4; i++ {
		w.svc.Poll("")
		w.svc.Poll("s1")
	}
	time.Sleep(30 * time.Millisecond)
	if n := w.lz.Calls("/im/session/list") + w.lz.Calls("/im/message/list"); n != callsBefore {
		t.Errorf("fetching went on while not authorized (%d calls)", n-callsBefore)
	}
	if n := len(w.topics.texts(0)); n != 1 {
		t.Errorf("%d notes in General, want exactly 1", n)
	}

	w.lz.AllowCode("fresh-code")
	w.svc.HandleAuth(context.Background(), AuthRequest{FromID: testOwner, MsgID: 77, Args: "fresh-code"})
	waitFor(t, "fetching to resume", func() bool { return len(w.events.Customers()) == 1 })
	if w.events.Customers()[0].HubMsgID != id {
		t.Errorf("events = %+v", w.events.Customers())
	}
}

// A revoked token also stops the channel, even after it worked before.
func TestRevokedTokenStopsTheChannel(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.lz.RevokeTokens()
	w.buyer("s1", "hi")
	w.run()
	w.waitRounds(1)
	if g := w.topics.texts(0); len(g) != 1 || !strings.Contains(g[0], "Lazada is not authorized") {
		t.Errorf("General = %q", g)
	}
}
