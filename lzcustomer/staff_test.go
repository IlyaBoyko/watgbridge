package lzcustomer_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"watgbridge/agentlink"
	"watgbridge/database"
	. "watgbridge/lzcustomer"
	"watgbridge/state"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"go.uber.org/zap"
)

var loki = &gotgbot.User{Id: 42, FirstName: "Loki", LastName: "Tan"}

func staffMsg(text string) *gotgbot.Message {
	return &gotgbot.Message{
		MessageId: 500, MessageThreadId: 1250, IsTopicMessage: true, Text: text, From: loki,
		Chat: gotgbot.Chat{Id: -1001234567890, Type: "supergroup"},
	}
}

/* ------------------------------------------------------- the service side -- */

func TestStaffTextIsSentConfirmedAndReported(t *testing.T) {
	w := sendWorld(t)
	w.start()
	w.buyer(sess, "hello")
	mustNoErr(t, w.scan())

	out := w.svc.StaffToBuyer(context.Background(), staffMsg("Yes, it is in stock."), sess)
	if out.Note != "" || !out.Confirmed {
		t.Fatalf("outcome = %+v", out)
	}
	sent := w.lz.Sent()
	if len(sent) != 1 || sent[0].Text != "Yes, it is in stock." || sent[0].SessionID != sess {
		t.Fatalf("sent = %+v", sent)
	}
	if w.sentRows()[sent[0].MessageID] != OriginTopic {
		t.Errorf("lazada_sent = %v, want origin topic", w.sentRows())
	}
	evs := w.events.Staffs()
	want := agentlink.StaffInput{
		ChatKey: "lz:S1", TopicID: 1250, HubMsgID: sent[0].MessageID, Source: "topic",
		TgUserID: 42, Name: "Loki Tan", Text: "Yes, it is in stock.",
	}
	if len(evs) != 1 || fmt.Sprintf("%+v", evs[0]) != fmt.Sprintf("%+v", want) {
		t.Errorf("staff events = %+v\nwant %+v", evs, want)
	}
	// The fetcher must not report it a second time as Seller Center speech.
	mustNoErr(t, w.scan())
	if n := len(w.events.Staffs()); n != 1 {
		t.Errorf("%d staff events after the next fetch", n)
	}
}

func TestStaffLongTextIsSplitAndReportedOnceWithTheFirstID(t *testing.T) {
	w := sendWorld(t, withTextLimit(25))
	w.start()
	text := "First paragraph here.\n\nSecond paragraph here."

	out := w.svc.StaffToBuyer(context.Background(), staffMsg(text), sess)
	if !out.Confirmed || out.Note != "" {
		t.Fatalf("outcome = %+v", out)
	}
	sent := w.lz.Sent()
	if len(sent) != 2 || sent[0].Text != "First paragraph here." || sent[1].Text != "Second paragraph here." {
		t.Fatalf("sent = %+v", sent)
	}
	evs := w.events.Staffs()
	if len(evs) != 1 || evs[0].HubMsgID != sent[0].MessageID || evs[0].Text != text {
		t.Errorf("staff events = %+v", evs)
	}
}

