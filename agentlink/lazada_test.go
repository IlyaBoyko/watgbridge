package agentlink

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"
)

const lzConv = "lz:100094063_2_1011822749_1_103"

type fakeLazada struct {
	mu    sync.Mutex
	polls []string
}

func (f *fakeLazada) Poll(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls = append(f.polls, sessionID)
}

func (f *fakeLazada) Polls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.polls...)
}

func TestLzChatKeys(t *testing.T) {
	if got := LzChatKey("100094063_2_1011822749_1_103"); got != lzConv {
		t.Errorf("LzChatKey = %q", got)
	}
	for in, want := range map[string]string{lzConv: "100094063_2_1011822749_1_103", "lz:a": "a", "lz:a:b": "a:b"} {
		if got, ok := ParseLzChatKey(in); !ok || got != want {
			t.Errorf("ParseLzChatKey(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "lz:", "lz", "tg:5550001111", "wa:60123456789@s.whatsapp.net", "60123456789@s.whatsapp.net", "LZ:abc", " lz:abc"} {
		if got, ok := ParseLzChatKey(in); ok {
			t.Errorf("ParseLzChatKey(%q) = %q, want no", in, got)
		}
	}
}

// A Lazada session's key is its own conversation id, like a Telegram customer's.
func TestLazadaKeyIsItsOwnConversation(t *testing.T) {
	got, err := ConversationFor(lzConv, nil)
	if err != nil || got != lzConv {
		t.Errorf("ConversationFor = %q, %v", got, err)
	}
	if _, err := ConversationFor("lz:", nil); err == nil {
		t.Error("an empty session id is a conversation")
	}
	if _, err := ChatKeyFor(lzConv, nil); err == nil {
		t.Error("an lz: conversation is not a WhatsApp chat")
	}
}

func lzWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.bridge.threads[lzConv] = 1300
	return w
}

// note, card and edit_card only touch the topic, so they work for Lazada now.
func TestNoteCardAndEditCardReachALazadaTopic(t *testing.T) {
	w := lzWorld(t)
	e := w.executor()
	ctx := context.Background()

	res := e.Handle(ctx, sendEnv(t, w, "N1", Send{Conversation: lzConv, Kind: "note", Text: "Draft ready"}))
	if !res.OK || res.Error != "" {
		t.Fatalf("note: %+v", res)
	}
	if posts := w.topics.Posts(); len(posts) != 1 || posts[0].Thread != 1300 {
		t.Errorf("note posts = %+v", posts)
	}

	res = e.Handle(ctx, cardEnv(t, w, "C1", Send{Conversation: lzConv, Text: "Draft", CardID: "draft_lz", Buttons: &draftButtons}))
	if !res.OK || res.Error != "" {
		t.Fatalf("card: %+v", res)
	}
	if cards := w.topics.Cards(); len(cards) != 1 || cards[0].Thread != 1300 {
		t.Errorf("cards = %+v", cards)
	}

	res = e.Handle(ctx, editEnv(t, w, "E1", EditCard{Conversation: lzConv, CardID: "draft_lz", Text: "Draft v2"}))
	if !res.OK || res.Error != "" {
		t.Fatalf("edit_card: %+v", res)
	}
	if edits := w.topics.Edits(); len(edits) != 1 || edits[0].Text != "Draft v2" {
		t.Errorf("edits = %+v", edits)
	}

	if n := len(w.wa.Calls()); n != 0 {
		t.Errorf("%d WhatsApp sends for a Lazada conversation", n)
	}
	if n := len(w.customer.Sends()); n != 0 {
		t.Errorf("%d Telegram customer sends for a Lazada conversation", n)
	}
}

// Sending to Lazada is not there yet: a reply says so with unknown_conversation
// and is neither sent nor mirrored, and never taken for a WhatsApp chat.
func TestReplyToALazadaConversationIsUnknownConversation(t *testing.T) {
	w := lzWorld(t)
	e := w.executor()
	for i, p := range []Send{
		{Kind: "reply", Text: "hello"},
		{Kind: "reply", Text: "hello", ReplyTo: "m1"},
		{Kind: "reply", Media: []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}}},
	} {
		p.Conversation = lzConv
		res := e.Handle(context.Background(), sendEnv(t, w, "R"+string(rune('1'+i)), p))
		if res.OK || res.Error != ErrUnknownConversation {
			t.Errorf("case %d: result = %+v", i, res)
		}
	}
	if len(w.wa.Calls()) != 0 || len(w.customer.Sends()) != 0 || len(w.topics.Posts()) != 0 || len(w.bridge.Pairs()) != 0 {
		t.Error("a reply to Lazada sent, posted or recorded something")
	}
}

