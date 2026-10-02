package agentlink

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	waTypes "go.mau.fi/whatsmeow/types"
)

/* ----------------------------------------------------------------- clock -- */

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

/* -------------------------------------------------------------------- db -- */

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	// One connection keeps SQLite free of "database is locked" under the link's goroutines.
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

/* ----------------------------------------------------------------- fakes -- */

type waCall struct {
	Kind     string // text | image | document
	To       string
	Text     string
	Data     []byte
	Mime     string
	Filename string
	Quote    *Quote
}

type fakeWA struct {
	mu    sync.Mutex
	calls []waCall
	fail  error
	// failIf, when set, fails the calls it returns an error for.
	failIf func(waCall) error
	seq    int
}

func (f *fakeWA) record(c waCall) (SentMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failIf != nil {
		if err := f.failIf(c); err != nil {
			return SentMessage{}, err
		}
	}
	if f.fail != nil {
		return SentMessage{}, f.fail
	}
	f.calls = append(f.calls, c)
	f.seq++
	return SentMessage{ID: fmt.Sprintf("WA%03d", f.seq)}, nil
}

func (f *fakeWA) SendText(_ context.Context, to, text string, q *Quote) (SentMessage, error) {
	return f.record(waCall{Kind: "text", To: to, Text: text, Quote: q})
}

func (f *fakeWA) SendImage(_ context.Context, to string, data []byte, mime, caption string, q *Quote) (SentMessage, error) {
	return f.record(waCall{Kind: "image", To: to, Data: data, Mime: mime, Text: caption, Quote: q})
}

func (f *fakeWA) SendDocument(_ context.Context, to string, data []byte, mime, filename, caption string, q *Quote) (SentMessage, error) {
	return f.record(waCall{Kind: "document", To: to, Data: data, Mime: mime, Filename: filename, Text: caption, Quote: q})
}

func (f *fakeWA) Calls() []waCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]waCall(nil), f.calls...)
}

type topicPost struct {
	Thread int64
	Text   string // text, or the caption of a media post
	Media  *OutMedia
}

type fakeTopics struct {
	mu    sync.Mutex
	posts []topicPost
	fail  error
	seq   int64
	// cards and edits are kept apart from posts: they are not mirrors.
	cards    []cardPost
	edits    []cardEdit
	editFail error
}

func (f *fakeTopics) PostText(_ context.Context, thread int64, text string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, f.fail
	}
	f.posts = append(f.posts, topicPost{Thread: thread, Text: text})
	f.seq++
	return 9000 + f.seq, nil
}

func (f *fakeTopics) PostMedia(_ context.Context, thread int64, m OutMedia) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, f.fail
	}
	f.posts = append(f.posts, topicPost{Thread: thread, Text: m.Caption, Media: &m})
	f.seq++
	return 9000 + f.seq, nil
}

type cardPost struct {
	Thread int64
	Text   string
	KB     Keyboard
}

type cardEdit struct {
	ChatID, MsgID int64
	Text          string
	KB            Keyboard
}

func (f *fakeTopics) PostCard(_ context.Context, thread int64, text string, kb Keyboard) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, 0, f.fail
	}
	f.cards = append(f.cards, cardPost{Thread: thread, Text: text, KB: kb})
	f.seq++
	return -1001, 9000 + f.seq, nil
}

func (f *fakeTopics) EditCardMessage(_ context.Context, chatID, msgID int64, text string, kb Keyboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editFail != nil {
		return f.editFail
	}
	f.edits = append(f.edits, cardEdit{ChatID: chatID, MsgID: msgID, Text: text, KB: kb})
	return nil
}

func (f *fakeTopics) Cards() []cardPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cardPost(nil), f.cards...)
}

func (f *fakeTopics) Edits() []cardEdit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cardEdit(nil), f.edits...)
}

func (f *fakeTopics) Posts() []topicPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]topicPost(nil), f.posts...)
}

type pairRecord struct {
	WaMsgID, ChatKey string
	TgMsgID, Thread  int64
}

type fakeBridge struct {
	mu           sync.Mutex
	threads      map[string]int64
	participants map[string]string
	pairs        []pairRecord
	fwd          []forwarded
	selfUser     string
}

func newFakeBridge() *fakeBridge {
	return &fakeBridge{
		threads:      map[string]int64{"60123456789@s.whatsapp.net": 1234},
		participants: map[string]string{},
		selfUser:     "60199999999",
	}
}

