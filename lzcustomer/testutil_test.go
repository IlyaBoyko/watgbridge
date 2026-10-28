package lzcustomer_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	. "watgbridge/lzcustomer"

	"watgbridge/agentlink"
	"watgbridge/lzcustomer/lzfake"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
)

/* ---------------------------------------------------------------- journal -- */

// journal is one ordered record of everything the channel did to the outside
// world, so a test can say "the note came before the message, then the event".
type journal struct {
	mu    sync.Mutex
	lines []string
}

func (j *journal) add(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, fmt.Sprintf(format, args...))
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.lines...)
}

/* ------------------------------------------------------------------ clock -- */

type waiter struct {
	at time.Time
	ch chan time.Time
}

type tickerChan struct {
	d  time.Duration
	ch chan time.Time
}

// simClock only moves when told to. Tickers tick when the test says so.
type simClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
	afters  []time.Duration
	tickers []tickerChan
}

func newSimClock() *simClock {
	return &simClock{now: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)}
}

func (c *simClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *simClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var keep []waiter
	var fire []waiter
	for _, w := range c.waiters {
		if !w.at.After(now) {
			fire = append(fire, w)
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
	c.mu.Unlock()
	for _, w := range fire {
		w.ch <- now
	}
}

func (c *simClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.afters = append(c.afters, d)
	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Afters is every duration anyone waited for, oldest first.
func (c *simClock) Afters() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.afters...)
}

func (c *simClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time)
	c.tickers = append(c.tickers, tickerChan{d: d, ch: ch})
	return ch, func() {}
}

// Tick delivers one tick to the ticker of period d. The channel is unbuffered,
// so when Tick returns the receiving loop has taken the tick, and the loop's
// next receive means the previous tick's work was done.
func (c *simClock) Tick(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		var ch chan time.Time
		for _, tk := range c.tickers {
			if tk.d == d {
				ch = tk.ch
			}
		}
		now := c.now
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- now:
				return
			case <-time.After(5 * time.Second):
				t.Fatalf("nobody took the %v tick", d)
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no ticker of %v was created", d)
}

/* ------------------------------------------------------------------ fakes -- */

type fakePost struct {
	Thread int64
	TopicPost
	ID int64
}

// fullText is what a reader of the topic sees: prefix and text.
func (p fakePost) fullText() string { return p.Prefix + p.Text }

type fakeTopics struct {
	j         *journal
	mu        sync.Mutex
	created   []string
	posts     []fakePost
	deleted   []int64
	next      int64
	seq       int64
	createErr error
	postErr   func(TopicPost) error
	deleteErr error
}

func newFakeTopics(j *journal) *fakeTopics { return &fakeTopics{j: j, next: 1250} }

func (f *fakeTopics) CreateTopic(_ context.Context, name string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return 0, f.createErr
	}
	f.created = append(f.created, name)
	f.next++
	f.j.add("create-topic:%s", name)
	return f.next - 1, nil
}

func (f *fakeTopics) Post(_ context.Context, thread int64, p TopicPost) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		if err := f.postErr(p); err != nil {
			return 0, err
		}
	}
	f.seq++
	id := 9000 + f.seq
	f.posts = append(f.posts, fakePost{Thread: thread, TopicPost: p, ID: id})
	f.j.add("post:%d:%s:%s", thread, p.Kind, strings.ReplaceAll(p.Prefix+p.Text, "\n", "|"))
	return id, nil
}

func (f *fakeTopics) Delete(_ context.Context, msgID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, msgID)
	f.j.add("delete:%d", msgID)
	return f.deleteErr
}

func (f *fakeTopics) Posts() []fakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePost(nil), f.posts...)
}

func (f *fakeTopics) Created() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

// texts are the plain texts posted in a thread, oldest first.
func (f *fakeTopics) texts(thread int64) []string {
	var out []string
	for _, p := range f.Posts() {
		if p.Thread == thread {
			out = append(out, p.fullText())
		}
	}
	return out
}

type fakeThreads struct {
	mu sync.Mutex
	m  map[string]int64
}

func (f *fakeThreads) Find(key string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.m[key]
	return id, ok, nil
}

func (f *fakeThreads) Add(key string, thread int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = thread
	return nil
}

type fakeEvents struct {
	j         *journal
	mu        sync.Mutex
	customers []agentlink.CustomerInput
	staff     []agentlink.StaffInput
	// failOn makes the event of that hub message id fail once.
	failOn map[string]bool
}

func (f *fakeEvents) fails(id string) bool {
	if f.failOn[id] {
		delete(f.failOn, id)
		return true
	}
	return false
}

func (f *fakeEvents) Customer(in agentlink.CustomerInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails(in.HubMsgID) {
		return fmt.Errorf("outbox is broken")
	}
	f.customers = append(f.customers, in)
	f.j.add("customer:%s", in.HubMsgID)
	return nil
}

func (f *fakeEvents) Staff(in agentlink.StaffInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails(in.HubMsgID) {
		return fmt.Errorf("outbox is broken")
	}
	f.staff = append(f.staff, in)
	f.j.add("staff:%s", in.HubMsgID)
	return nil
}

func (f *fakeEvents) Customers() []agentlink.CustomerInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentlink.CustomerInput(nil), f.customers...)
}

func (f *fakeEvents) Staffs() []agentlink.StaffInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentlink.StaffInput(nil), f.staff...)
}

type fakeImages struct {
	mu    sync.Mutex
	data  map[string][]byte
	mime  string
	err   error
	calls []string
}

