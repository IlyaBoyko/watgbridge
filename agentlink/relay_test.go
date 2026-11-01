package agentlink

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

const (
	relayStaffChat = int64(-1001234567890)
	relayStaffMsg  = int64(5521)
	waConv         = "wa:60123456789@s.whatsapp.net"
	waChatKey      = "60123456789@s.whatsapp.net"
)

func cmdEnv(t *testing.T, w *world, id, typ string, payload any) Envelope {
	t.Helper()
	env, err := NewEnvelope(id, typ, w.clock.Now(), payload)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func expiry(w *world) string { return FormatTS(w.clock.Now().Add(10 * time.Minute)) }

// relayHub is a Hub whose link is not running; up says whether it counts as
// past the handshake.
func relayHub(t *testing.T, w *world, up bool) *Hub {
	t.Helper()
	h := w.hub("ws://127.0.0.1:1/hub")
	h.link.connected.Store(up)
	return h
}

func relayInput(key string) RelayInput {
	return RelayInput{
		ChatKey: key, ChatID: relayStaffChat, TopicID: 1234, MsgID: relayStaffMsg,
		AuthorID: 704338780, AuthorName: "Ilya", IsStaff: true, TextOnly: true,
		Text: "Yes, we ship to Penang.", ReplyTo: "3EB0A1B2C3D4E5F6",
	}
}

func outboxEvents(t *testing.T, h *Hub) []Envelope {
	t.Helper()
	rows, err := h.outbox.Pending(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []Envelope
	for _, r := range rows {
		env, err := DecodeEnvelope([]byte(r.JSON))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, env)
	}
	return out
}

/* ------------------------------------------------------------- set_relay -- */

func TestSetRelayStoresAndSurvivesAReopen(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	set := func(id string, on bool) Result {
		return e.Handle(context.Background(), cmdEnv(t, w, id, TypeSetRelay, SetRelay{Conversation: waConv, On: on, ExpiresAt: expiry(w)}))
	}
	if on, _ := e.Relay.RelayOn(waConv); on {
		t.Fatal("the default must be off")
	}
	if res := set("S1", true); !res.OK {
		t.Fatalf("set_relay on: %+v", res)
	}
	if res := set("S2", true); !res.OK {
		t.Fatalf("set_relay on again must still be ok: %+v", res)
	}
	// A new kit on the same database is a restart.
	again := NewRelayKit(w.db, w.clock, w.log, nil, 0)
	if on, err := again.RelayOn(waConv); err != nil || !on {
		t.Fatalf("after a reopen: on=%v err=%v", on, err)
	}
	if res := set("S3", false); !res.OK {
		t.Fatalf("set_relay off: %+v", res)
	}
	if on, _ := again.RelayOn(waConv); on {
		t.Error("off did not stick")
	}
	if res := e.Handle(context.Background(), cmdEnv(t, w, "S4", TypeSetRelay, SetRelay{Conversation: "bogus", On: true, ExpiresAt: expiry(w)})); res.OK || res.Error != ErrInvalid {
		t.Errorf("a bad conversation = %+v", res)
	}
	if res := e.Handle(context.Background(), cmdEnv(t, w, "S5", TypeSetRelay, SetRelay{Conversation: waConv, On: true, ExpiresAt: FormatTS(w.clock.Now().Add(-time.Minute))})); res.OK || res.Error != ErrExpired {
		t.Errorf("an expired set_relay = %+v", res)
	}
}

func TestMigrateAddsTheTranslationTablesToAPopulatedDatabase(t *testing.T) {
	w := newWorld(t)
	// The database as the previous release left it, with rows in it.
	if err := w.db.AutoMigrate(&OutboxRow{}, &CommandResultRow{}, &CardRow{}); err != nil {
		t.Fatal(err)
	}
	now := w.clock.Now()
	if err := w.db.Create(&OutboxRow{EventID: "old-1", JSON: "{}", CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := w.db.Create(&CardRow{CardID: "draft_1", Conversation: waConv, TgChatID: -1, TgThreadID: 1, TgMsgID: 2, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := Migrate(w.db); err != nil {
			t.Fatalf("migrate #%d: %v", i+1, err)
		}
	}
	var n int64
	w.db.Model(&OutboxRow{}).Count(&n)
	var cards int64
	w.db.Model(&CardRow{}).Count(&cards)
	if n != 1 || cards != 1 {
		t.Errorf("rows lost: outbox=%d cards=%d", n, cards)
	}
	k := NewRelayKit(w.db, w.clock, w.log, nil, 0)
	if err := k.SetRelay(waConv, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := k.RelayOn(waConv); !on {
		t.Error("the new table does not work")
	}
}

/* ------------------------------------------------------ the interceptor -- */

func TestRelayTakesOnlyStaffTextWhenOnAndLinked(t *testing.T) {
	cases := []struct {
		name string
		key  string
		conv string
	}{
		{"wa", waChatKey, waConv},
		{"tg", "tg:555001", "tg:555001"},
		{"lz", "lz:SESSION1", "lz:SESSION1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			h := relayHub(t, w, true)
			if err := h.relay.SetRelay(c.conv, true); err != nil {
				t.Fatal(err)
			}

			// Each of these must go through today's path untouched.
			not := map[string]func(*RelayInput){
				"not staff":   func(in *RelayInput) { in.IsStaff = false },
				"a bot":       func(in *RelayInput) { in.IsBot = true },
				"an edit":     func(in *RelayInput) { in.IsEdit = true },
				"not text":    func(in *RelayInput) { in.TextOnly = false },
				"blank":       func(in *RelayInput) { in.Text = "  " },
				"unmapped":    func(in *RelayInput) { in.ChatKey = "" },
				"a group JID": func(in *RelayInput) { in.ChatKey = "120363@g.us" },
			}
			for name, mutate := range not {
				in := relayInput(c.key)
				mutate(&in)
				if h.RelayTopicMessage(in) {
					t.Errorf("%s: the relay took the post", name)
				}
			}
			if n := len(outboxEvents(t, h)); n != 0 {
				t.Fatalf("%d events queued for posts the relay must not take", n)
			}

			if !h.RelayTopicMessage(relayInput(c.key)) {
				t.Fatal("staff text with relay on and the link up was not taken")
			}
			evs := outboxEvents(t, h)
			if len(evs) != 1 || evs[0].Type != TypeStaffRelay {
				t.Fatalf("events = %+v", evs)
			}
			var sr StaffRelay
			_ = json.Unmarshal(evs[0].Payload, &sr)
			if sr.Conversation != c.conv || sr.RelayID != "-1001234567890:5521" || sr.TopicID != "1234" ||
				sr.Text != "Yes, we ship to Penang." || sr.ReplyTo != "3EB0A1B2C3D4E5F6" ||
				sr.Author != (StaffAuthor{Source: "topic", TgUserID: "704338780", Name: "Ilya"}) {
				t.Errorf("payload = %+v", sr)
			}
			if want := EventID(TypeStaffRelay, c.conv, sr.RelayID); evs[0].ID != want {
				t.Errorf("event id = %s, want %s", evs[0].ID, want)
			}
			if w.timers.Live() != 1 {
				t.Errorf("watchdog timers = %d, want 1", w.timers.Live())
			}
			if len(w.topics.Cards()) != 0 || len(w.wa.Calls()) != 0 || len(w.customer.Sends()) != 0 {
				t.Error("something was posted or delivered")
			}

			// The same post again is the same event.
			h.RelayTopicMessage(relayInput(c.key))
			if n := len(outboxEvents(t, h)); n != 1 {
				t.Errorf("the same post queued %d events", n)
			}

			// With relay off it is today's path.
			if err := h.relay.SetRelay(c.conv, false); err != nil {
				t.Fatal(err)
			}
			if h.RelayTopicMessage(relayInput(c.key)) {
				t.Error("relay off: the post was taken")
			}
		})
	}
}

func TestRelayEventIDIsDeterministicAndAttemptsDiffer(t *testing.T) {
	a1 := RelayEventID(waConv, "-100:5", 1)
	if a1 != EventID(TypeStaffRelay, waConv, "-100:5") || a1 != RelayEventID(waConv, "-100:5", 1) {
		t.Error("attempt 1 must keep the plain derivation")
	}
	a2 := RelayEventID(waConv, "-100:5", 2)
	if a2 == a1 || a2 != EventID(TypeStaffRelay, waConv, "-100:5|2") {
		t.Errorf("attempt 2 = %s", a2)
	}
	if RelayEventID(waConv, "-100:6", 1) == a1 || RelayEventID("wa:1@s.whatsapp.net", "-100:5", 1) == a1 {
		t.Error("the id must depend on conversation and relay id")
	}
}

func TestRelayWithTheLinkDownDeliversNothingAndOffersRetryAndEdit(t *testing.T) {
	w := newWorld(t)
	h := relayHub(t, w, false)
	_ = h.relay.SetRelay(waConv, true)

	if !h.RelayTopicMessage(relayInput(waChatKey)) {
		t.Fatal("the post must be taken: it may not be delivered as written")
	}
	if n := len(outboxEvents(t, h)); n != 0 {
		t.Errorf("%d events with the link down", n)
	}
	if len(w.wa.Calls()) != 0 || len(w.customer.Sends()) != 0 {
		t.Error("something was delivered to the customer")
	}
	if w.timers.Live() != 0 {
		t.Error("a watchdog was started for nothing")
	}
	cards := w.topics.Cards()
	if len(cards) != 1 {
		t.Fatalf("failure posts = %d", len(cards))
	}
	c := cards[0]
	if c.ReplyTo != relayStaffMsg || c.Thread != 1234 || !c.HTML ||
		c.Text != "⚠️ <b>Not sent</b>: the agent is offline, so this wasn't translated." {
		t.Errorf("failure post = %+v", c)
	}
	if len(c.KB) != 1 || len(c.KB[0]) != 2 || c.KB[0][0].Label != "Retry" || c.KB[0][1].Label != "Edit" {
		t.Fatalf("buttons = %+v", c.KB)
	}
	for _, b := range c.KB[0] {
		if IsCardCallback(b.CallbackData) {
			t.Errorf("%q could be mistaken for an Agent card", b.CallbackData)
		}
		if !IsRelayCallback(b.CallbackData) || len(b.CallbackData) > maxCallbackDataBytes {
			t.Errorf("bad callback data %q", b.CallbackData)
		}
	}
	// The post was refused, so an edit of it must not be delivered either.
	if !h.relay.IsRelayPost(relayStaffChat, relayStaffMsg) {
		t.Error("the refused post is not marked")
	}
}

func TestRelayRetryEditAndWhoMayPress(t *testing.T) {
	w := newWorld(t)
	h := relayHub(t, w, false)
	_ = h.relay.SetRelay(waConv, true)
	h.RelayTopicMessage(relayInput(waChatKey))
	retry, edit := w.topics.Cards()[0].KB[0][0].CallbackData, w.topics.Cards()[0].KB[0][1].CallbackData
	failMsg := int64(9001)

	press := func(data string, staff bool) string {
		return h.HandleRelayCallback(CallbackInput{Data: data, Authorized: staff, UserID: 704338780, UserName: "Ilya"})
	}

	if got := press(retry, false); got != CallbackNotAllowed {
		t.Errorf("a non-staff press = %q", got)
	}
	if got := press(retry, true); got != CallbackAgentOffline {
		t.Errorf("retry while down = %q, want %q", got, CallbackAgentOffline)
	}
	if len(outboxEvents(t, h)) != 0 || len(w.topics.Deleted()) != 0 {
		t.Fatal("a refused retry changed something")
	}

	h.link.connected.Store(true)
	if got := press(retry, true); got != "" {
		t.Fatalf("retry while up = %q", got)
	}
	evs := outboxEvents(t, h)
	if len(evs) != 1 || evs[0].Type != TypeStaffRelay {
		t.Fatalf("events = %+v", evs)
	}
	first := EventID(TypeStaffRelay, waConv, "-1001234567890:5521")
	if evs[0].ID == first || evs[0].ID != RelayEventID(waConv, "-1001234567890:5521", 2) {
		t.Errorf("retry event id = %s (attempt 1 would be %s)", evs[0].ID, first)
	}
	var sr StaffRelay
	_ = json.Unmarshal(evs[0].Payload, &sr)
	if sr.Text != "Yes, we ship to Penang." || sr.RelayID != "-1001234567890:5521" || sr.ReplyTo != "3EB0A1B2C3D4E5F6" {
		t.Errorf("retry payload = %+v", sr)
	}
	if d := w.topics.Deleted(); len(d) != 1 || d[0][1] != failMsg {
		t.Errorf("deleted = %v, want the failure post %d", d, failMsg)
	}
	if w.timers.Live() != 1 {
		t.Errorf("watchdog timers after retry = %d", w.timers.Live())
	}
	if got := press(retry, true); got != CallbackExpired {
		t.Errorf("a second press on a used failure post = %q", got)
	}

	// Edit on a fresh failure post.
	h.link.connected.Store(false)
	in := relayInput(waChatKey)
	in.MsgID = 5522
	h.RelayTopicMessage(in)
	cards := w.topics.Cards()
	edit = cards[len(cards)-1].KB[0][1].CallbackData
	if got := press(edit, false); got != CallbackNotAllowed {
		t.Errorf("edit by non-staff = %q", got)
	}
	if got := press(edit, true); got != "" {
		t.Fatalf("edit = %q", got)
	}
	edits := w.topics.Edits()
	if len(edits) != 1 || edits[0].Text != "✏️ <i>Not sent. Write the corrected message in this topic.</i>" || len(edits[0].KB) != 0 {
		t.Errorf("edit = %+v", edits)
	}
	if len(outboxEvents(t, h)) != 1 {
		t.Error("Edit must not emit anything")
	}
}

func TestRelayFailureSurvivesARestart(t *testing.T) {
	w := newWorld(t)
	h := relayHub(t, w, false)
	_ = h.relay.SetRelay(waConv, true)
	h.RelayTopicMessage(relayInput(waChatKey))
	retry := w.topics.Cards()[0].KB[0][0].CallbackData

	// A new Hub on the same database is a restart.
	h2 := relayHub(t, w, true)
	if got := h2.HandleRelayCallback(CallbackInput{Data: retry, Authorized: true}); got != "" {
		t.Fatalf("retry after a restart = %q", got)
	}
	if evs := outboxEvents(t, h2); len(evs) != 1 || evs[0].ID != RelayEventID(waConv, "-1001234567890:5521", 2) {
		t.Errorf("events = %+v", evs)
	}
}

func TestWatchdogFiresOnceAndIsStoppedBySendOrCard(t *testing.T) {
	w := newWorld(t)
	h := relayHub(t, w, true)
	_ = h.relay.SetRelay(waConv, true)
	h.RelayTopicMessage(relayInput(waChatKey))
	if w.timers.Live() != 1 {
		t.Fatal("no watchdog")
	}
	if n := w.timers.Fire(); n != 1 {
		t.Fatalf("fired %d", n)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].ReplyTo != relayStaffMsg || posts[0].Text != "⏳ <i>Not sent yet: waiting for the agent.</i>" || !posts[0].HTML {
		t.Fatalf("reminder = %+v", posts)
	}
	if n := w.timers.Fire(); n != 0 || len(w.topics.Posts()) != 1 {
		t.Error("the reminder came twice")
	}

	// A delivered send with that relay_of stops it.
	w2 := newWorld(t)
	h2 := relayHub(t, w2, true)
	_ = h2.relay.SetRelay(waConv, true)
	h2.RelayTopicMessage(relayInput(waChatKey))
	res := h2.exec.Handle(context.Background(), sendEnv(t, w2, "R1", Send{Kind: "reply", Text: "Ya", RelayOf: "-1001234567890:5521"}))
	if !res.OK {
		t.Fatalf("send: %+v", res)
	}
	if w2.timers.Live() != 0 || w2.timers.Fire() != 0 {
		t.Error("a delivered send did not stop the watchdog")
	}

	// So does the Agent's failure card.
	w3 := newWorld(t)
	h3 := relayHub(t, w3, true)
	_ = h3.relay.SetRelay(waConv, true)
	h3.RelayTopicMessage(relayInput(waChatKey))
	res = h3.exec.Handle(context.Background(), sendEnv(t, w3, "C1", Send{
		Kind: "card", CardID: "relay_1", Title: "⚠️ Not sent", Text: "The translation changed a number.", RelayOf: "-1001234567890:5521",
		Buttons: &Buttons{{{ID: "relay_retry", Label: "Retry"}, {ID: "relay_edit", Label: "Edit"}}},
	}))
	if !res.OK {
		t.Fatalf("card: %+v", res)
	}
	cards := w3.topics.Cards()
	if len(cards) != 1 || cards[0].ReplyTo != relayStaffMsg {
		t.Errorf("failure card = %+v", cards)
	}
	if w3.timers.Live() != 0 {
		t.Error("the failure card did not stop the watchdog")
	}
}

func TestHandleRelayCallbackRefusesJunk(t *testing.T) {
	w := newWorld(t)
	h := relayHub(t, w, true)
	for _, d := range []string{"hr:", "hr:x:r", "hr:0:r", "hr:1:z", "hr:1", "hr:99:r"} {
		if got := h.HandleRelayCallback(CallbackInput{Data: d, Authorized: true}); got != CallbackExpired {
			t.Errorf("%q = %q", d, got)
		}
	}
}

/* ------------------------------------------------------- the relay send -- */

func relayReplyPost(posts []topicPost) (topicPost, bool) {
	for _, p := range posts {
		if p.ReplyTo == relayStaffMsg {
			return p, true
		}
	}
	return topicPost{}, false
}

func TestRelaySendOnWhatsAppMirrorsAsAReplyWithTheGlobe(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	res := e.Handle(context.Background(), sendEnv(t, w, "R1", Send{
		Kind: "reply", Text: "Ya, kami hantar ke Penang.", Translation: "Yes, we ship to Penang.",
		RelayOf: "-1001234567890:5521", Signature: "Sent by Ilya (translated)", ReplyTo: "3EB0A1B2C3D4E5F6",
	}))
	if !res.OK || res.HubMsgID != "WA001" {
		t.Fatalf("result = %+v", res)
	}
	calls := w.wa.Calls()
	if len(calls) != 1 || calls[0].Text != "Ya, kami hantar ke Penang." || calls[0].Quote == nil {
		t.Fatalf("delivered = %+v", calls)
	}
	p, ok := relayReplyPost(w.topics.Posts())
	if !ok {
		t.Fatalf("no mirror replying to the staff post: %+v", w.topics.Posts())
	}
	want := "🌐 Ya, kami hantar ke Penang.\n🌐 <i>Yes, we ship to Penang.</i>\n<i>✓ Sent by Ilya (translated)</i>"
	if !p.HTML || p.Text != want {
		t.Errorf("mirror = %q (html=%v), want %q", p.Text, p.HTML, want)
	}
	if pairs := w.bridge.Pairs(); len(pairs) != 1 || pairs[0].WaMsgID != "WA001" {
		t.Errorf("pairs = %+v", pairs)
	}
	if !e.Guard.Has("WA001") {
		t.Error("the sent id is not guarded, so its echo could become a staff.message")
	}
}

func TestRelaySendWhenTheStaffPostIsGonePostsWithoutTheReplyLink(t *testing.T) {
	w := newWorld(t)
	w.topics.replyGone = true
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "R1", Send{Kind: "reply", Text: "Ya", RelayOf: "-1001234567890:5521"}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].ReplyTo != 0 || !strings.HasPrefix(posts[0].Text, "🌐 Ya") {
		t.Errorf("posts = %+v", posts)
	}
}

func TestRelaySendOnTelegramMirrorsAsAReplyAndPairs(t *testing.T) {
	w := newWorld(t)
	w.bridge.threads["tg:555001"] = 2200
	e := w.executor()
	res := e.Handle(context.Background(), sendEnv(t, w, "R1", Send{
		Conversation: "tg:555001", Kind: "reply", Text: "Ya <b>", Translation: "Yes <b>", RelayOf: "-1001234567890:5521",
	}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	sends := w.customer.Sends()
	if len(sends) != 1 || sends[0].Text.HTML != "Ya &lt;b&gt;" {
		t.Fatalf("delivered = %+v", sends)
	}
	p, ok := relayReplyPost(w.topics.Posts())
	if !ok || p.Text != "🌐 Ya &lt;b&gt;\n🌐 <i>Yes &lt;b&gt;</i>" || p.Thread != 2200 {
		t.Errorf("mirror = %+v (found %v)", p, ok)
	}
	if pairs := w.customer.Pairs(); len(pairs) != 1 {
		t.Errorf("pairs = %+v", pairs)
	}
}

func TestRelaySendOnLazadaMirrorsAsAReply(t *testing.T) {
	w, e, lz := lzExec(t)
	res := e.Handle(context.Background(), lzSend(t, w, "R1", Send{
		Kind: "reply", Text: "Ya", Translation: "Yes", RelayOf: "-1001234567890:5521",
	}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	if calls := lz.Calls(); len(calls) != 1 || calls[0].Text != "Ya" {
		t.Fatalf("delivered = %+v", calls)
	}
	p, ok := relayReplyPost(w.topics.Posts())
	if !ok || p.Text != "🌐 Ya\n🌐 <i>Yes</i>" {
		t.Errorf("mirror = %+v (found %v)", p, ok)
	}
}

func TestRelaySendThatFailsLeavesTheWatchdogRunning(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	e.Relay.startWatchdog("-1001234567890:5521", func() {})
	w.wa.fail = context.DeadlineExceeded
	if res := e.Handle(context.Background(), sendEnv(t, w, "R1", Send{Kind: "reply", Text: "Ya", RelayOf: "-1001234567890:5521"})); res.OK {
		t.Fatal("the send should have failed")
	}
	if w.timers.Live() != 1 {
		t.Error("a send that was not delivered stopped the watchdog")
	}
}

/* ------------------------------------------------------- translation -- */

func TestTranslationOnEachKind(t *testing.T) {
	w := newWorld(t)
	e := w.executor()

	if res := e.Handle(context.Background(), sendEnv(t, w, "N1", Send{Kind: "note", Text: "Slip <ok>", Translation: "Slip ok & done"})); !res.OK {
		t.Fatalf("note: %+v", res)
	}
	if p := w.topics.Posts()[0]; p.Text != "ℹ️ <i>Slip &lt;ok&gt;</i>\n🌐 <i>Slip ok &amp; done</i>" || !p.HTML {
		t.Errorf("note = %+v", p)
	}

	if res := e.Handle(context.Background(), sendEnv(t, w, "C1", Send{
		Kind: "card", CardID: "c1", Title: "Draft", Text: "Boleh <b>", Translation: "Sure <b>",
		Buttons: &Buttons{{{ID: "send", Label: "Send now"}}},
	})); !res.OK {
		t.Fatalf("card: %+v", res)
	}
	if c := w.topics.Cards()[0]; c.Text != "📝 <b>Draft</b>\n<blockquote>Boleh &lt;b&gt;</blockquote>\n🌐 <i>Sure &lt;b&gt;</i>" || len(c.KB) != 1 {
		t.Errorf("card = %+v", c)
	}

	env := cmdEnv(t, w, "E1", TypeEditCard, EditCard{Conversation: waConv, CardID: "c1", Text: "Ya", Title: "Draft", Translation: "Yes", ExpiresAt: expiry(w)})
	if res := e.Handle(context.Background(), env); !res.OK {
		t.Fatalf("edit_card: %+v", res)
	}
	if ed := w.topics.Edits(); len(ed) != 1 || ed[0].Text != "📝 <b>Draft</b>\n<blockquote>Ya</blockquote>\n🌐 <i>Yes</i>" {
		t.Errorf("edit = %+v", ed)
	}

	if res := e.Handle(context.Background(), sendEnv(t, w, "P1", Send{Kind: "reply", Text: "Boleh", Translation: "Sure", Signature: "Sent automatically"})); !res.OK {
		t.Fatalf("reply: %+v", res)
	}
	posts := w.topics.Posts()
	if p := posts[len(posts)-1]; p.Text != "🤖 Boleh\n🌐 <i>Sure</i>\n<i>✓ Sent automatically</i>" {
		t.Errorf("reply mirror = %q", p.Text)
	}
	if c := w.wa.Calls(); strings.Contains(c[0].Text, "Sure") {
		t.Error("the translation reached the customer")
	}
}

func TestTranslationOnATelegramReplyNeverReachesTheCustomer(t *testing.T) {
	w := newWorld(t)
	w.bridge.threads["tg:555001"] = 2200
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "P1", Send{Conversation: "tg:555001", Kind: "reply", Text: "Boleh", Translation: "Sure"}))
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	s := w.customer.Sends()
	if len(s) != 1 || strings.Contains(s[0].Text.HTML, "Sure") {
		t.Errorf("customer got %+v", s)
	}
	if p := w.topics.Posts()[0]; p.Text != "🤖 Boleh\n🌐 <i>Sure</i>" {
		t.Errorf("mirror = %q", p.Text)
	}
}

func TestTranslationIsShortenedNeverTheText(t *testing.T) {
	text := strings.Repeat("a", 100)
	long := strings.Repeat("é & ", 3000)
	for _, limit := range []int{tgTextLimit, tgCaptionLimit} {
		got, ok := renderMirror(mirrorParts{Text: text, Translation: long, Signature: "Loki"}, limit)
		if !ok || tgLen(got.Text) > limit {
			t.Fatalf("limit %d: ok=%v len=%d", limit, ok, tgLen(got.Text))
		}
		if !strings.Contains(got.Text, text) || !strings.Contains(got.Text, "…</i>") || !strings.HasSuffix(got.Text, "<i>✓ Loki</i>") {
			t.Errorf("limit %d: text lost or translation not shortened: %.80q ... %.40q", limit, got.Text, got.Text[len(got.Text)-60:])
		}
		// It keeps as much as fits: one more rune would not.
		if tgLen(got.Text) < limit-12 {
			t.Errorf("limit %d: shortened too much (%d units)", limit, tgLen(got.Text))
		}
	}

	card := renderCardTr("T", text, long)
	if tgLen(card.Text) > tgTextLimit || !strings.Contains(card.Text, text) || !strings.Contains(card.Text, "…</i>") {
		t.Errorf("card: len=%d", tgLen(card.Text))
	}
	note := renderNoteTr(text, long)
	if tgLen(note.Text) > tgTextLimit || !strings.Contains(note.Text, text) || !strings.Contains(note.Text, "…</i>") {
		t.Errorf("note: len=%d", tgLen(note.Text))
	}

	// A text that fills the limit alone leaves no room: the translation goes,
	// the text stays whole.
	full := strings.Repeat("b", tgCaptionLimit-3)
	got, ok := renderMirror(mirrorParts{Text: full, Translation: "Hello there"}, tgCaptionLimit)
	if !ok || !strings.Contains(got.Text, full) || strings.Contains(got.Text, "🌐") {
		t.Errorf("full text: ok=%v translation kept=%v", ok, strings.Contains(got.Text, "🌐"))
	}
}

/* ------------------------------------------------------------- annotate -- */

func annotateEnv(t *testing.T, w *world, id, conv, hubMsgID, text string) Envelope {
	return cmdEnv(t, w, id, TypeAnnotate, Annotate{Conversation: conv, HubMsgID: hubMsgID, Text: text, ExpiresAt: expiry(w)})
}

func TestAnnotateEditsTheHubsOwnPostKeepingItsFormatting(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	w.bridge.setTopicMsg("3EB0A1B2C3D4E5F6", 777, 1234)
	_ = e.Relay.SaveTopicPost(TopicPostRow{TgMsgID: 777, TgChatID: -1001, HTML: "<b>Boleh hantar ke Penang?</b>"})

	env := annotateEnv(t, w, "A1", waConv, "3EB0A1B2C3D4E5F6", "Can you ship to Penang? <3")
	res := e.Handle(context.Background(), env)
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	ed := w.topics.PostEdits()
	want := "<b>Boleh hantar ke Penang?</b>\n🌐 <i>Can you ship to Penang? &lt;3</i>"
	if len(ed) != 1 || ed[0].MsgID != 777 || ed[0].ChatID != -1001 || ed[0].Text != want || !ed[0].HTML || ed[0].Caption {
		t.Fatalf("edit = %+v", ed)
	}
	if len(w.topics.Posts()) != 0 {
		t.Error("an edit must not also post a reply")
	}

	// A repeat of the command id returns the first result and does nothing.
	if again := e.Handle(context.Background(), env); !again.OK || len(w.topics.PostEdits()) != 1 {
		t.Errorf("repeat = %+v, edits = %d", again, len(w.topics.PostEdits()))
	}
	// A second annotation stacks under the first (the stored HTML moved on).
	e.Handle(context.Background(), annotateEnv(t, w, "A2", waConv, "3EB0A1B2C3D4E5F6", "More"))
	if ed := w.topics.PostEdits(); len(ed) != 2 || ed[1].Text != want+"\n🌐 <i>More</i>" {
		t.Errorf("second edit = %+v", ed)
	}
	if len(w.wa.Calls()) != 0 {
		t.Error("the customer was touched")
	}
}

func TestAnnotateRepliesWhenItCannotEdit(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	w.bridge.setTopicMsg("OLD", 700, 1234)
	w.bridge.setTopicMsg("LONG", 701, 1234)
	w.bridge.setTopicMsg("CAP", 702, 1234)
	w.bridge.setTopicMsg("FAILS", 703, 1234)
	_ = e.Relay.SaveTopicPost(TopicPostRow{TgMsgID: 701, TgChatID: -1001, HTML: strings.Repeat("x", tgTextLimit-5)})
	_ = e.Relay.SaveTopicPost(TopicPostRow{TgMsgID: 702, TgChatID: -1001, HTML: strings.Repeat("c", tgCaptionLimit-5), Caption: true})
	_ = e.Relay.SaveTopicPost(TopicPostRow{TgMsgID: 703, TgChatID: -1001, HTML: "ok"})

	for i, id := range []string{"OLD", "LONG", "CAP"} {
		if res := e.Handle(context.Background(), annotateEnv(t, w, "A"+id, waConv, id, "Hello there")); !res.OK {
			t.Fatalf("%s: %+v", id, res)
		}
		posts := w.topics.Posts()
		if len(posts) != i+1 {
			t.Fatalf("%s: posts = %+v", id, posts)
		}
		p := posts[i]
		if p.ReplyTo != int64(700+i) || p.Text != "🌐 <i>Hello there</i>" || !p.HTML || p.Thread != 1234 {
			t.Errorf("%s: reply = %+v", id, p)
		}
	}
	if len(w.topics.PostEdits()) != 0 {
		t.Error("an edit was made where the reply fallback was due")
	}

	// An edit Telegram refuses falls back to a reply as well.
	w.topics.editPostFail = context.DeadlineExceeded
	if res := e.Handle(context.Background(), annotateEnv(t, w, "AF", waConv, "FAILS", "Hi")); !res.OK {
		t.Fatalf("%+v", res)
	}
	if posts := w.topics.Posts(); posts[len(posts)-1].ReplyTo != 703 {
		t.Errorf("no reply after a failed edit: %+v", posts)
	}
}

func TestAnnotateRepliesWithoutTheLinkWhenThePostIsGone(t *testing.T) {
	w := newWorld(t)
	w.topics.replyGone = true
	w.bridge.setTopicMsg("OLD", 700, 1234)
	if res := w.executor().Handle(context.Background(), annotateEnv(t, w, "A1", waConv, "OLD", "Hi")); !res.OK {
		t.Fatalf("%+v", res)
	}
	if posts := w.topics.Posts(); len(posts) != 1 || posts[0].ReplyTo != 0 {
		t.Errorf("posts = %+v", posts)
	}
}

func TestAnnotateUnknownMessageIsInvalid(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	w.bridge.setTopicMsg("OTHER-CHAT", 700, 9999) // a post of another topic
	for _, id := range []string{"NOPE", "OTHER-CHAT"} {
		res := e.Handle(context.Background(), annotateEnv(t, w, "A-"+id, waConv, id, "Hi"))
		if res.OK || res.Error != ErrInvalid {
			t.Errorf("%s: %+v", id, res)
		}
	}
	if res := e.Handle(context.Background(), annotateEnv(t, w, "A-bad", "wa:1@s.whatsapp.net", "X", "Hi")); res.OK || res.Error != ErrUnknownConversation {
		t.Errorf("unknown conversation: %+v", res)
	}
	if len(w.topics.Posts()) != 0 || len(w.topics.PostEdits()) != 0 {
		t.Error("something was posted for an unknown message")
	}
}

type finderCustomer struct {
	*fakeCustomer
	finderMap
}

type finderLazada struct {
	*lzSender
	finderMap
}

func TestAnnotateFindsTelegramAndLazadaPosts(t *testing.T) {
	w := newWorld(t)
	w.bridge.threads["tg:555001"] = 2200
	w.bridge.threads[lzConv] = 1300
	e := w.executor()
	e.Customer = finderCustomer{w.customer, finderMap{"tg:555001|555001:42": 810}}
	e.Lazada = finderLazada{&lzSender{}, finderMap{lzConv + "|MSG9": 820}}
	_ = e.Relay.SaveTopicPost(TopicPostRow{TgMsgID: 810, TgChatID: -1001, HTML: "<b>Hola</b>"})

	if res := e.Handle(context.Background(), annotateEnv(t, w, "T1", "tg:555001", "555001:42", "Hello")); !res.OK {
		t.Fatalf("tg: %+v", res)
	}
	if ed := w.topics.PostEdits(); len(ed) != 1 || ed[0].MsgID != 810 || ed[0].Text != "<b>Hola</b>\n🌐 <i>Hello</i>" {
		t.Errorf("tg edit = %+v", ed)
	}
	if res := e.Handle(context.Background(), annotateEnv(t, w, "L1", lzConv, "MSG9", "Hello")); !res.OK {
		t.Fatalf("lz: %+v", res)
	}
	if posts := w.topics.Posts(); len(posts) != 1 || posts[0].ReplyTo != 820 || posts[0].Thread != 1300 {
		t.Errorf("lz reply = %+v", posts)
	}
	if res := e.Handle(context.Background(), annotateEnv(t, w, "T2", "tg:555001", "555001:43", "Hello")); res.OK || res.Error != ErrInvalid {
		t.Errorf("unknown tg message: %+v", res)
	}
	// A channel that cannot say where its posts are answers invalid, not a crash.
	e.Customer, e.Lazada = w.customer, &lzSender{}
	if res := e.Handle(context.Background(), annotateEnv(t, w, "T3", "tg:555001", "555001:42", "Hello")); res.OK || res.Error != ErrInvalid {
		t.Errorf("no finder: %+v", res)
	}
}

func TestRecordTopicPostKeepsBoldAndSkipsKeyboards(t *testing.T) {
	w := newWorld(t)
	h := relayHub(t, w, true)
	old := current.Swap(h)
	t.Cleanup(func() { current.Store(old) })

	RecordTopicPost(&gotgbot.Message{
		MessageId: 5, Chat: gotgbot.Chat{Id: -1001}, Text: "👤 Aiman:\nBoleh <hantar>?",
		Entities: []gotgbot.MessageEntity{{Type: "bold", Offset: 10, Length: 15}},
	})
	row, ok, _ := h.relay.topicPost(5)
	if !ok || row.Caption || row.TgChatID != -1001 || row.HTML != "👤 Aiman:\n<b>Boleh &lt;hantar&gt;?</b>" {
		t.Errorf("row = %+v ok=%v", row, ok)
	}

	RecordTopicPost(&gotgbot.Message{MessageId: 6, Chat: gotgbot.Chat{Id: -1001}, Caption: "pic", CaptionEntities: []gotgbot.MessageEntity{{Type: "bold", Offset: 0, Length: 3}}})
	if row, ok, _ := h.relay.topicPost(6); !ok || !row.Caption || row.HTML != "<b>pic</b>" {
		t.Errorf("caption row = %+v ok=%v", row, ok)
	}

	RecordTopicPost(&gotgbot.Message{MessageId: 7, Chat: gotgbot.Chat{Id: -1001}, Text: "has buttons", ReplyMarkup: &gotgbot.InlineKeyboardMarkup{}})
	RecordTopicPost(&gotgbot.Message{MessageId: 8, Chat: gotgbot.Chat{Id: -1001}})
	for _, id := range []int64{7, 8} {
		if _, ok, _ := h.relay.topicPost(id); ok {
			t.Errorf("message %d should not be stored", id)
		}
	}
}

func TestIsRelayedPostForEdits(t *testing.T) {
	w := newWorld(t)
	h := relayHub(t, w, true)
	old := current.Swap(h)
	t.Cleanup(func() { current.Store(old) })
	_ = h.relay.SetRelay(waConv, true)
	h.RelayTopicMessage(relayInput(waChatKey))
	if !IsRelayedPost(relayStaffChat, relayStaffMsg) {
		t.Error("an edit of a relayed post must be recognised")
	}
	if IsRelayedPost(relayStaffChat, relayStaffMsg+1) {
		t.Error("another post was taken for a relayed one")
	}
}

func TestMenuHasAITranslate(t *testing.T) {
	for _, c := range MenuCommands() {
		if c.Command == "ai_translate" {
			if c.Description != "Translation for this chat: on|off" {
				t.Errorf("description = %q", c.Description)
			}
			return
		}
	}
	t.Fatal("ai_translate is not in the menu")
}

func TestAICommandsStillBecomeControlWithRelayOn(t *testing.T) {
	w := newWorld(t)
	h := relayHub(t, w, true)
	_ = h.relay.SetRelay(waConv, true)
	intercepted, _ := h.InterceptAI(ControlInput{Text: "/ai_status", ChatKey: waChatKey, TopicID: 1234, AuthorID: 7, AuthorName: "Ilya"})
	if !intercepted {
		t.Fatal("not intercepted")
	}
	evs := outboxEvents(t, h)
	if len(evs) != 1 || evs[0].Type != TypeControl {
		t.Errorf("events = %+v", evs)
	}
}

func TestRelaySwitchLookupFailureRefusesThePostAndRetryRechecks(t *testing.T) {
	w := newWorld(t)
	h := relayHub(t, w, true)
	if err := w.db.Migrator().DropTable(&RelaySettingRow{}); err != nil { // the store now fails
		t.Fatal(err)
	}
	if !h.RelayTopicMessage(relayInput(waChatKey)) {
		t.Fatal("a post must be refused, not delivered, when the switch cannot be read")
	}
	if len(outboxEvents(t, h)) != 0 || len(w.wa.Calls()) != 0 {
		t.Fatal("something was emitted or delivered")
	}
	cards := w.topics.Cards()
	if len(cards) != 1 || cards[0].ReplyTo != relayStaffMsg || len(cards[0].KB) != 1 || len(cards[0].KB[0]) != 2 {
		t.Fatalf("failure post = %+v", cards)
	}
	retry := cards[0].KB[0][0].CallbackData
	press := func() string { return h.HandleRelayCallback(CallbackInput{Data: retry, Authorized: true}) }

	if got := press(); got != CallbackTryAgain {
		t.Errorf("retry with the store still failing = %q", got)
	}
	if len(w.topics.Deleted()) != 0 || len(w.wa.Calls()) != 0 {
		t.Fatal("a failed retry changed something")
	}

	// The store is back and the switch is off: the text goes out as written.
	if err := w.db.AutoMigrate(&RelaySettingRow{}); err != nil {
		t.Fatal(err)
	}
	if got := press(); got != "" {
		t.Fatalf("retry with relay off = %q", got)
	}
	calls := w.wa.Calls()
	if len(calls) != 1 || calls[0].Text != "Yes, we ship to Penang." || calls[0].Quote == nil || calls[0].Quote.StanzaID != "3EB0A1B2C3D4E5F6" {
		t.Fatalf("delivered = %+v", calls)
	}
	if d := w.topics.Deleted(); len(d) != 1 {
		t.Errorf("failure post not deleted: %v", d)
	}
	evs := outboxEvents(t, h)
	if len(evs) != 1 || evs[0].Type != TypeStaffMessage {
		t.Fatalf("events = %+v", evs)
	}
	if got := press(); got != CallbackExpired {
		t.Errorf("second press = %q", got)
	}

	// With the switch on and the link up, Retry emits the next attempt.
	w2 := newWorld(t)
	h2 := relayHub(t, w2, true)
	_ = w2.db.Migrator().DropTable(&RelaySettingRow{})
	h2.RelayTopicMessage(relayInput(waChatKey))
	retry2 := w2.topics.Cards()[0].KB[0][0].CallbackData
	_ = w2.db.AutoMigrate(&RelaySettingRow{})
	_ = h2.relay.SetRelay(waConv, true)
	if got := h2.HandleRelayCallback(CallbackInput{Data: retry2, Authorized: true}); got != "" {
		t.Fatalf("retry with relay on = %q", got)
	}
	if evs := outboxEvents(t, h2); len(evs) != 1 || evs[0].ID != RelayEventID(waConv, "-1001234567890:5521", 2) {
		t.Errorf("events = %+v", evs)
	}
	if len(w2.wa.Calls()) != 0 {
		t.Error("delivered as written although relay is on")
	}
}
