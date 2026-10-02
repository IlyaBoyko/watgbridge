package tgcustomer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"watgbridge/agentlink"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

/* -------------------------------------------------------------------- db -- */

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

/* ----------------------------------------------------------------- fakes -- */

type fakePost struct {
	Thread int64
	TopicPost
	ID int64
}

// fakeTopics is the Hub bot: it posts into topics, creates them, and holds the
// files staff sent.
type fakeTopics struct {
	mu        sync.Mutex
	created   []string
	posts     []fakePost
	files     map[string][]byte
	downloads []string
	createErr error
	postErr   error
	dlErr     map[string]error
	nextThred int64
	seq       int64
}

func newFakeTopics() *fakeTopics {
	return &fakeTopics{files: map[string][]byte{}, dlErr: map[string]error{}, nextThred: 1250}
}

func (f *fakeTopics) Download(_ context.Context, fileID string, _ int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads = append(f.downloads, fileID)
	if err := f.dlErr[fileID]; err != nil {
		return nil, err
	}
	return f.files[fileID], nil
}

func (f *fakeTopics) CreateTopic(_ context.Context, name string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return 0, f.createErr
	}
	f.created = append(f.created, name)
	f.nextThred++
	return f.nextThred - 1, nil
}

func (f *fakeTopics) Post(_ context.Context, thread int64, p TopicPost) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return 0, f.postErr
	}
	f.seq++
	id := 9000 + f.seq
	f.posts = append(f.posts, fakePost{Thread: thread, TopicPost: p, ID: id})
	return id, nil
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

// fakeFiles is the customer bot's downloads.
type fakeFiles struct {
	mu    sync.Mutex
	files map[string][]byte
	errs  map[string]error
	calls []string
}

func (f *fakeFiles) Download(_ context.Context, fileID string, _ int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fileID)
	if err := f.errs[fileID]; err != nil {
		return nil, err
	}
	return f.files[fileID], nil
}

func (f *fakeFiles) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type sentItem struct {
	File   bool
	ChatID int64
	Text   agentlink.CustomerText
	Media  agentlink.CustomerFile
}

type fakeSender struct {
	mu    sync.Mutex
	sent  []sentItem
	err   error
	seq   int64
	label string
}

func (f *fakeSender) SendText(_ context.Context, chatID int64, m agentlink.CustomerText) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.seq++
	f.sent = append(f.sent, sentItem{ChatID: chatID, Text: m})
	return 7000 + f.seq, nil
}

func (f *fakeSender) SendFile(_ context.Context, chatID int64, m agentlink.CustomerFile) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.seq++
	f.sent = append(f.sent, sentItem{File: true, ChatID: chatID, Media: m})
	return 7000 + f.seq, nil
}

func (f *fakeSender) Sent() []sentItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentItem(nil), f.sent...)
}

type fakeThreads struct {
	mu     sync.Mutex
	m      map[string]int64
	addErr error
	adds   []string
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
	if f.addErr != nil {
		return f.addErr
	}
	f.m[key] = thread
	f.adds = append(f.adds, key)
	return nil
}

type fakeEvents struct {
	mu        sync.Mutex
	customers []agentlink.CustomerInput
	staff     []agentlink.StaffInput
	err       error
}

func (f *fakeEvents) Customer(in agentlink.CustomerInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customers = append(f.customers, in)
	return f.err
}

func (f *fakeEvents) Staff(in agentlink.StaffInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staff = append(f.staff, in)
	return f.err
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

/* ------------------------------------------------------------- the world -- */

type world struct {
	t       *testing.T
	topics  *fakeTopics
	files   *fakeFiles
	sender  *fakeSender
	threads *fakeThreads
	events  *fakeEvents
	pairs   *Pairs
	bridge  *Bridge
}

func newWorld(t *testing.T) *world {
	t.Helper()
	db := newTestDB(t)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	w := &world{
		t: t, topics: newFakeTopics(),
		files:   &fakeFiles{files: map[string][]byte{}, errs: map[string]error{}},
		sender:  &fakeSender{},
		threads: &fakeThreads{m: map[string]int64{}},
		events:  &fakeEvents{},
		pairs:   NewPairs(db),
	}
	w.bridge = &Bridge{
		CustomerFiles: w.files, Topics: w.topics, Sender: w.sender, Threads: w.threads,
		Pairs: w.pairs, Events: w.events, Log: zap.NewNop(),
	}
	return w
}

func (w *world) handle(msg *gotgbot.Message, edited bool) {
	w.t.Helper()
	w.bridge.HandleMessage(context.Background(), msg, edited)
}

const customerID = int64(5550001111)

func customerUser() *gotgbot.User {
	return &gotgbot.User{Id: customerID, FirstName: "Wei", LastName: "Tan", Username: "wei_kl"}
}

// privateMsg is a message the customer sends to the bot.
func privateMsg(id int64) *gotgbot.Message {
	return &gotgbot.Message{
		MessageId: id, Date: 1, From: customerUser(),
		Chat: gotgbot.Chat{Id: customerID, Type: "private"},
	}
}

func textMsg(id int64, text string) *gotgbot.Message {
	m := privateMsg(id)
	m.Text = text
	return m
}

/* ------------------------------------------------------ fake Bot API server -- */

type tgCall struct {
	Method    string
	Form      map[string]string
	Files     map[string][]byte
	FileNames map[string]string
}

// fakeTG is a stand-in for the Telegram Bot API. It speaks just enough HTTP
// for gotgbot: multipart requests in, JSON out.
type fakeTG struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []tgCall
	updates []string
	// respond overrides the answer of a method: it returns the HTTP status and
	// the body.
	respond map[string]func(c tgCall) (int, string)
	files   map[string][]byte // file_id -> content
	seq     int64
}