func (b *fakeBridge) IsSelf(jid waTypes.JID) bool { return jid.User == b.selfUser }

func (b *fakeBridge) ThreadFor(key string) (int64, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.threads[key]
	return id, ok, nil
}

func (b *fakeBridge) ParticipantOf(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.participants[id]
}

func (b *fakeBridge) RecordPair(waMsgID, key string, tgMsgID, thread int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pairs = append(b.pairs, pairRecord{waMsgID, key, tgMsgID, thread})
	return nil
}

// forwarded is a pair the bridge made when it sent a staff message on.
type forwarded struct {
	TgMsgID, Thread int64
	WaChat, WaID    string
}

func (b *fakeBridge) PairIDsFor(tgMsgID, thread int64, waChat string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for _, f := range b.fwd {
		if f.TgMsgID == tgMsgID && f.Thread == thread && f.WaChat == waChat {
			ids = append(ids, f.WaID)
		}
	}
	return ids, nil
}

// addForwarded plays the part of TgSendToWhatsApp storing its pair.
func (b *fakeBridge) addForwarded(f forwarded) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fwd = append(b.fwd, f)
}

func (b *fakeBridge) Pairs() []pairRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]pairRecord(nil), b.pairs...)
}

type fakeNotifier struct {
	mu    sync.Mutex
	texts []string
}

func (n *fakeNotifier) NotifyOwner(text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.texts = append(n.texts, text)
}

func (n *fakeNotifier) Texts() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.texts...)
}

/* ------------------------------------------------------------- the world -- */

type world struct {
	t        *testing.T
	db       *gorm.DB
	clock    *fakeClock
	wa       *fakeWA
	topics   *fakeTopics
	bridge   *fakeBridge
	notifier *fakeNotifier
	customer *fakeCustomer
	log      *zap.Logger
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return &world{
		t: t, db: newTestDB(t), clock: newFakeClock(), wa: &fakeWA{}, topics: &fakeTopics{},
		bridge: newFakeBridge(), notifier: &fakeNotifier{}, customer: &fakeCustomer{targets: map[[2]int64]string{}}, log: zap.NewNop(),
	}
}

func (w *world) deps() Deps {
	return Deps{
		DB: w.db, Bridge: w.bridge, WA: w.wa, Customer: w.customer, Topics: w.topics, Notifier: w.notifier,
		Clock: w.clock, Log: w.log, HubVersion: "test",
		Link: LinkOptions{
			PingInterval: 50 * time.Millisecond, DeadAfter: 2 * time.Second, HandshakeTimeout: 2 * time.Second,
			BackoffMin: 10 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
		},
	}
}

func (w *world) hub(url string) *Hub {
	w.t.Helper()
	h, err := NewHub(Config{Enabled: true, URL: url, Token: "secret-token", HubID: "pi-test", OutboxMaxAgeDays: 7}, w.deps())
	if err != nil {
		w.t.Fatal(err)
	}
	return h
}

func (w *world) executor() *Executor {
	if err := Migrate(w.db); err != nil {
		w.t.Fatal(err)
	}
	return &Executor{
		WA: w.wa, Topics: w.topics, Customer: w.customer, Bridge: w.bridge, Memory: NewCommandMemory(w.db, w.clock),
		Cards: NewCardStore(w.db, w.clock), Clock: w.clock, Guard: NewSentGuard(w.clock), Log: w.log,
	}
}

/* ------------------------------------------------------------ fake agent -- */

type fakeAgent struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	nConn  int
	script func(s *agentSession)
	done   chan struct{}
}

type agentSession struct {
	t      *testing.T
	n      int // 1-based connection number
	conn   *websocket.Conn
	ctx    context.Context
	frames chan Envelope
	auth   string
}

func newFakeAgent(t *testing.T, script func(s *agentSession)) *fakeAgent {
	t.Helper()
	a := &fakeAgent{t: t, script: script, done: make(chan struct{})}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(maxFrameBytes)
		a.mu.Lock()
		a.nConn++
		n := a.nConn
		a.mu.Unlock()
		s := &agentSession{t: t, n: n, conn: conn, ctx: r.Context(), frames: make(chan Envelope, 256), auth: r.Header.Get("Authorization")}
		defer conn.CloseNow()
		a.script(s)
		select {
		case <-a.done:
		case <-s.ctx.Done():
		}
	}))
	t.Cleanup(func() {
		close(a.done)
		a.srv.CloseClientConnections()
		a.srv.Close()
	})
	return a
}