// What the Lazada channel hands to EmitCustomerMessage and EmitStaffMessage
// goes out with the conversation, the Lazada message id as hub_msg_id, and an
// event id derived from them, so a re-fetched message is the same event.
func TestLazadaEventsCarryConversationAndDeterministicIDs(t *testing.T) {
	w := newWorld(t)
	got := make(chan []Envelope, 1)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		var frames []Envelope
		for i := 0; i < 2; i++ {
			frames = append(frames, s.mustNext())
		}
		got <- frames
		if env, err := s.next(300 * time.Millisecond); err == nil {
			t.Errorf("a repeated message went out twice: %s", env.Payload)
		}
	})
	h := w.hub(a.URL())
	in := CustomerInput{ChatKey: lzConv, TopicID: 1300, HubMsgID: "1011822749103", ContactName: "Aiman", Text: "hi"}
	for i := 0; i < 2; i++ { // the second is the same message fetched again
		if err := h.EmitCustomerMessage(in); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.EmitStaffMessage(StaffInput{ChatKey: lzConv, TopicID: 1300, HubMsgID: "1011822749104", Source: "phone", Text: "ok"}); err != nil {
		t.Fatal(err)
	}
	startLink(t, h)

	frames := <-got
	cust, staff := frames[0], frames[1]
	if cust.Type != TypeCustomerMessage || cust.ID != EventID(TypeCustomerMessage, lzConv, "1011822749103") {
		t.Errorf("customer frame = %s %s", cust.Type, cust.ID)
	}
	var cm CustomerMessage
	_ = json.Unmarshal(cust.Payload, &cm)
	if cm.Conversation != lzConv || cm.HubMsgID != "1011822749103" || cm.TopicID != "1300" || cm.Contact.Name != "Aiman" ||
		cm.Contact.Phone != "" || cm.Contact.Username != "" || cm.ReplyTo != "" {
		t.Errorf("customer payload = %+v", cm)
	}
	if staff.Type != TypeStaffMessage || staff.ID != EventID(TypeStaffMessage, lzConv, "1011822749104") {
		t.Errorf("staff frame = %s %s", staff.Type, staff.ID)
	}
	var sm StaffMessage
	_ = json.Unmarshal(staff.Payload, &sm)
	if sm.Conversation != lzConv || sm.Author.Source != "phone" || sm.HubMsgID != "1011822749104" {
		t.Errorf("staff payload = %+v", sm)
	}
}

// /ai_ commands typed in a Lazada topic reach the Agent as control events.
func TestAICommandInALazadaTopicBecomesAControlEvent(t *testing.T) {
	w := newWorld(t)
	got := make(chan Envelope, 1)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		got <- s.expectType(TypeControl)
	})
	h := w.hub(a.URL())
	intercepted, reply := h.InterceptAI(ControlInput{Text: "/ai_status", ChatKey: lzConv, TopicID: 1300, AuthorID: 7, AuthorName: "Ilya"})
	if !intercepted || reply != "" {
		t.Fatalf("intercepted=%v reply=%q", intercepted, reply)
	}
	startLink(t, h)
	var c Control
	env := <-got
	_ = json.Unmarshal(env.Payload, &c)
	if c.Conversation != lzConv || c.Command != "ai_status" || c.TopicID != "1300" {
		t.Errorf("control = %+v", c)
	}
}

/* ---------------------------------------------------------- the protocol -- */