func newFakeTG(t *testing.T) *fakeTG {
	t.Helper()
	f := &fakeTG{t: t, respond: map[string]func(tgCall) (int, string){}, files: map[string][]byte{}, seq: 100}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTG) URL() string { return f.srv.URL }

func (f *fakeTG) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) >= 4 && parts[0] == "file" {
		// /file/bot<token>/files/<file_id>
		f.mu.Lock()
		data, ok := f.files[parts[3]]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
		return
	}
	method := parts[len(parts)-1]

	call := tgCall{Method: method, Form: map[string]string{}, Files: map[string][]byte{}, FileNames: map[string]string{}}
	if err := r.ParseMultipartForm(64 << 20); err == nil {
		for k, v := range r.MultipartForm.Value {
			call.Form[k] = v[0]
		}
		for k, fh := range r.MultipartForm.File {
			file, _ := fh[0].Open()
			data, _ := io.ReadAll(file)
			file.Close()
			call.Files[k], call.FileNames[k] = data, fh[0].Filename
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	override := f.respond[method]
	f.seq++
	seq := f.seq
	var update string
	if method == "getUpdates" && len(f.updates) > 0 {
		// Everything queued comes out as one batch, like a busy bot's.
		update, f.updates = strings.Join(f.updates, ","), nil
	}
	f.mu.Unlock()

	reply := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	if override != nil {
		status, body := override(call)
		reply(status, body)
		return
	}
	ok := func(result string) { reply(200, `{"ok":true,"result":`+result+`}`) }
	switch method {
	case "getMe":
		ok(`{"id":123456,"is_bot":true,"first_name":"Customers","username":"customers_bot"}`)
	case "deleteWebhook":
		ok(`true`)
	case "getUpdates":
		if update != "" {
			ok("[" + update + "]")
			return
		}
		// Long polling, shortened: an empty answer after a moment.
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Millisecond):
		}
		ok(`[]`)
	case "getFile":
		id := call.Form["file_id"]
		f.mu.Lock()
		data, found := f.files[id]
		f.mu.Unlock()
		if !found {
			reply(400, `{"ok":false,"error_code":400,"description":"Bad Request: invalid file_id"}`)
			return
		}
		ok(fmt.Sprintf(`{"file_id":%q,"file_path":"files/%s","file_size":%d}`, id, id, len(data)))
	case "createForumTopic":
		ok(`{"message_thread_id":4242,"name":"x","icon_color":0}`)
	case "sendMessage", "sendPhoto", "sendVideo", "sendAnimation", "sendVoice", "sendAudio", "sendDocument",
		"sendSticker", "sendLocation", "sendContact":
		chat := call.Form["chat_id"]
		ok(fmt.Sprintf(`{"message_id":%d,"date":1,"chat":{"id":%s,"type":"private"}}`, seq, chat))
	default:
		ok(`true`)
	}
}

func (f *fakeTG) Calls() []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tgCall(nil), f.calls...)
}

func (f *fakeTG) CallsTo(method string) []tgCall {
	var out []tgCall
	for _, c := range f.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeTG) Methods() []string {
	var out []string
	for _, c := range f.Calls() {
		out = append(out, c.Method)
	}
	return out
}

// Push queues an update for the next getUpdates.
func (f *fakeTG) Push(u gotgbot.Update) {
	raw, err := json.Marshal(u)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.updates = append(f.updates, string(raw))
	f.mu.Unlock()
}

// newTestBot makes a gotgbot bot that talks to the fake server.
func newTestBot(t *testing.T, f *fakeTG, token string) *gotgbot.Bot {
	t.Helper()
	b, err := gotgbot.NewBot(token, &gotgbot.BotOpts{
		BotClient: &gotgbot.BaseBotClient{
			Client:             http.Client{},
			DefaultRequestOpts: &gotgbot.RequestOpts{APIURL: f.URL(), Timeout: 5 * time.Second},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
