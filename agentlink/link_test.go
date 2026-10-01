package agentlink

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// startLink runs the hub's link against the fake agent and returns a stop func
// and a channel carrying Run's result.
func startLink(t *testing.T, h *Hub) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- h.link.Run(ctx)
		close(done) // a second receive (the cleanup below) must not block
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("link did not stop")
		}
	})
	return done, cancel
}

func emitCustomer(t *testing.T, h *Hub, msgID, text string) {
	t.Helper()
	if err := h.EmitCustomerMessage(CustomerInput{ChatKey: "60123456789@s.whatsapp.net", TopicID: 1234, HubMsgID: msgID, ContactName: "Aiman", Text: text}); err != nil {
		t.Fatal(err)
	}
}

func textOf(t *testing.T, env Envelope) string {
	t.Helper()
	var m CustomerMessage
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		t.Fatal(err)
	}
	return m.Text
}

// A2: hello first, nothing else before welcome, then the outbox in order.
func TestHandshakeThenReplayInOrder(t *testing.T) {
	w := newWorld(t)
	gotAll := make(chan []string, 1)
	a := newFakeAgent(t, func(s *agentSession) {
		s.startReader()
		hello := s.expectType(TypeHello)
		var hp Hello
		_ = json.Unmarshal(hello.Payload, &hp)
		if hp.HubID != "pi-test" || hp.Protocol != 1 || hp.HubVersion != "test" || len(hp.Channels) != 2 {
			t.Errorf("hello = %+v", hp)
		}
		if s.auth != "Bearer secret-token" {
			t.Errorf("Authorization = %q", s.auth)
		}
		// The Hub must send nothing else before welcome.
		if env, err := s.next(300 * time.Millisecond); err == nil {
			t.Errorf("hub sent %q before welcome", env.Type)
		}
		s.welcome()
		var texts []string
		for i := 0; i < 3; i++ {
			texts = append(texts, textOf(t, s.expectType(TypeCustomerMessage)))
		}
		gotAll <- texts
	})
	h := w.hub(a.URL())
	// The outbox already holds events from before this start.
	emitCustomer(t, h, "M1", "first")
	emitCustomer(t, h, "M2", "second")
	emitCustomer(t, h, "M3", "third")

	startLink(t, h)
	select {
	case texts := <-gotAll:
		if len(texts) != 3 || texts[0] != "first" || texts[1] != "second" || texts[2] != "third" {
			t.Errorf("replay order = %v", texts)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("outbox was not replayed after welcome")
	}
}

// A2: close 4001 stops reconnecting and tells the owner exactly once.
func TestCloseCodes4001And4003StopAndNotifyOnce(t *testing.T) {
	for _, code := range []websocket.StatusCode{4001, 4003} {
		code := code
		t.Run(code.String(), func(t *testing.T) {
			w := newWorld(t)
			a := newFakeAgent(t, func(s *agentSession) {
				s.startReader()
				s.expectType(TypeHello)
				_ = s.conn.Close(code, "expected protocol 1, got 2")
			})
			h := w.hub(a.URL())
			done, _ := startLink(t, h)

			select {
			case err := <-done:
				var stop *StopError
				if !errors.As(err, &stop) || stop.Code != int(code) || stop.Reason != "expected protocol 1, got 2" || !stop.Notify {
					t.Fatalf("Run returned %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("link kept running after a fatal close code")
			}
			time.Sleep(150 * time.Millisecond) // several backoff periods
			if n := a.Connections(); n != 1 {
				t.Errorf("%d connections, want 1 (no reconnect)", n)
			}
			texts := w.notifier.Texts()
			if len(texts) != 1 {
				t.Fatalf("owner notified %d times, want exactly 1: %v", len(texts), texts)
			}
		})
	}
}

func TestCloseCode4003AfterWelcomeAlsoStops(t *testing.T) {
	w := newWorld(t)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		time.Sleep(50 * time.Millisecond)
		_ = s.conn.Close(4003, "token revoked")
	})
	h := w.hub(a.URL())
	done, _ := startLink(t, h)
	select {
	case err := <-done:
		var stop *StopError
		if !errors.As(err, &stop) || stop.Code != 4003 {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
	if n := len(w.notifier.Texts()); n != 1 {
		t.Errorf("owner notified %d times, want 1", n)
	}
}

// Close code 4009: stop, log, no owner DM.
func TestCloseCode4009StopsWithoutNotifying(t *testing.T) {
	w := newWorld(t)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		_ = s.conn.Close(4009, "replaced")
	})
	h := w.hub(a.URL())
	done, _ := startLink(t, h)
	select {
	case err := <-done:
		var stop *StopError
		if !errors.As(err, &stop) || stop.Code != 4009 || stop.Notify {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
	time.Sleep(100 * time.Millisecond)
	if a.Connections() != 1 {
		t.Errorf("%d connections, want 1", a.Connections())
	}
	if n := len(w.notifier.Texts()); n != 0 {
		t.Errorf("owner notified %d times for 4009, want 0", n)
	}
}

// Any other close reconnects, and an unacked event is sent again.
func TestReconnectsAndResendsUnackedEvents(t *testing.T) {
	w := newWorld(t)
	seen := make(chan string, 16)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		switch s.n {
		case 1:
			seen <- "1:" + textOf(t, s.expectType(TypeCustomerMessage)) // never acked
			_ = s.conn.Close(websocket.StatusGoingAway, "restart")
		case 2:
			first := s.expectType(TypeCustomerMessage)
			second := s.expectType(TypeCustomerMessage)
			seen <- "2:" + textOf(t, first)
			seen <- "2:" + textOf(t, second)
			s.write("ACK1", TypeAck, Ack{ID: first.ID})
		}
	})
	h := w.hub(a.URL())
	emitCustomer(t, h, "M1", "first")
	startLink(t, h)

	expect := func(want string) {
		t.Helper()
		select {
		case got := <-seen:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	expect("1:first")
	// Queued while disconnected or on the new connection: arrives after the replay, in order.
	emitCustomer(t, h, "M2", "second")
	expect("2:first")
	expect("2:second")

	// A3: deleted on ack; the other row stays until it is acked.
	waitFor(t, "ack to delete the first row", func() bool {
		rows, _ := h.outbox.Pending(0, 10)
		return len(rows) == 1 && rows[0].EventID == EventID(TypeCustomerMessage, "wa:60123456789@s.whatsapp.net", "M2")
	})
}

// A3: events are persisted before they are sent, with no connection at all.
func TestEventsPersistBeforeAnyConnection(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://127.0.0.1:1/hub") // nothing listens there
	emitCustomer(t, h, "M1", "offline one")
	emitCustomer(t, h, "M2", "offline two")
	if n, _ := h.outbox.Count(); n != 2 {
		t.Fatalf("outbox holds %d rows, want 2", n)
	}
}

// A3: strict order across many events, replayed and live.
func TestOrderIsKeptAcrossReplayAndLive(t *testing.T) {
	w := newWorld(t)
	got := make(chan string, 64)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		for {
			env, err := s.next(5 * time.Second)
			if err != nil {
				return
			}
			got <- textOf(t, env)
			s.write("ACK-"+env.ID, TypeAck, Ack{ID: env.ID})
		}
	})
	h := w.hub(a.URL())
	for i := 0; i < 10; i++ {
		emitCustomer(t, h, "R"+string(rune('a'+i)), string(rune('a'+i)))
	}
	startLink(t, h)
	for i := 10; i < 20; i++ {
		emitCustomer(t, h, "R"+string(rune('a'+i)), string(rune('a'+i)))
	}
	for i := 0; i < 20; i++ {
		select {
		case g := <-got:
			if g != string(rune('a'+i)) {
				t.Fatalf("event %d arrived as %q", i, g)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d never arrived", i)
		}
	}
	waitFor(t, "all acks to empty the outbox", func() bool { n, _ := h.outbox.Count(); return n == 0 })
}

// Whole path: an Agent command executes once, gets its result, and a retry with
// the same id returns the stored result without sending again.
func TestCommandRoundTripAndRetry(t *testing.T) {
	w := newWorld(t)
	results := make(chan Result, 8)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		send := func(id string) {
			s.write(id, TypeSend, Send{
				Conversation: "wa:60123456789@s.whatsapp.net", Kind: "reply", Text: "Boleh!", Media: []Media{},
				ExpiresAt: FormatTS(time.Now().Add(time.Hour)),
			})
		}
		send("CMD-A")
		send("CMD-A") // the Agent retries
		for i := 0; i < 2; i++ {
			env := s.expectType(TypeResult)
			var r Result
			_ = json.Unmarshal(env.Payload, &r)
			results <- r
		}
	})
	// The command's expires_at uses the real clock here, so give the hub one.
	d := w.deps()
	d.Clock = realClock{}
	h, err := NewHub(Config{Enabled: true, URL: a.URL(), Token: "secret-token", HubID: "pi-test", OutboxMaxAgeDays: 7}, d)
	if err != nil {
		t.Fatal(err)
	}
	startLink(t, h)

	r1, r2 := <-results, <-results
	if !r1.OK || r1.CommandID != "CMD-A" || r1.HubMsgID != "WA001" || r1 != r2 {
		t.Fatalf("results %+v and %+v", r1, r2)
	}
	if n := len(w.wa.Calls()); n != 1 {
		t.Errorf("WhatsApp was called %d times, want 1", n)
	}
}

