package agentlink

import (
	"encoding/json"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

// A6: parsing of /ai_* messages.
func TestParseAICommand(t *testing.T) {
	cases := []struct {
		text, command, args string
		ok                  bool
	}{
		{"/ai_once he wants Penang shipping time", "ai_once", "he wants Penang shipping time", true},
		{"/ai_after 15", "ai_after", "15", true},
		{"/ai_on@ClarusHubBot", "ai_on", "", true},
		{"/ai_on@ClarusHubBot  now please ", "ai_on", "now please", true},
		{"/ai_off", "ai_off", "", true},
		{"/ai_once\nsecond line stays\nin the args", "ai_once", "second line stays\nin the args", true},
		{"/AI_Once Loud", "ai_once", "Loud", true},
		{"/ai_", "ai_", "", true}, // still an /ai_ message: never forwarded
		{"/aim something", "", "", false},
		{"/ai something", "", "", false},
		{"ai_once text", "", "", false},
		{"hello /ai_once", "", "", false},
		{"", "", "", false},
		{"/start", "", "", false},
	}
	for _, c := range cases {
		cmd, args, ok := ParseAICommand(c.text)
		if cmd != c.command || args != c.args || ok != c.ok {
			t.Errorf("ParseAICommand(%q) = (%q, %q, %v), want (%q, %q, %v)", c.text, cmd, args, ok, c.command, c.args, c.ok)
		}
	}
}

func queuedEvents(t *testing.T, h *Hub) []Envelope {
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
		if _, err := DecodePayload(env); err != nil {
			t.Fatalf("queued event does not validate: %v", err)
		}
		out = append(out, env)
	}
	return out
}

// A6: interception happens with the link disabled, and nothing is written.
func TestInterceptAIWithLinkDisabled(t *testing.T) {
	w := newWorld(t)
	h, err := NewHub(Config{Enabled: false}, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"/ai_once hello", "/ai_on@ClarusHubBot", "/ai_after 15"} {
		intercepted, reply := h.InterceptAI(ControlInput{Text: text, ChatKey: "60123456789@s.whatsapp.net", TopicID: 1234, AuthorID: 7, AuthorName: "Ilya"})
		if !intercepted || reply != UnavailableLine {
			t.Errorf("%q: intercepted=%v reply=%q", text, intercepted, reply)
		}
	}
	if intercepted, reply := h.InterceptAI(ControlInput{Text: "hello there"}); intercepted || reply != "" {
		t.Errorf("plain text was intercepted (%v, %q)", intercepted, reply)
	}

	// A nil Hub (Start never ran) behaves as a disabled one.
	var none *Hub
	if intercepted, reply := none.InterceptAI(ControlInput{Text: "/ai_once x"}); !intercepted || reply != UnavailableLine {
		t.Errorf("nil hub: intercepted=%v reply=%q", intercepted, reply)
	}
	if w.db.Migrator().HasTable(&OutboxRow{}) {
		t.Error("a disabled link must not even create its tables")
	}
}

func TestInterceptAIEmitsControl(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")

	intercepted, reply := h.InterceptAI(ControlInput{
		Text: "/ai_once@ClarusHubBot he wants Penang shipping time", ChatKey: "60123456789@s.whatsapp.net",
		TopicID: 1234, TargetWAMsgID: "3EB0A1B2C3D4E5F6", AuthorID: 704338780, AuthorName: "Ilya",
	})
	if !intercepted || reply != "" {
		t.Fatalf("intercepted=%v reply=%q", intercepted, reply)
	}
	evs := queuedEvents(t, h)
	if len(evs) != 1 || evs[0].Type != TypeControl {
		t.Fatalf("queued: %+v", evs)
	}
	var c Control
	_ = json.Unmarshal(evs[0].Payload, &c)
	want := Control{
		Conversation: "wa:60123456789@s.whatsapp.net", TopicID: "1234", Command: "ai_once",
		Args: "he wants Penang shipping time", TargetHubMsgID: "3EB0A1B2C3D4E5F6",
		Author: TgAuthor{TgUserID: "704338780", Name: "Ilya"},
	}
	if c != want {
		t.Errorf("control = %+v\nwant     %+v", c, want)
	}
	if len(evs[0].ID) != 26 {
		t.Errorf("control id %q should be a ULID", evs[0].ID)
	}
}

