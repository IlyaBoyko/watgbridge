package tgcustomer

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"watgbridge/agentlink"
	"watgbridge/database"
	"watgbridge/state"
	"watgbridge/telegram/middlewares"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"gorm.io/gorm"
)

// withBridgeState points the bridge's global state at test doubles for the
// length of a test.
func withBridgeState(t *testing.T, db *gorm.DB, hub *gotgbot.Bot, targetChat int64) {
	t.Helper()
	oldDB, oldBot, oldChat := state.State.Database, state.State.TelegramBot, state.State.Config.Telegram.TargetChatID
	state.State.Database, state.State.TelegramBot, state.State.Config.Telegram.TargetChatID = db, hub, targetChat
	t.Cleanup(func() {
		state.State.Database, state.State.TelegramBot, state.State.Config.Telegram.TargetChatID = oldDB, oldBot, oldChat
	})
}

func newCustomerBot(t *testing.T, f *fakeTG) *customerBot {
	t.Helper()
	return &customerBot{bot: newTestBot(t, f, "222:CUST"), hc: http.DefaultClient}
}

func tgError(code int, desc string, extra string) func(tgCall) (int, string) {
	return func(tgCall) (int, string) {
		return code, `{"ok":false,"error_code":` + itoa(code) + `,"description":"` + desc + `"` + extra + `}`
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// A6 (glue): copyables and the link reach Telegram as copy_text and url
// buttons, the text is sent with HTML parse mode, and the quote cannot fail
// the send.
func TestCustomerBotSendTextBuildsTheKeyboard(t *testing.T) {
	f := newFakeTG(t)
	cb := newCustomerBot(t, f)

	id, err := cb.SendText(t.Context(), customerID, agentlink.CustomerText{
		HTML: "Pay <code>1234</code>",
		Buttons: [][]agentlink.CustomerButton{
			{{Text: "Copy Bank", CopyText: "1234"}},
			{{Text: "View your order", URL: "https://example.com/o/1"}},
		},
		ReplyTo: 40,
	})
	if err != nil || id == 0 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	calls := f.CallsTo("sendMessage")
	if len(calls) != 1 {
		t.Fatalf("%d sendMessage calls", len(calls))
	}
	c := calls[0]
	if c.Form["chat_id"] != "5550001111" || c.Form["text"] != "Pay <code>1234</code>" || c.Form["parse_mode"] != "HTML" {
		t.Errorf("form = %v", c.Form)
	}
	var kb struct {
		Rows [][]map[string]any `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(c.Form["reply_markup"]), &kb); err != nil {
		t.Fatalf("reply_markup %q: %v", c.Form["reply_markup"], err)
	}
	want := [][]map[string]any{
		{{"text": "Copy Bank", "copy_text": map[string]any{"text": "1234"}}},
		{{"text": "View your order", "url": "https://example.com/o/1"}},
	}
	if !reflect.DeepEqual(kb.Rows, want) {
		t.Errorf("keyboard = %v", kb.Rows)
	}
	var rp map[string]any
	_ = json.Unmarshal([]byte(c.Form["reply_parameters"]), &rp)
	if rp["message_id"] != float64(40) || rp["allow_sending_without_reply"] != true {
		t.Errorf("reply_parameters = %v", c.Form["reply_parameters"])
	}

	// No buttons, no quote: neither parameter is sent (an empty keyboard would
	// wipe nothing but is noise).
	if _, err := cb.SendText(t.Context(), customerID, agentlink.CustomerText{HTML: "plain"}); err != nil {
		t.Fatal(err)
	}
	c = f.CallsTo("sendMessage")[1]
	if _, has := c.Form["reply_markup"]; has {
		t.Errorf("reply_markup sent for no buttons: %v", c.Form)
	}
	if _, has := c.Form["reply_parameters"]; has {
		t.Errorf("reply_parameters sent for no quote: %v", c.Form)
	}
}

func TestCustomerBotSendFile(t *testing.T) {
	for _, tc := range []struct {
		kind, method, field string
		caption             bool
	}{
		{"image", "sendPhoto", "photo", true},
		{"video", "sendVideo", "video", true},
		{"voice", "sendVoice", "voice", true},
		{"audio", "sendAudio", "audio", true},
		{"document", "sendDocument", "document", true},
		{"sticker", "sendSticker", "sticker", false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			f := newFakeTG(t)
			cb := newCustomerBot(t, f)
			id, err := cb.SendFile(t.Context(), customerID, agentlink.CustomerFile{
				Kind: tc.kind, Data: []byte("DATA"), Filename: "thing.bin", HTMLCaption: "cap <b>x</b>", ReplyTo: 7,
				Buttons: [][]agentlink.CustomerButton{{{Text: "Copy", CopyText: "v"}}},
			})
			if err != nil || id == 0 {
				t.Fatalf("id=%d err=%v", id, err)
			}
			calls := f.CallsTo(tc.method)
			if len(calls) != 1 {
				t.Fatalf("methods = %v", f.Methods())
			}
			c := calls[0]
			if string(c.Files[tc.field]) != "DATA" || c.FileNames[tc.field] != "thing.bin" || c.Form["chat_id"] != "5550001111" {
				t.Errorf("upload = %v %v %v", c.Files, c.FileNames, c.Form)
			}
			if tc.caption && (c.Form["caption"] != "cap <b>x</b>" || c.Form["parse_mode"] != "HTML") {
				t.Errorf("caption form = %v", c.Form)
			}
			if !strings.Contains(c.Form["reply_markup"], "copy_text") || !strings.Contains(c.Form["reply_parameters"], `"message_id":7`) {
				t.Errorf("markup/quote missing: %v", c.Form)
			}
		})
	}

	f := newFakeTG(t)
	if _, err := newCustomerBot(t, f).SendFile(t.Context(), customerID, agentlink.CustomerFile{Kind: "hologram", Data: []byte("x")}); err == nil {
		t.Error("an unknown kind was sent")
	}
}

// A6: Telegram's 403 and a vanished chat are customer_unreachable, 429 is
// rate_limited and is not retried in here (the staff bot's middleware would
// sleep and retry; the Agent has to hear about it).
func TestCustomerBotSendErrorsAreClassified(t *testing.T) {
	for name, tc := range map[string]struct {
		respond func(tgCall) (int, string)
		want    error
	}{
		"blocked":      {tgError(403, "Forbidden: bot was blocked by the user", ""), agentlink.ErrCustomerBlocked},
		"deactivated":  {tgError(403, "Forbidden: user is deactivated", ""), agentlink.ErrCustomerBlocked},
		"chat gone":    {tgError(400, "Bad Request: chat not found", ""), agentlink.ErrCustomerBlocked},
		"rate limited": {tgError(429, "Too Many Requests: retry after 2", `,"parameters":{"retry_after":2}`), agentlink.ErrCustomerRateLimited},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeTG(t)
			f.respond["sendMessage"] = tc.respond
			f.respond["sendPhoto"] = tc.respond
			cb := newCustomerBot(t, f)
			_, err := cb.SendText(t.Context(), customerID, agentlink.CustomerText{HTML: "hi"})
			if !errors.Is(err, tc.want) {
				t.Errorf("SendText err = %v", err)
			}
			_, err = cb.SendFile(t.Context(), customerID, agentlink.CustomerFile{Kind: "image", Data: []byte("x")})
			if !errors.Is(err, tc.want) {
				t.Errorf("SendFile err = %v", err)
			}
			if n := len(f.CallsTo("sendMessage")); n != 1 {
				t.Errorf("%d sendMessage attempts, want exactly 1 (no retry)", n)
			}
		})
	}

	f := newFakeTG(t)
	f.respond["sendMessage"] = tgError(400, "Bad Request: can't parse entities", "")
	_, err := newCustomerBot(t, f).SendText(t.Context(), customerID, agentlink.CustomerText{HTML: "<b>"})
	if err == nil || errors.Is(err, agentlink.ErrCustomerBlocked) || errors.Is(err, agentlink.ErrCustomerRateLimited) {
		t.Errorf("a plain 400 was classified: %v", err)
	}
}

func TestDownload(t *testing.T) {
	f := newFakeTG(t)
	f.files["doc1"] = []byte("file body")
	cb := newCustomerBot(t, f)

	data, err := cb.Download(t.Context(), "doc1", 9)
	if err != nil || string(data) != "file body" {
		t.Fatalf("data=%q err=%v", data, err)
	}

	// Over the limit by the reported size: no request at all.
	before := len(f.Calls())
	if _, err := cb.Download(t.Context(), "doc1", MaxDownloadBytes+1); !errors.Is(err, ErrTooBig) || len(f.Calls()) != before {
		t.Errorf("err=%v, calls %d -> %d", err, before, len(f.Calls()))
	}

	// Telegram refusing at getFile.
	f.respond["getFile"] = tgError(400, "Bad Request: file is too big", "")
	if _, err := cb.Download(t.Context(), "doc1", 0); !errors.Is(err, ErrTooBig) {
		t.Errorf("err = %v", err)
	}
	delete(f.respond, "getFile")

	// The file URL answering something else (404); the error must not leak the token.
	f.respond["getFile"] = func(tgCall) (int, string) {
		return 200, `{"ok":true,"result":{"file_id":"x","file_path":"files/missing"}}`
	}
	if _, err := cb.Download(t.Context(), "x", 1); err == nil || strings.Contains(err.Error(), "222:CUST") {
		t.Errorf("err = %v", err)
	}
}

// The Hub bot's side: text is escaped (its client sends HTML), posts land in
// the topic, replies quote, and every kind the bridge uses has a method.
func TestHubTopicsPost(t *testing.T) {
	f := newFakeTG(t)
	hub := newTestBot(t, f, "111:HUB")
	hub.UseMiddleware(middlewares.ParseAsHTML)
	withBridgeState(t, nil, hub, -1001234567890)
	h := hubTopics{}

	id, err := h.Post(t.Context(), 1250, TopicPost{Kind: "text", Text: "a < b & c", ReplyTo: 9001})
	if err != nil || id == 0 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	c := f.CallsTo("sendMessage")[0]
	if c.Form["chat_id"] != "-1001234567890" || c.Form["message_thread_id"] != "1250" || c.Form["text"] != "a &lt; b &amp; c" ||
		c.Form["parse_mode"] != "html" || !strings.Contains(c.Form["reply_parameters"], `"message_id":9001`) {
		t.Errorf("form = %v", c.Form)
	}

	for _, tc := range []struct {
		kind, method, field string
	}{
		{"photo", "sendPhoto", "photo"}, {"video", "sendVideo", "video"}, {"animation", "sendAnimation", "animation"},
		{"voice", "sendVoice", "voice"}, {"audio", "sendAudio", "audio"}, {"document", "sendDocument", "document"},
		{"sticker", "sendSticker", "sticker"},
	} {
		if _, err := h.Post(t.Context(), 1250, TopicPost{Kind: tc.kind, Text: "<cap>", Data: []byte("D"), Filename: "f.bin"}); err != nil {
			t.Errorf("%s: %v", tc.kind, err)
			continue
		}
		calls := f.CallsTo(tc.method)
		if len(calls) != 1 || string(calls[0].Files[tc.field]) != "D" || calls[0].Form["message_thread_id"] != "1250" {
			t.Errorf("%s: calls = %+v", tc.kind, calls)
			continue
		}
		if tc.kind != "sticker" && calls[0].Form["caption"] != "&lt;cap&gt;" {
			t.Errorf("%s: caption = %q", tc.kind, calls[0].Form["caption"])
		}
	}

	if _, err := h.Post(t.Context(), 1250, TopicPost{Kind: "location", Lat: 3.1, Lon: 101.6}); err != nil {
		t.Fatal(err)
	}
	if c := f.CallsTo("sendLocation")[0]; c.Form["latitude"] != "3.1" || c.Form["longitude"] != "101.6" || c.Form["message_thread_id"] != "1250" {
		t.Errorf("location form = %v", c.Form)
	}
	if _, err := h.Post(t.Context(), 1250, TopicPost{Kind: "contact", Phone: "+601", First: "Aiman", Last: "Yusof"}); err != nil {
		t.Fatal(err)
	}
	if c := f.CallsTo("sendContact")[0]; c.Form["phone_number"] != "+601" || c.Form["first_name"] != "Aiman" || c.Form["last_name"] != "Yusof" {
		t.Errorf("contact form = %v", c.Form)
	}
	if _, err := h.Post(t.Context(), 1250, TopicPost{Kind: "hologram"}); err == nil {
		t.Error("an unknown kind was posted")
	}

	// Topic creation goes through the Hub bot too.
	thread, err := h.CreateTopic(t.Context(), "Wei Tan (@wei_kl)")
	if err != nil || thread != 4242 {
		t.Fatalf("thread=%d err=%v", thread, err)
	}
	if c := f.CallsTo("createForumTopic")[0]; c.Form["name"] != "Wei Tan (@wei_kl)" || c.Form["chat_id"] != "-1001234567890" {
		t.Errorf("createForumTopic form = %v", c.Form)
	}
}

// A2 + the migration rule: the topic of a customer is a ChatThreadPair keyed
// `tg:<user_id>`, WhatsApp rows are untouched, and migrating a database that
// already holds bridge data (twice) keeps all of it.
func TestThreadsAndMigrationOnAnAlreadyPopulatedDatabase(t *testing.T) {
	db := newTestDB(t)
	withBridgeState(t, db, nil, -1001234567890)
	if err := database.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	// What a live Pi install already holds.
	wa := "60123456789@s.whatsapp.net"
	if err := database.ChatThreadAddNewPair(wa, -1001234567890, 1234); err != nil {
		t.Fatal(err)
	}
	if err := database.MsgIdAddNewPair("3EB0A1", "60123456789@s.whatsapp.net", wa, -1001234567890, 9001, 1234); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	p := NewPairs(db)
	if err := p.Record(customerID, 40, 1250, 9100); err != nil {
		t.Fatal(err)
	}
	if err := p.Record(customerID, 40, 1250, 9100); err != nil { // the same pair twice is one row
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var n int64
	db.Model(&MsgPair{}).Count(&n)
	if n != 1 {
		t.Errorf("%d pair rows, want 1", n)
	}

	th := dbThreads{}
	if _, found, err := th.Find("tg:5550001111"); err != nil || found {
		t.Fatalf("found=%v err=%v before any topic", found, err)
	}
	if err := th.Add("tg:5550001111", 1250); err != nil {
		t.Fatal(err)
	}
	if id, found, err := th.Find("tg:5550001111"); err != nil || !found || id != 1250 {
		t.Errorf("id=%d found=%v err=%v", id, found, err)
	}
	// The row is what the bridge's own lookups see, and the WhatsApp rows are as they were.
	if key, err := database.ChatThreadGetWaFromTg(-1001234567890, 1250); err != nil || key != "tg:5550001111" {
		t.Errorf("topic key = %q err=%v", key, err)
	}
	if id, found, _ := database.ChatThreadGetTgFromWa(wa, -1001234567890); !found || id != 1234 {
		t.Errorf("the WhatsApp thread changed: %d %v", id, found)
	}
	if tg, _, _, err := database.MsgIdGetTgFromWa("3EB0A1", wa); err != nil || tg != -1001234567890 {
		t.Errorf("the WhatsApp message pair changed: %d err=%v", tg, err)
	}
}