func lazadaHub(t *testing.T, w *world, url string, lz LazadaChannel) *Hub {
	t.Helper()
	d := w.deps()
	d.Lazada = lz
	h, err := NewHub(Config{Enabled: true, URL: url, Token: "secret-token", HubID: "pi-test", OutboxMaxAgeDays: 7}, d)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHelloDeclaresLazadaOnlyWhenTheChannelIsOn(t *testing.T) {
	for _, on := range []bool{false, true} {
		w := newWorld(t)
		got := make(chan Hello, 1)
		a := newFakeAgent(t, func(s *agentSession) {
			var hp Hello
			_ = json.Unmarshal(s.handshake().Payload, &hp)
			got <- hp
		})
		var lz LazadaChannel
		if on {
			lz = &fakeLazada{}
		}
		startLink(t, lazadaHub(t, w, a.URL(), lz))
		hp := <-got
		wantCh, wantFeat := []string{"wa", "tg"}, []string{"presence"}
		if on {
			wantCh, wantFeat = []string{"wa", "tg", "lz"}, []string{"presence", "lz_poll"}
		}
		if !reflect.DeepEqual(hp.Channels, wantCh) || !reflect.DeepEqual(hp.Features, wantFeat) {
			t.Errorf("lazada on=%v: channels %v features %v, want %v %v", on, hp.Channels, hp.Features, wantCh, wantFeat)
		}
	}
}

// lz_poll is not a command: the channel is told, no result comes back, and a
// bad payload gets an error frame. The only frame the Agent sees is that error.
func TestLzPollFramesAreHandedOnWithoutAResult(t *testing.T) {
	w := newWorld(t)
	lz := &fakeLazada{}
	seen := make(chan []Envelope, 1)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		s.write("LZ1", TypeLzPoll, LzPoll{SessionID: "100094063_2_1011822749_1_103"})
		s.write("LZ2", TypeLzPoll, LzPoll{})
		s.writeEnv(Envelope{V: 1, ID: "LZ3", Type: TypeLzPoll, TS: "2026-10-28T09:30:12.345Z", Payload: json.RawMessage(`"not an object"`)})
		s.write("LZ4", TypeLzPoll, LzPoll{SessionID: "s2"})
		var frames []Envelope
		frames = append(frames, s.mustNext())
		seen <- frames
		if env, err := s.next(300 * time.Millisecond); err == nil {
			t.Errorf("unexpected extra frame %q: %s", env.Type, env.Payload)
		}
	})
	startLink(t, lazadaHub(t, w, a.URL(), lz))

	frames := <-seen
	if frames[0].Type != TypeError {
		t.Fatalf("frame = %q, want the error for the bad payload", frames[0].Type)
	}
	var pe ProtocolError
	_ = json.Unmarshal(frames[0].Payload, &pe)
	if pe.RefID != "LZ3" || pe.Code != "schema" {
		t.Errorf("error = %+v", pe)
	}
	waitFor(t, "the polls", func() bool { return len(lz.Polls()) >= 4 })
	// The first is the poll of every session after the handshake.
	want := []string{"", "100094063_2_1011822749_1_103", "", "s2"}
	if got := lz.Polls(); !reflect.DeepEqual(got, want) {
		t.Errorf("polls = %q, want %q", got, want)
	}
}

// With the channel off an lz_poll is ignored quietly, and commands after it
// are unaffected.
func TestLzPollIsIgnoredWhenTheChannelIsOff(t *testing.T) {
	w := newWorld(t)
	w.bridge.threads[lzConv] = 1300
	frames := make(chan []Envelope, 1)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		s.write("LZ1", TypeLzPoll, LzPoll{SessionID: "s1"})
		s.write("NOTE1", TypeSend, Send{Conversation: "wa:60123456789@s.whatsapp.net", Kind: "note", Text: "after", Media: []Media{}, ExpiresAt: FormatTS(time.Now().Add(time.Minute))})
		frames <- []Envelope{s.mustNext()}
	})
	d := w.deps()
	d.Clock = realClock{}
	h, err := NewHub(Config{Enabled: true, URL: a.URL(), Token: "secret-token", HubID: "pi-test", OutboxMaxAgeDays: 7}, d)
	if err != nil {
		t.Fatal(err)
	}
	startLink(t, h)
	if first := (<-frames)[0]; first.Type != TypeResult {
		t.Errorf("first frame = %q, want the note's result (nothing for the lz_poll)", first.Type)
	}
}

// Every successful handshake, the first and each reconnect, polls every session:
// pushes are not queued.
func TestEveryHandshakePollsAllSessions(t *testing.T) {
	w := newWorld(t)
	lz := &fakeLazada{}
	var n int
	var mu sync.Mutex
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			time.Sleep(50 * time.Millisecond)
			_ = s.conn.Close(1011, "going away")
		}
	})
	startLink(t, lazadaHub(t, w, a.URL(), lz))
	waitFor(t, "two polls", func() bool { return len(lz.Polls()) >= 2 })
	for i, p := range lz.Polls() {
		if p != "" {
			t.Errorf("poll %d was for session %q, want all sessions", i, p)
		}
	}
	if a.Connections() < 2 {
		t.Errorf("%d connections", a.Connections())
	}
}

func TestLazadaMenuEntryOnlyWhenEnabled(t *testing.T) {
	base := MenuCommands()[:2]
	if got := WithLazadaMenu(base, false); len(got) != 2 {
		t.Errorf("disabled: %v", got)
	}
	got := WithLazadaMenu(base, true)
	if len(got) != 3 || got[2].Command != "lazada_auth" || got[2].Description != "Owner: connect Lazada chat (/lazada_auth <code>)" {
		t.Errorf("enabled: %v", got)
	}
	if !telegramCommandName.MatchString(got[2].Command) || len([]rune(got[2].Description)) > 256 {
		t.Errorf("not valid for Telegram: %+v", got[2])
	}
	// It is the bridge's own command: the /ai_ interceptor must not take it.
	if _, _, ok := ParseAICommand("/lazada_auth abc"); ok {
		t.Error("/lazada_auth was taken for an /ai_ command")
	}
	if len(base) != 2 || len(MenuCommands()) != 8 {
		t.Error("the base list or the agent menu was changed")
	}
}