func TestInterceptAINotOneToOne(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")
	for name, key := range map[string]string{
		"unmapped topic": "",
		"group":          "120363025246125486@g.us",
		"status":         "status@broadcast",
		"self":           "60199999999@s.whatsapp.net",
	} {
		intercepted, reply := h.InterceptAI(ControlInput{Text: "/ai_once x", ChatKey: key, TopicID: 5})
		if !intercepted || reply != UnavailableLine {
			t.Errorf("%s: intercepted=%v reply=%q", name, intercepted, reply)
		}
	}
	intercepted, reply := h.InterceptAI(ControlInput{Text: "/ai_x2 nope", ChatKey: "60123456789@s.whatsapp.net"})
	if !intercepted || reply == "" {
		t.Errorf("a command the protocol cannot carry must still be held back and answered (%v, %q)", intercepted, reply)
	}
	if n := len(queuedEvents(t, h)); n != 0 {
		t.Errorf("%d events queued, want 0", n)
	}
}

// A9: with the link disabled the event hooks do nothing at all.
func TestDisabledHubWritesNothing(t *testing.T) {
	w := newWorld(t)
	h, err := NewHub(Config{Enabled: false}, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	if h.Enabled() {
		t.Fatal("Enabled() is true")
	}
	if err := h.EmitCustomerMessage(CustomerInput{ChatKey: "60123456789@s.whatsapp.net", HubMsgID: "A", Text: "hi"}); err != nil {
		t.Error(err)
	}
	if err := h.EmitStaffMessage(StaffInput{ChatKey: "60123456789@s.whatsapp.net", HubMsgID: "B", Source: "phone", Text: "hi"}); err != nil {
		t.Error(err)
	}
	if w.db.Migrator().HasTable(&OutboxRow{}) || w.db.Migrator().HasTable(&CommandResultRow{}) {
		t.Error("tables were created for a disabled link")
	}
	h.Start(t.Context()) // must not dial or panic
	h.Wait(time.Second)
}

// The whatsapp hook returns before touching any bridge state when the link is
// off (state.State has no clients in tests, so touching it would panic).
func TestWhatsAppHookIsInertWhenDisabled(t *testing.T) {
	w := newWorld(t)
	h, _ := NewHub(Config{Enabled: false}, w.deps())
	current.Store(h)
	t.Cleanup(func() { current.Store(nil) })

	OnWhatsAppMessage(&events.Message{}, "hello", false)
	OnTopicMessage(nil)
	if Enabled() {
		t.Error("Enabled() is true")
	}
}

func TestNewHubNeedsConnectionSettings(t *testing.T) {
	w := newWorld(t)
	if _, err := NewHub(Config{Enabled: true, URL: "ws://x"}, w.deps()); err == nil {
		t.Error("enabled link without token and hub_id was accepted")
	}
}

func TestEmitCustomerMessage(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")

	in := CustomerInput{
		ChatKey: "60123456789@s.whatsapp.net", TopicID: 1234, HubMsgID: "3EB0A1B2C3D4E5F6", ContactName: "Aiman (60123456789)",
		Text: "boleh ship ke Penang tak?", ReplyTo: "3EB0AAAA", EditOf: "3EB0BBBB",
		Media: []Media{{Kind: "image", Mime: "image/png", Size: 3, Data: "AAEC"}},
	}
	if err := h.EmitCustomerMessage(in); err != nil {
		t.Fatal(err)
	}
	// Reported twice (WhatsApp redelivery): still one queued event.
	if err := h.EmitCustomerMessage(in); err != nil {
		t.Fatal(err)
	}
	evs := queuedEvents(t, h)
	if len(evs) != 1 {
		t.Fatalf("%d events queued, want 1", len(evs))
	}
	if want := EventID(TypeCustomerMessage, "wa:60123456789@s.whatsapp.net", "3EB0A1B2C3D4E5F6"); evs[0].ID != want {
		t.Errorf("id = %q, want %q", evs[0].ID, want)
	}
	var m CustomerMessage
	_ = json.Unmarshal(evs[0].Payload, &m)
	if m.TopicID != "1234" || m.Contact.Phone != "+60123456789" || m.Contact.Name != "Aiman (60123456789)" ||
		m.ReplyTo != "3EB0AAAA" || m.EditOf != "3EB0BBBB" || len(m.Media) != 1 || m.Text != in.Text {
		t.Errorf("payload = %+v", m)
	}

	// A LID with no known number carries no phone; groups and the self chat give nothing.
	if err := h.EmitCustomerMessage(CustomerInput{ChatKey: "98765432101@lid", HubMsgID: "L1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"120363025246125486@g.us", "status@broadcast", "60199999999@s.whatsapp.net"} {
		if err := h.EmitCustomerMessage(CustomerInput{ChatKey: key, HubMsgID: "G1", Text: "hi"}); err == nil {
			t.Errorf("%s produced an event", key)
		}
	}
	// A reaction-like message (no text, no media) produces nothing.
	if err := h.EmitCustomerMessage(CustomerInput{ChatKey: "60123456789@s.whatsapp.net", HubMsgID: "R1"}); err != nil {
		t.Fatal(err)
	}
	evs = queuedEvents(t, h)
	if len(evs) != 2 {
		t.Fatalf("%d events queued, want 2", len(evs))
	}
	var lid CustomerMessage
	_ = json.Unmarshal(evs[1].Payload, &lid)
	if lid.Contact.Phone != "" || lid.Conversation != "wa:98765432101@lid" {
		t.Errorf("lid payload = %+v", lid)
	}
}

func TestEmitStaffMessage(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")

	if err := h.EmitStaffMessage(StaffInput{ChatKey: "60123456789@s.whatsapp.net", TopicID: 0, HubMsgID: "3EB0FFEE00112233", Source: "phone", Text: "Boleh boss"}); err != nil {
		t.Fatal(err)
	}
	if err := h.EmitStaffMessage(StaffInput{
		ChatKey: "60123456789@s.whatsapp.net", TopicID: 1234, HubMsgID: "3EB0FFEE00112244", Source: "topic",
		TgUserID: 704338780, Name: "Ilya", Text: "Order confirmed", ReplyTo: "3EB0A1B2C3D4E5F6",
	}); err != nil {
		t.Fatal(err)
	}

	// A message the Hub sent for the Agent must never come back as staff speech.
	h.guard.Mark("WA777")
	if err := h.EmitStaffMessage(StaffInput{ChatKey: "60123456789@s.whatsapp.net", HubMsgID: "WA777", Source: "phone", Text: "echo"}); err != nil {
		t.Fatal(err)
	}

	evs := queuedEvents(t, h)
	if len(evs) != 2 {
		t.Fatalf("%d events queued, want 2 (the Agent's own message must be skipped)", len(evs))
	}
	var phone, topic StaffMessage
	_ = json.Unmarshal(evs[0].Payload, &phone)
	_ = json.Unmarshal(evs[1].Payload, &topic)
	if phone.TopicID != "0" || phone.Author != (StaffAuthor{Source: "phone"}) || len(phone.Media) != 0 {
		t.Errorf("phone = %+v", phone)
	}
	if topic.Author != (StaffAuthor{Source: "topic", TgUserID: "704338780", Name: "Ilya"}) || topic.ReplyTo != "3EB0A1B2C3D4E5F6" {
		t.Errorf("topic = %+v", topic)
	}
}

// A staff photo sent from the topic reaches the customer but carries no text
// and no downloaded media. It is still a human answer, so it must be reported
// (it cancels an /ai_after draft). An empty message from the phone is a
// reaction or protocol stub, and is not.
func TestEmptyStaffMessageReportedFromTopicNotFromPhone(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")

	if err := h.EmitStaffMessage(StaffInput{ChatKey: "60123456789@s.whatsapp.net", TopicID: 1234, HubMsgID: "WA-PHOTO", Source: "topic", TgUserID: 704338780, Name: "Ilya"}); err != nil {
		t.Fatal(err)
	}
	if err := h.EmitStaffMessage(StaffInput{ChatKey: "60123456789@s.whatsapp.net", HubMsgID: "WA-REACTION", Source: "phone"}); err != nil {
		t.Fatal(err)
	}

	evs := queuedEvents(t, h)
	if len(evs) != 1 {
		t.Fatalf("%d events queued, want 1 (the topic photo only)", len(evs))
	}
	var got StaffMessage
	_ = json.Unmarshal(evs[0].Payload, &got)
	if got.HubMsgID != "WA-PHOTO" || got.Author.Source != "topic" || got.Text != "" || len(got.Media) != 0 {
		t.Errorf("event = %+v", got)
	}
}