func (f *fakeImages) Fetch(_ context.Context, url string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, url)
	if f.err != nil {
		return nil, "", f.err
	}
	d, ok := f.data[url]
	if !ok {
		return nil, "", fmt.Errorf("404")
	}
	mt := f.mime
	if mt == "" {
		mt = "image/png"
	}
	return d, mt, nil
}

// stubAuth is an Authorizer with a scripted answer.
type stubAuth struct {
	j        *journal
	mu       sync.Mutex
	result   Authorized
	err      error
	status   Authorized
	hasState bool
	codes    []string
}

func (a *stubAuth) Authorize(_ context.Context, code string) (Authorized, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.codes = append(a.codes, code)
	a.j.add("authorize")
	return a.result, a.err
}

func (a *stubAuth) Status() (Authorized, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status, a.hasState
}

/* ------------------------------------------------------------------ world -- */

type world struct {
	t       *testing.T
	db      *gorm.DB
	lz      *lzfake.Server
	clock   *simClock
	j       *journal
	topics  *fakeTopics
	threads *fakeThreads
	events  *fakeEvents
	images  *fakeImages
	logs    *observer.ObservedLogs
	tokens  *TokenSource
	svc     *Service
}

type worldOpts struct {
	api           func(API) API // wraps the API the service uses
	auth          Authorizer    // replaces the real token source
	unauth        bool          // no token is stored
	safety        time.Duration
	active        time.Duration
	images        ImageFetcher
	loc           *time.Location
	boldBuyer     bool
	callbackURL   string
	textLimit     int
	pollAfterSend time.Duration
}

func newWorld(t *testing.T, mod ...func(*worldOpts)) *world {
	t.Helper()
	var o worldOpts
	for _, m := range mod {
		m(&o)
	}
	clock := newSimClock()
	lz := lzfake.New(testKey, testSecret)
	t.Cleanup(lz.Close)
	lz.SetNow(clock.Now)

	db := newTestDB(t)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	tokens := NewTokenSource(db, NewAuth(lz.HTTPClient(), lz.URL(), testKey, testSecret, clock.Now), clock.Now)
	if !o.unauth {
		lz.AllowCode("setup")
		if _, err := tokens.Authorize(context.Background(), "setup"); err != nil {
			t.Fatal(err)
		}
	}
	var api API = NewClient(lz.HTTPClient(), lz.URL(), testKey, testSecret, clock.Now, tokens)
	if o.api != nil {
		api = o.api(api)
	}
	var auth Authorizer = tokens
	if o.auth != nil {
		auth = o.auth
	}

	core, logs := observer.New(zap.DebugLevel)
	j := &journal{}
	w := &world{
		t: t, db: db, lz: lz, clock: clock, j: j, tokens: tokens, logs: logs,
		topics:  newFakeTopics(j),
		threads: &fakeThreads{m: map[string]int64{}},
		events:  &fakeEvents{j: j, failOn: map[string]bool{}},
		images:  &fakeImages{data: map[string][]byte{}},
	}
	var imgs ImageFetcher = w.images
	if o.images != nil {
		imgs = o.images
	}
	cb := o.callbackURL
	if cb == "" {
		cb = "https://clarusmolecular.com/"
	}
	svc, err := NewService(Options{
		API: api, Auth: auth, DB: db, Topics: w.topics, Threads: w.threads, Events: w.events,
		Images: imgs, Clock: clock, Log: zap.New(core),
		AppKey: testKey, CallbackURL: cb, OwnerID: testOwner, Location: o.loc,
		SafetyEvery: o.safety, ActiveEvery: o.active, BoldCustomer: o.boldBuyer,
		TextLimit: o.textLimit, PollAfterSend: o.pollAfterSend,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.svc = svc
	return w
}

// start records the moment the channel first ran, as Run does, and moves the
// clock on: messages added before it are history, after it are new.
func (w *world) start() {
	w.t.Helper()
	if _, _, err := w.svc.Meta(w.clock.Now().UnixMilli()); err != nil {
		w.t.Fatal(err)
	}
	w.clock.Advance(time.Second)
}

// scan is one all-sessions round, run in the test's goroutine.
func (w *world) scan() error {
	return w.svc.ScanAll(context.Background())
}

// fetchOne is a round for one session.
func (w *world) fetchOne(id string) error {
	return w.svc.FetchSessions(context.Background(), id)
}

// buyer adds a text message from the buyer and moves the clock a little.
func (w *world) buyer(session, text string) string {
	w.clock.Advance(10 * time.Millisecond)
	return w.lz.AddBuyerMessage(session, 1, lzfake.TextContent(text))
}

func (w *world) seller(session, text string, o lzfake.SellerOpts) string {
	w.clock.Advance(10 * time.Millisecond)
	return w.lz.AddSellerMessage(session, 1, lzfake.TextContent(text), o)
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// waitFor polls until cond is true.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// run starts the service's goroutines and stops them with the test.
func (w *world) run() {
	w.t.Helper()
	w.svc.Run(context.Background())
	w.t.Cleanup(w.svc.Stop)
}

func (w *world) waitRounds(n int64) {
	w.t.Helper()
	waitFor(w.t, fmt.Sprintf("%d fetch rounds", n), func() bool { return w.svc.Rounds() >= n })
}

// logged returns the messages of the logs at or above a message prefix.
func (w *world) logged(prefix string) []string {
	var out []string
	for _, e := range w.logs.All() {
		if strings.HasPrefix(e.Message, prefix) {
			out = append(out, e.Message)
		}
	}
	return out
}

func contains(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

const testOwner = int64(704338780)