func TestStaffMediaIsRefusedWithANoteAndNothingIsSent(t *testing.T) {
	photo := []gotgbot.PhotoSize{{FileId: "ph", Width: 10, Height: 10}}
	cases := map[string]func(m *gotgbot.Message){
		"photo":                func(m *gotgbot.Message) { m.Photo = photo },
		"photo with a caption": func(m *gotgbot.Message) { m.Photo, m.Caption = photo, "look at this" },
		"document":             func(m *gotgbot.Message) { m.Document = &gotgbot.Document{FileId: "d"}; m.Caption = "spec" },
		"voice":                func(m *gotgbot.Message) { m.Voice = &gotgbot.Voice{FileId: "v"} },
		"sticker":              func(m *gotgbot.Message) { m.Sticker = &gotgbot.Sticker{FileId: "s"} },
		"video":                func(m *gotgbot.Message) { m.Video = &gotgbot.Video{FileId: "vi"}; m.Caption = "clip" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			w := sendWorld(t)
			w.start()
			m := staffMsg("")
			mod(m)
			out := w.svc.StaffToBuyer(context.Background(), m, sess)
			if out.Note != "Images and files can't be sent to Lazada from here yet. Use Seller Center." || out.Confirmed {
				t.Errorf("outcome = %+v", out)
			}
			if len(w.lz.Sent()) != 0 || len(w.events.Staffs()) != 0 {
				t.Errorf("sent %+v, events %+v", w.lz.Sent(), w.events.Staffs())
			}
		})
	}
	w := sendWorld(t)
	w.start()
	m := staffMsg("")
	m.Location = &gotgbot.Location{}
	if out := w.svc.StaffToBuyer(context.Background(), m, sess); out.Note != TextOnlyNote || out.Confirmed {
		t.Errorf("a location: %+v", out)
	}
}

func TestStaffTextWhenNotAuthorizedGetsTheNoteAndNothingIsReported(t *testing.T) {
	for name, mod := range map[string]func(w *world){
		"no token stored": nil,
		"token refused":   func(w *world) { w.lz.FailNext("/im/message/send", "IllegalAccessToken") },
	} {
		t.Run(name, func(t *testing.T) {
			var opts []func(*worldOpts)
			if mod == nil {
				opts = append(opts, func(o *worldOpts) { o.unauth = true })
			}
			w := sendWorld(t, opts...)
			w.start()
			if mod != nil {
				mod(w)
			}
			out := w.svc.StaffToBuyer(context.Background(), staffMsg("hello"), sess)
			if out.Note != "Lazada is not authorized; send /lazada_auth first." || out.Confirmed {
				t.Errorf("outcome = %+v", out)
			}
			if len(w.lz.Sent()) != 0 || len(w.events.Staffs()) != 0 {
				t.Errorf("sent %+v, events %+v", w.lz.Sent(), w.events.Staffs())
			}
		})
	}
}

func TestStaffTextRateLimitedAndOtherFailuresGetANote(t *testing.T) {
	w := sendWorld(t)
	w.start()
	w.lz.FailNext("/im/message/send", "ApiCallLimit")
	out := w.svc.StaffToBuyer(context.Background(), staffMsg("hello"), sess)
	if !strings.Contains(out.Note, "rate limiting") || out.Confirmed || len(w.events.Staffs()) != 0 {
		t.Errorf("rate limited: %+v", out)
	}
	w.lz.FailNext("/im/message/send", "InvalidParameter")
	out = w.svc.StaffToBuyer(context.Background(), staffMsg("hello"), sess)
	if !strings.HasPrefix(out.Note, "Cannot send to Lazada: ") || !strings.Contains(out.Note, "InvalidParameter") || out.Confirmed {
		t.Errorf("other: %+v", out)
	}
}

func TestStaffPartialSendSaysHowManyPartsWentAndStillReportsThem(t *testing.T) {
	w := sendWorld(t, withTextLimit(12), func(o *worldOpts) {
		o.api = func(a API) API {
			return &failingSendAPI{API: a, pass: 1, err: errors.New("lazada: Timeout: gateway")}
		}
	})
	w.start()

	out := w.svc.StaffToBuyer(context.Background(), staffMsg("first part.\n\nsecond one.\n\nthird"), sess)
	if out.Confirmed || !strings.HasPrefix(out.Note, "Only 1 of 3 parts reached Lazada: ") || !strings.Contains(out.Note, "gateway") {
		t.Fatalf("outcome = %+v", out)
	}
	sent := w.lz.Sent()
	evs := w.events.Staffs()
	if len(sent) != 1 || len(evs) != 1 || evs[0].Text != "first part." || evs[0].HubMsgID != sent[0].MessageID || evs[0].Source != "topic" {
		t.Errorf("sent %+v, events %+v", sent, evs)
	}
}

