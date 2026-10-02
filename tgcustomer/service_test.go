package tgcustomer

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"watgbridge/agentlink"
	"watgbridge/database"
	"watgbridge/state"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"go.uber.org/zap"
)

func newTestService(t *testing.T, w *world, cust *fakeTG) *Service {
	t.Helper()
	s, err := NewService(Options{
		Token: "222:CUST", APIURL: cust.URL(), DB: newTestDB(t), Topics: w.topics, Threads: w.threads,
		Events: w.events, Log: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func eventually(t *testing.T, what string, cond func() bool) {
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

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}

// A1: enabled, the webhook is deleted before the first poll, pending updates
// are kept, and only messages and edits are asked for.
func TestStartDeletesTheWebhookBeforePolling(t *testing.T) {
	w := newWorld(t)
	cust := newFakeTG(t)
	s := newTestService(t, w, cust)

	// Building the bot logs in and nothing more: no webhook change, no polling yet.
	if got := cust.Methods(); !reflect.DeepEqual(got, []string{"getMe"}) {
		t.Fatalf("before Start, methods = %v", got)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	eventually(t, "the first poll", func() bool { return len(cust.CallsTo("getUpdates")) > 0 })

	methods := cust.Methods()
	del, poll := indexOf(methods, "deleteWebhook"), indexOf(methods, "getUpdates")
	if del < 0 || poll < 0 || del > poll {
		t.Fatalf("methods = %v, want deleteWebhook before getUpdates", methods)
	}
	if got := cust.CallsTo("deleteWebhook")[0].Form["drop_pending_updates"]; got != "" && got != "false" {
		t.Errorf("drop_pending_updates = %q: the customers' waiting messages must be kept", got)
	}
	if got := cust.CallsTo("getUpdates")[0].Form["allowed_updates"]; got != `["message","edited_message"]` {
		t.Errorf("allowed_updates = %q", got)
	}
}

func TestStartFailsLoudlyWhenTheWebhookCannotBeDeleted(t *testing.T) {
	w := newWorld(t)
	cust := newFakeTG(t)
	cust.respond["deleteWebhook"] = tgError(401, "Unauthorized", "")
	s := newTestService(t, w, cust)
	if err := s.Start(); err == nil || !strings.Contains(err.Error(), "webhook") {
		t.Errorf("Start() = %v", err)
	}
	if len(cust.CallsTo("getUpdates")) != 0 {
		t.Error("polled without owning the bot")
	}
}

func TestNewServiceRejectsABadToken(t *testing.T) {
	w := newWorld(t)
	cust := newFakeTG(t)
	cust.respond["getMe"] = tgError(401, "Unauthorized", "")
	if _, err := NewService(Options{Token: "222:CUST", APIURL: cust.URL(), DB: newTestDB(t), Topics: w.topics, Threads: w.threads, Events: w.events}); err == nil {
		t.Error("a bot that cannot log in was accepted")
	}
}

// Updates from Telegram reach the bridge in order, edits are told apart from
// new messages, and a group's message is ignored.
func TestPolledUpdatesReachTheBridgeInOrder(t *testing.T) {
	w := newWorld(t)
	cust := newFakeTG(t)
	s := newTestService(t, w, cust)

	var batch []gotgbot.Update
	for i := int64(1); i <= 6; i++ {
		batch = append(batch, gotgbot.Update{UpdateId: i, Message: textMsg(i, fmt.Sprintf("message %d", i))})
	}
	group := textMsg(50, "in a group")
	group.Chat = gotgbot.Chat{Id: -1001, Type: "supergroup"}
	edit := textMsg(3, "message 3, fixed")
	edit.EditDate = 1790000000
	batch = append(batch, gotgbot.Update{UpdateId: 7, Message: group}, gotgbot.Update{UpdateId: 8, EditedMessage: edit})
	for _, u := range batch {
		cust.Push(u)
	}

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	eventually(t, "seven events", func() bool { return len(w.events.Customers()) == 7 })

	var ids []string
	for _, e := range w.events.Customers() {
		ids = append(ids, e.HubMsgID)
	}
	want := []string{
		"5550001111:1", "5550001111:2", "5550001111:3", "5550001111:4", "5550001111:5", "5550001111:6",
		"5550001111:3:e1790000000",
	}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("event ids = %v\nwant       %v", ids, want)
	}
	if last := w.events.Customers()[6]; last.EditOf != "5550001111:3" || last.Text != "message 3, fixed" {
		t.Errorf("edit event = %+v", last)
	}
	if n := len(w.topics.Created()); n != 1 {
		t.Errorf("%d topics, want 1", n)
	}
}

func TestChannelIsWhatTheAgentLinkNeeds(t *testing.T) {
	w := newWorld(t)
	cust := newFakeTG(t)
	s := newTestService(t, w, cust)
	var _ agentlink.CustomerChannel = s

	id, err := s.SendText(t.Context(), customerID, agentlink.CustomerText{HTML: "hi"})
	if err != nil || id == 0 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if err := s.RecordPair(customerID, id, 1250, 9100); err != nil {
		t.Fatal(err)
	}
	if got := s.HubMsgIDOfTopicMsg(1250, 9100); got != fmt.Sprintf("5550001111:%d", id) {
		t.Errorf("HubMsgIDOfTopicMsg = %q", got)
	}
	if got := s.HubMsgIDOfTopicMsg(1250, 9999); got != "" {
		t.Errorf("an unpaired message gave %q", got)
	}
}

func TestInitWhenDisabledOrMisconfigured(t *testing.T) {
	cfg := state.State.Config
	old := cfg.CustomerBot
	oldToken := cfg.Telegram.BotToken
	t.Cleanup(func() { cfg.CustomerBot = old; cfg.Telegram.BotToken = oldToken })
	current.Store(nil)

	// A1: disabled means no customer bot at all, even with a token configured.
	cfg.CustomerBot.Enabled, cfg.CustomerBot.BotToken = false, "222:CUST"
	if s, err := Init(); s != nil || err != nil || current.Load() != nil {
		t.Errorf("disabled: service=%v err=%v", s, err)
	}

	cfg.CustomerBot.Enabled, cfg.CustomerBot.BotToken = true, ""
	if s, err := Init(); s != nil || err == nil {
		t.Errorf("no token: service=%v err=%v", s, err)
	}
	cfg.Telegram.BotToken, cfg.CustomerBot.BotToken = "111:HUB", "111:HUB"
	if s, err := Init(); s != nil || err == nil || !strings.Contains(err.Error(), "different bot") {
		t.Errorf("the staff bot's token: service=%v err=%v", s, err)
	}
}

/* ------------------------------------------------------ the staff-side hook -- */

type hookWorld struct {
	*world
	hub   *fakeTG
	cust  *fakeTG
	svc   *Service
	hubB  *gotgbot.Bot
	group int64
}

// newHookWorld sets up a database holding a customer's topic and a WhatsApp
// topic, a Hub bot and a customer bot on fake servers, and an installed service.
func newHookWorld(t *testing.T) *hookWorld {
	t.Helper()
	h := &hookWorld{world: newWorld(t), hub: newFakeTG(t), cust: newFakeTG(t), group: -1001234567890}
	db := newTestDB(t)
	h.hubB = newTestBot(t, h.hub, "111:HUB")
	withBridgeState(t, db, h.hubB, h.group)
	if err := database.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	if err := database.ChatThreadAddNewPair("tg:5550001111", h.group, 1250); err != nil {
		t.Fatal(err)
	}
	if err := database.ChatThreadAddNewPair("60123456789@s.whatsapp.net", h.group, 1300); err != nil {
		t.Fatal(err)
	}
	h.svc = newTestService(t, h.world, h.cust)
	current.Store(h.svc)
	t.Cleanup(func() { current.Store(nil) })
	return h
}

func (h *hookWorld) staffCtx(m *gotgbot.Message) *ext.Context {
	m.Chat = gotgbot.Chat{Id: h.group, Type: "supergroup"}
	return ext.NewContext(h.hubB, &gotgbot.Update{UpdateId: 1, Message: m}, nil)
}

// A5: a staff message in a customer's topic is taken by the hook, goes out
// through the customer bot, and is reported. A topic of a WhatsApp chat is not
// the hook's business.
func TestHandleTopicMessageSendsThroughTheCustomerBot(t *testing.T) {
	h := newHookWorld(t)

	m := staffMsg(500)
	m.Text = "Yes, in stock & ready <today>"
	if !HandleTopicMessage(h.hubB, h.staffCtx(m)) {
		t.Fatal("the hook did not take a message of a customer's topic")
	}
	calls := h.cust.CallsTo("sendMessage")
	if len(calls) != 1 || calls[0].Form["chat_id"] != "5550001111" || calls[0].Form["text"] != "Yes, in stock &amp; ready &lt;today&gt;" || calls[0].Form["parse_mode"] != "HTML" {
		t.Fatalf("customer bot calls = %+v", calls)
	}
	if n := len(h.hub.CallsTo("sendMessage")); n != 0 {
		t.Errorf("the Hub bot sent %d messages to somebody", n)
	}
	if evs := h.events.Staffs(); len(evs) != 1 || evs[0].Source != "topic" || !strings.HasPrefix(evs[0].HubMsgID, "5550001111:") {
		t.Errorf("staff events = %+v", evs)
	}

	// A photo-only reply is taken and reported too (it was sent by the customer bot, from the Hub bot's file).
	h.hub.files["ph"] = []byte("PNG")
	h.topics.files["ph"] = []byte("PNG")
	photo := staffMsg(501)
	photo.Photo = []gotgbot.PhotoSize{{FileId: "ph", Width: 10, Height: 10}}
	if !HandleTopicMessage(h.hubB, h.staffCtx(photo)) {
		t.Fatal("photo not taken")
	}
	if up := h.cust.CallsTo("sendPhoto"); len(up) != 1 || string(up[0].Files["photo"]) != "PNG" {
		t.Errorf("sendPhoto calls = %+v", up)
	}
	if evs := h.events.Staffs(); len(evs) != 2 || evs[1].Text != "" {
		t.Errorf("staff events = %+v", evs)
	}

	// A WhatsApp topic is left to the WhatsApp bridge.
	wa := staffMsg(502)
	wa.MessageThreadId = 1300
	wa.Text = "hi"
	if HandleTopicMessage(h.hubB, h.staffCtx(wa)) {
		t.Error("a WhatsApp topic was taken by the customer hook")
	}
	// And so is a message outside any topic.
	general := staffMsg(503)
	general.MessageThreadId = 0
	general.Text = "hi"
	if HandleTopicMessage(h.hubB, h.staffCtx(general)) {
		t.Error("a message outside topics was taken")
	}
}

func TestHandleTopicMessageShowsFailuresInTheTopic(t *testing.T) {
	h := newHookWorld(t)
	h.cust.respond["sendMessage"] = tgError(403, "Forbidden: bot was blocked by the user", "")

	m := staffMsg(500)
	m.Text = "hello?"
	if !HandleTopicMessage(h.hubB, h.staffCtx(m)) {
		t.Fatal("not taken")
	}
	notes := h.hub.CallsTo("sendMessage")
	if len(notes) != 1 || !strings.Contains(notes[0].Form["text"], "Cannot send to the customer") || notes[0].Form["message_thread_id"] != "1250" {
		t.Fatalf("Hub bot messages = %+v", notes)
	}
	if len(h.events.Staffs()) != 0 {
		t.Error("a failed send was reported as staff speech")
	}
}

// With the customer bot off, a topic that belongs to a Telegram customer still
// never reaches WhatsApp: the hook takes it and says why nothing was sent.
func TestHandleTopicMessageWithTheCustomerBotOff(t *testing.T) {
	h := newHookWorld(t)
	current.Store(nil)

	m := staffMsg(500)
	m.Text = "hello?"
	if !HandleTopicMessage(h.hubB, h.staffCtx(m)) {
		t.Fatal("the message of a customer's topic was left for WhatsApp")
	}
	notes := h.hub.CallsTo("sendMessage")
	if len(notes) != 1 || !strings.Contains(notes[0].Form["text"], "customer bot is turned off") {
		t.Errorf("Hub bot messages = %+v", notes)
	}
	if len(h.cust.CallsTo("sendMessage")) != 0 {
		t.Error("something was sent to the customer")
	}
}

func TestHandleTopicMessageIgnoresServiceMessages(t *testing.T) {
	h := newHookWorld(t)
	pin := staffMsg(500)
	pin.PinnedMessage = &gotgbot.Message{MessageId: 9100}
	if !HandleTopicMessage(h.hubB, h.staffCtx(pin)) {
		t.Fatal("a pin in a customer's topic fell through to WhatsApp")
	}
	if len(h.hub.CallsTo("sendMessage")) != 0 || len(h.cust.Calls()) != 1 /* getMe */ {
		t.Errorf("a service message produced traffic: hub %v, customer %v", h.hub.Methods(), h.cust.Methods())
	}
}
