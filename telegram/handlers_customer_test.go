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

// A5: a staff message in a topic that belongs to a Telegram customer is taken
// by the customer hook before anything WhatsApp is touched. With no WhatsApp
// client in this test, reaching the WhatsApp path would panic, so a clean
// return is the proof; the hook's own behaviour is tested in tgcustomer.
func TestCustomerTopicNeverReachesTheWhatsAppPath(t *testing.T) {
	var (
		mu    sync.Mutex
		sends []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Hub","username":"hub_bot"}}`)
		case "sendMessage":
			mu.Lock()
			sends = append(sends, r.FormValue("text")+"|thread="+r.FormValue("message_thread_id"))
			mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":5,"date":1,"chat":{"id":-1001,"type":"supergroup"}}}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	defer srv.Close()

	bot, err := gotgbot.NewBot("111:HUB", &gotgbot.BotOpts{BotClient: &gotgbot.BaseBotClient{
		Client:             http.Client{},
		DefaultRequestOpts: &gotgbot.RequestOpts{APIURL: srv.URL, Timeout: 5 * time.Second},
	}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()

	cfg := state.State.Config
	oldDB, oldOwner, oldChat := state.State.Database, cfg.Telegram.OwnerID, cfg.Telegram.TargetChatID
	defer func() {
		state.State.Database, cfg.Telegram.OwnerID, cfg.Telegram.TargetChatID = oldDB, oldOwner, oldChat
	}()
	state.State.Database, cfg.Telegram.OwnerID, cfg.Telegram.TargetChatID = db, 704338780, -1001
	if err := database.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	if err := database.ChatThreadAddNewPair("tg:5550001111", -1001, 1250); err != nil {
		t.Fatal(err)
	}

	msg := &gotgbot.Message{
		MessageId: 500, MessageThreadId: 1250, IsTopicMessage: true, Text: "Yes, in stock",
		From: &gotgbot.User{Id: 704338780, FirstName: "Ilya"},
		Chat: gotgbot.Chat{Id: -1001, Type: "supergroup"},
	}
	c := ext.NewContext(bot, &gotgbot.Update{UpdateId: 1, Message: msg}, nil)
	if err := BridgeTelegramToWhatsAppHandler(bot, c); err != nil {
		t.Fatalf("handler error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sends) != 1 || !strings.Contains(sends[0], "customer bot is turned off") || !strings.HasSuffix(sends[0], "thread=1250") {
		t.Errorf("messages the Hub bot sent = %q", sends)
	}
}