/* ----------------------------------------------------------- the hooks ---- */

// tgCall is one Bot API call the Hub bot made.
type tgCall struct {
	Method string
	Form   map[string]string
}

// fakeBotAPI answers the Bot API calls the hooks make and writes them down.
type fakeBotAPI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []tgCall
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		call := tgCall{Method: parts[len(parts)-1], Form: map[string]string{}}
		if r.ParseMultipartForm(1<<20) == nil {
			for k, v := range r.MultipartForm.Value {
				call.Form[k] = v[0]
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, call)
		n := len(f.calls)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch call.Method {
		case "getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":111,"is_bot":true,"first_name":"Hub","username":"hub_bot"}}`)
		case "sendMessage":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d,"date":1,"chat":{"id":%s,"type":"supergroup"}}}`, 9000+n, call.Form["chat_id"]))
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotAPI) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.Method == "sendMessage" {
			out = append(out, c.Form["text"])
		}
	}
	return out
}

func (f *fakeBotAPI) callsTo(method string) []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tgCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

type hookWorld struct {
	*world
	api   *fakeBotAPI
	bot   *gotgbot.Bot
	group int64
}

// newHookWorld installs a bridge database holding a Lazada topic (1250) and a
// WhatsApp topic (1300), a Hub bot on a fake Bot API, and the channel.
func newHookWorld(t *testing.T, mod ...func(*worldOpts)) *hookWorld {
	t.Helper()
	h := &hookWorld{world: sendWorld(t, mod...), api: newFakeBotAPI(t), group: -1001234567890}
	h.start()
	bot, err := gotgbot.NewBot("111:HUB", &gotgbot.BotOpts{
		BotClient: &gotgbot.BaseBotClient{
			Client:             http.Client{},
			DefaultRequestOpts: &gotgbot.RequestOpts{APIURL: h.api.srv.URL, Timeout: 5 * time.Second},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.bot = bot

	oldDB, oldBot, oldChat := state.State.Database, state.State.TelegramBot, state.State.Config.Telegram.TargetChatID
	oldConf, oldLog := state.State.Config.Telegram.ConfirmationType, state.State.Logger
	state.State.Database, state.State.TelegramBot, state.State.Config.Telegram.TargetChatID = h.db, bot, h.group
	state.State.Config.Telegram.ConfirmationType, state.State.Logger = "text", zap.NewNop()
	t.Cleanup(func() {
		state.State.Database, state.State.TelegramBot, state.State.Config.Telegram.TargetChatID = oldDB, oldBot, oldChat
		state.State.Config.Telegram.ConfirmationType, state.State.Logger = oldConf, oldLog
		SetCurrent(nil)
	})
	if err := database.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	if err := database.ChatThreadAddNewPair("lz:"+sess, h.group, 1250); err != nil {
		t.Fatal(err)
	}
	if err := database.ChatThreadAddNewPair("60123456789@s.whatsapp.net", h.group, 1300); err != nil {
		t.Fatal(err)
	}
	SetCurrent(h.svc)
	return h
}

func (h *hookWorld) ctx(m *gotgbot.Message) *ext.Context {
	m.Chat = gotgbot.Chat{Id: h.group, Type: "supergroup"}
	return ext.NewContext(h.bot, &gotgbot.Update{UpdateId: 1, Message: m}, nil)
}

func TestTopicHookSendsConfirmsAndReports(t *testing.T) {
	h := newHookWorld(t)
	if !HandleTopicMessage(h.bot, h.ctx(staffMsg("In stock, ready today."))) {
		t.Fatal("the hook did not take a message of a Lazada topic")
	}
	sent := h.lz.Sent()
	if len(sent) != 1 || sent[0].Text != "In stock, ready today." {
		t.Fatalf("sent = %+v", sent)
	}
	if got := h.api.sentTexts(); len(got) != 1 || got[0] != "Successfully sent" {
		t.Errorf("Hub bot messages = %q, want the usual confirmation", got)
	}
	if evs := h.events.Staffs(); len(evs) != 1 || evs[0].Source != "topic" || evs[0].HubMsgID != sent[0].MessageID || evs[0].TopicID != 1250 {
		t.Errorf("staff events = %+v", evs)
	}
}

func TestTopicHookRefusesAPhotoWithACaption(t *testing.T) {
	h := newHookWorld(t)
	m := staffMsg("")
	m.Photo, m.Caption = []gotgbot.PhotoSize{{FileId: "ph", Width: 1, Height: 1}}, "this one?"
	if !HandleTopicMessage(h.bot, h.ctx(m)) {
		t.Fatal("not taken: it would go to WhatsApp")
	}
	if got := h.api.sentTexts(); len(got) != 1 || got[0] != MediaNote {
		t.Errorf("Hub bot messages = %q", got)
	}
	if len(h.lz.Sent()) != 0 || len(h.events.Staffs()) != 0 {
		t.Error("something was sent or reported")
	}
}

func TestTopicHookNotAuthorizedAndChannelOff(t *testing.T) {
	h := newHookWorld(t, func(o *worldOpts) { o.unauth = true })
	if !HandleTopicMessage(h.bot, h.ctx(staffMsg("hello"))) {
		t.Fatal("not taken")
	}
	if got := h.api.sentTexts(); len(got) != 1 || got[0] != NotAuthorizedNote {
		t.Errorf("not authorized: Hub bot messages = %q", got)
	}

	SetCurrent(nil)
	if !HandleTopicMessage(h.bot, h.ctx(staffMsg("hello"))) {
		t.Fatal("with the channel off the message was left for WhatsApp")
	}
	if got := h.api.sentTexts(); len(got) != 2 || got[1] != OffNote {
		t.Errorf("channel off: Hub bot messages = %q", got)
	}
	if len(h.lz.Sent()) != 0 || len(h.events.Staffs()) != 0 {
		t.Error("something was sent or reported")
	}
}

func TestTopicHookLeavesOtherTopicsAndServiceMessagesAlone(t *testing.T) {
	h := newHookWorld(t)
	wa := staffMsg("hi")
	wa.MessageThreadId = 1300
	if HandleTopicMessage(h.bot, h.ctx(wa)) {
		t.Error("a WhatsApp topic was taken by the Lazada hook")
	}
	general := staffMsg("hi")
	general.MessageThreadId = 0
	if HandleTopicMessage(h.bot, h.ctx(general)) {
		t.Error("a message outside topics was taken")
	}
	pin := staffMsg("")
	pin.PinnedMessage = &gotgbot.Message{MessageId: 9100}
	if !HandleTopicMessage(h.bot, h.ctx(pin)) {
		t.Error("a pin in a Lazada topic fell through to WhatsApp")
	}
	if n := len(h.api.sentTexts()); n != 0 || len(h.lz.Sent()) != 0 {
		t.Errorf("%d replies, %d sends for messages that are not the hook's", n, len(h.lz.Sent()))
	}
}

func TestTopicEditGetsTheEditNoteAndNothingElse(t *testing.T) {
	h := newHookWorld(t)
	m := staffMsg("corrected text")
	m.EditDate = 12345
	if !HandleTopicEdit(h.bot, h.ctx(m)) {
		t.Fatal("the edit was not taken")
	}
	if got := h.api.sentTexts(); len(got) != 1 || got[0] != "Edits can't be sent to Lazada; send a new message." {
		t.Errorf("Hub bot messages = %q", got)
	}
	if len(h.lz.Sent()) != 0 || len(h.events.Staffs()) != 0 {
		t.Error("an edit sent or reported something")
	}

	wa := staffMsg("x")
	wa.MessageThreadId = 1300
	if HandleTopicEdit(h.bot, h.ctx(wa)) {
		t.Error("an edit in a WhatsApp topic was taken by the Lazada hook")
	}
}
