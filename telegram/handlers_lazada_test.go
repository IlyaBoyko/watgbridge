package telegram

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"watgbridge/database"
	"watgbridge/state"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// lzHub is a Hub bot on a fake Bot API, a database with a Lazada topic and a
// WhatsApp topic, and the handler's globals set for the test.
type lzHub struct {
	bot   *gotgbot.Bot
	mu    sync.Mutex
	sends []string
}

func (h *lzHub) Sent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sends...)
}

func newLzHub(t *testing.T) *lzHub {
	t.Helper()
	h := &lzHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Hub","username":"hub_bot"}}`)
		case "sendMessage":
			h.mu.Lock()
			h.sends = append(h.sends, r.FormValue("text")+"|thread="+r.FormValue("message_thread_id"))
			h.mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":5,"date":1,"chat":{"id":-1001,"type":"supergroup"}}}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(srv.Close)

	bot, err := gotgbot.NewBot("111:HUB", &gotgbot.BotOpts{BotClient: &gotgbot.BaseBotClient{
		Client:             http.Client{},
		DefaultRequestOpts: &gotgbot.RequestOpts{APIURL: srv.URL, Timeout: 5 * time.Second},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.bot = bot

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })

	cfg := state.State.Config
	oldDB, oldOwner, oldChat := state.State.Database, cfg.Telegram.OwnerID, cfg.Telegram.TargetChatID
	t.Cleanup(func() {
		state.State.Database, cfg.Telegram.OwnerID, cfg.Telegram.TargetChatID = oldDB, oldOwner, oldChat
	})
	state.State.Database, cfg.Telegram.OwnerID, cfg.Telegram.TargetChatID = db, 704338780, -1001
	if err := database.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	if err := database.ChatThreadAddNewPair("lz:100094063_2_1011822749_1_103", -1001, 1300); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *lzHub) handle(t *testing.T, text string, thread int64) {
	t.Helper()
	msg := &gotgbot.Message{
		MessageId: 500, MessageThreadId: thread, IsTopicMessage: thread != 0, Text: text,
		From: &gotgbot.User{Id: 704338780, FirstName: "Ilya"},
		Chat: gotgbot.Chat{Id: -1001, Type: "supergroup"},
	}
	c := ext.NewContext(h.bot, &gotgbot.Update{UpdateId: 1, Message: msg}, nil)
	// With no WhatsApp client in this test, reaching the WhatsApp path would
	// panic, so a clean return is the proof that it was not reached.
	if err := BridgeTelegramToWhatsAppHandler(h.bot, c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
}

// With the Lazada channel off, staff typing in a Lazada topic are told so, and
// nothing is sent anywhere (least of all to WhatsApp). The channel's own paths
// are tested in lzcustomer.
func TestLazadaTopicMessageWithTheChannelOffGetsTheNoteAndGoesNowhere(t *testing.T) {
	h := newLzHub(t)
	h.handle(t, "Yes, in stock", 1300)

	sent := h.Sent()
	if len(sent) != 1 || sent[0] != "Lazada is turned off (lazada.enabled), so this was not sent to Lazada.|thread=1300" {
		t.Errorf("messages the Hub bot sent = %q", sent)
	}
}

// An edit in a Lazada topic is answered by the Lazada hook and never looked up
// as a WhatsApp message (which would panic here: there is no WhatsApp client).
func TestEditInALazadaTopicGetsTheEditNote(t *testing.T) {
	h := newLzHub(t)
	msg := &gotgbot.Message{
		MessageId: 500, MessageThreadId: 1300, IsTopicMessage: true, Text: "fixed", EditDate: 99,
		From: &gotgbot.User{Id: 704338780, FirstName: "Ilya"},
		Chat: gotgbot.Chat{Id: -1001, Type: "supergroup"},
	}
	c := ext.NewContext(h.bot, &gotgbot.Update{UpdateId: 1, EditedMessage: msg}, nil)
	if err := BridgeTelegramEditedToWhatsAppHandler(h.bot, c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	sent := h.Sent()
	if len(sent) != 1 || sent[0] != "Edits can't be sent to Lazada; send a new message.|thread=1300" {
		t.Errorf("messages the Hub bot sent = %q", sent)
	}
}

// The /ai_ intercept runs before the Lazada hook: an /ai_ command in a Lazada
// topic is the Agent's (here the link is off, so staff are told so), not the
// Lazada hook's note.
func TestAICommandInALazadaTopicIsInterceptedFirst(t *testing.T) {
	h := newLzHub(t)
	h.handle(t, "/ai_status", 1300)

	sent := h.Sent()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "The agent isn't available here.") || strings.Contains(sent[0], "turned off") {
		t.Errorf("messages the Hub bot sent = %q", sent)
	}
}

// /lazada_auth is never forwarded, from a customer's topic or from General,
// even with the Lazada channel off.
func TestLazadaAuthCommandNeverReachesWhatsApp(t *testing.T) {
	h := newLzHub(t)
	if err := database.ChatThreadAddNewPair("60123456789@s.whatsapp.net", -1001, 1400); err != nil {
		t.Fatal(err)
	}
	for _, thread := range []int64{0, 1300, 1400} {
		h.handle(t, "/lazada_auth SOME-CODE", thread)
	}
	sent := h.Sent()
	if len(sent) != 3 {
		t.Fatalf("messages the Hub bot sent = %q", sent)
	}
	for _, s := range sent {
		if !strings.Contains(s, "Lazada is turned off") || strings.Contains(s, "SOME-CODE") {
			t.Errorf("reply = %q", s)
		}
	}
}