func (a *fakeAgent) URL() string { return "ws" + strings.TrimPrefix(a.srv.URL, "http") + "/hub" }

func (a *fakeAgent) Connections() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nConn
}

// startReader pumps frames from the Hub into s.frames. It also keeps the
// connection reading, which is what answers the Hub's pings.
func (s *agentSession) startReader() {
	go func() {
		defer close(s.frames)
		for {
			_, data, err := s.conn.Read(s.ctx)
			if err != nil {
				return
			}
			env, err := DecodeEnvelope(data)
			if err != nil {
				s.t.Errorf("hub sent a bad envelope: %v: %s", err, data)
				return
			}
			s.frames <- env
		}
	}()
}

func (s *agentSession) next(timeout time.Duration) (Envelope, error) {
	select {
	case env, ok := <-s.frames:
		if !ok {
			return Envelope{}, errors.New("connection ended")
		}
		return env, nil
	case <-time.After(timeout):
		return Envelope{}, errors.New("timed out waiting for a frame")
	}
}

func (s *agentSession) mustNext() Envelope {
	s.t.Helper()
	env, err := s.next(3 * time.Second)
	if err != nil {
		s.t.Fatalf("agent session %d: %v", s.n, err)
	}
	return env
}

func (s *agentSession) expectType(typ string) Envelope {
	s.t.Helper()
	env := s.mustNext()
	if env.Type != typ {
		s.t.Fatalf("agent session %d: got frame type %q, want %q", s.n, env.Type, typ)
	}
	return env
}

func (s *agentSession) write(id, typ string, payload any) {
	s.t.Helper()
	env, err := NewEnvelope(id, typ, time.Now(), payload)
	if err != nil {
		s.t.Fatal(err)
	}
	s.writeEnv(env)
}

func (s *agentSession) writeEnv(env Envelope) {
	s.t.Helper()
	data := mustJSON(s.t, env)
	if err := s.conn.Write(s.ctx, websocket.MessageText, data); err != nil {
		s.t.Fatalf("agent session %d: write: %v", s.n, err)
	}
}

func (s *agentSession) welcome() {
	s.write("01JB00000000000000000000A0", TypeWelcome, Welcome{Protocol: 1, AgentVersion: "test"})
}

// handshake reads hello (checking the token) and answers welcome.
func (s *agentSession) handshake() Envelope {
	s.t.Helper()
	s.startReader()
	hello := s.expectType(TypeHello)
	if s.auth != "Bearer secret-token" {
		s.t.Errorf("Authorization header = %q", s.auth)
	}
	s.welcome()
	return hello
}

// waitFor polls until cond is true.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

/* ------------------------------------------------------ customer channel -- */

type custSend struct {
	File   bool
	ChatID int64
	Text   CustomerText
	Media  CustomerFile
}

type custPair struct{ Chat, CustMsg, Thread, TopicMsg int64 }

// fakeCustomer is the Telegram customer bot's channel.
type fakeCustomer struct {
	mu     sync.Mutex
	sends  []custSend
	pairs  []custPair
	failIf func(custSend) error
	seq    int64
	// targets answers HubMsgIDOfTopicMsg, keyed by thread and topic message.
	targets map[[2]int64]string
}

func (f *fakeCustomer) record(s custSend) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failIf != nil {
		if err := f.failIf(s); err != nil {
			return 0, err
		}
	}
	f.sends = append(f.sends, s)
	f.seq++
	return 7000 + f.seq, nil
}

func (f *fakeCustomer) SendText(_ context.Context, chatID int64, m CustomerText) (int64, error) {
	return f.record(custSend{ChatID: chatID, Text: m})
}

func (f *fakeCustomer) SendFile(_ context.Context, chatID int64, m CustomerFile) (int64, error) {
	return f.record(custSend{File: true, ChatID: chatID, Media: m})
}

func (f *fakeCustomer) RecordPair(chat, custMsg, thread, topicMsg int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairs = append(f.pairs, custPair{chat, custMsg, thread, topicMsg})
	return nil
}

func (f *fakeCustomer) HubMsgIDOfTopicMsg(thread, topicMsg int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.targets[[2]int64{thread, topicMsg}]
}

func (f *fakeCustomer) Sends() []custSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]custSend(nil), f.sends...)
}

func (f *fakeCustomer) Pairs() []custPair {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]custPair(nil), f.pairs...)
}