func TestInvalidFramesGetAnErrorAndTheConnectionStays(t *testing.T) {
	w := newWorld(t)
	type report struct {
		errs   []ProtocolError
		result Result
	}
	reported := make(chan report, 1)
	a := newFakeAgent(t, func(s *agentSession) {
		var errs []ProtocolError
		var result Result
		s.handshake()
		// Not JSON.
		if err := s.conn.Write(s.ctx, websocket.MessageText, []byte("garbage")); err != nil {
			t.Error(err)
		}
		// Unknown type.
		s.write("U1", "presence", map[string]string{})
		// A type that exists but is never sent to a Hub.
		s.write("U2", TypeCustomerMessage, map[string]string{})
		// A send that violates the schema (no expires_at).
		s.writeEnv(Envelope{V: 1, ID: "BADSEND", Type: TypeSend, TS: FormatTS(time.Now()), Payload: []byte(`{"conversation":"wa:60123456789@s.whatsapp.net","kind":"note","media":[]}`)})

		for len(errs) < 4 || result.CommandID == "" {
			env := s.mustNext()
			switch env.Type {
			case TypeError:
				var pe ProtocolError
				_ = json.Unmarshal(env.Payload, &pe)
				errs = append(errs, pe)
			case TypeResult:
				_ = json.Unmarshal(env.Payload, &result)
			default:
				t.Errorf("unexpected frame %q", env.Type)
			}
		}
		reported <- report{errs, result}
		// The connection is still usable: a valid command still works.
		s.write("OK1", TypeSend, Send{Conversation: "wa:60123456789@s.whatsapp.net", Kind: "note", Text: "still here", Media: []Media{}, ExpiresAt: FormatTS(time.Now().Add(time.Hour))})
	})
	d := w.deps()
	d.Clock = realClock{}
	h, _ := NewHub(Config{Enabled: true, URL: a.URL(), Token: "secret-token", HubID: "pi-test", OutboxMaxAgeDays: 7}, d)
	startLink(t, h)

	waitFor(t, "the valid command after the bad frames", func() bool { return len(w.topics.Posts()) == 1 })
	if a.Connections() != 1 {
		t.Errorf("connection was dropped: %d connections", a.Connections())
	}
	rep := <-reported
	codes := map[string]string{}
	for _, e := range rep.errs {
		codes[e.RefID] = e.Code
	}
	if codes["U1"] != "unknown_type" || codes["U2"] != "unexpected_type" || codes["BADSEND"] != "schema" || codes[""] != "schema" {
		t.Errorf("error frames = %+v", rep.errs)
	}
	if rep.result.CommandID != "BADSEND" || rep.result.Error != ErrInvalid {
		t.Errorf("the schema-violating command's result = %+v", rep.result)
	}
}

// 90 s of silence kills the connection (scaled down): a peer that never answers
// pings is dropped and the Hub reconnects.
func TestSilentPeerIsDroppedAndHubReconnects(t *testing.T) {
	w := newWorld(t)
	a := newFakeAgent(t, func(s *agentSession) {
		if s.n == 1 {
			// Complete the handshake but never read again, so pings go unanswered.
			_, _, _ = s.conn.Read(s.ctx) // hello
			s.welcome()
			return
		}
		s.handshake()
	})
	d := w.deps()
	d.Link.PingInterval = 30 * time.Millisecond
	d.Link.DeadAfter = 150 * time.Millisecond
	h, _ := NewHub(Config{Enabled: true, URL: a.URL(), Token: "secret-token", HubID: "pi-test"}, d)
	startLink(t, h)
	waitFor(t, "a second connection after the silent one", func() bool { return a.Connections() >= 2 })
}

func TestControlEventTravelsOverTheLink(t *testing.T) {
	w := newWorld(t)
	got := make(chan Control, 1)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		env := s.expectType(TypeControl)
		var c Control
		_ = json.Unmarshal(env.Payload, &c)
		got <- c
	})
	h := w.hub(a.URL())
	startLink(t, h)
	h.InterceptAI(ControlInput{Text: "/ai_after 15", ChatKey: "60123456789@s.whatsapp.net", TopicID: 1234, AuthorID: 7, AuthorName: "Ilya"})
	select {
	case c := <-got:
		if c.Command != "ai_after" || c.Args != "15" {
			t.Errorf("control = %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control event never arrived")
	}
}
