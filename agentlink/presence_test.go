package agentlink

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

const (
	presWA  = "wa:60123456789@s.whatsapp.net"
	presLID = "wa:98765432101234@lid"
	presTG  = "tg:5550001111"
)

type waPresenceCall struct {
	To        string
	Composing bool
}

type fakeWAPresence struct {
	mu    sync.Mutex
	calls []waPresenceCall
	fail  error
}

func (f *fakeWAPresence) SetComposing(_ context.Context, to string, composing bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, waPresenceCall{to, composing})
	return f.fail
}

func (f *fakeWAPresence) Calls() []waPresenceCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]waPresenceCall(nil), f.calls...)
}

func (f *fakeWAPresence) composing() int {
	n := 0
	for _, c := range f.Calls() {
		if c.Composing {
			n++
		}
	}
	return n
}

type fakeTGPresence struct {
	mu    sync.Mutex
	chats []int64
	fail  error
}

func (f *fakeTGPresence) SendTyping(_ context.Context, chatID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chats = append(f.chats, chatID)
	return f.fail
}

func (f *fakeTGPresence) Chats() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.chats...)
}

type presenceWorld struct {
	*world
	waP *fakeWAPresence
	tgP *fakeTGPresence
	k   *PresenceKeeper
}

// newPresenceWorld has short ticks (the fake clock only decides deadlines) and
// a topic for each of the conversations above.
func newPresenceWorld(t *testing.T) *presenceWorld {
	t.Helper()
	w := newWorld(t)
	w.bridge.threads["98765432101234@lid"] = 1300
	w.bridge.threads[presTG] = 1250
	p := &presenceWorld{world: w, waP: &fakeWAPresence{}, tgP: &fakeTGPresence{}}
	p.k = NewPresenceKeeper(PresenceOptions{
		WA: p.waP, TG: p.tgP, WATick: 5 * time.Millisecond, TGTick: 5 * time.Millisecond,
		Bridge: w.bridge, Clock: w.clock, Log: w.log,
	})
	t.Cleanup(p.k.StopAll)
	return p
}

func (p *presenceWorld) typing(conv string, d time.Duration) {
	p.k.HandlePresence(context.Background(), Presence{Conversation: conv, State: PresenceTyping, ExpiresAt: FormatTS(p.clock.Now().Add(d))})
}

func (p *presenceWorld) paused(conv string) {
	p.k.HandlePresence(context.Background(), Presence{Conversation: conv, State: PresencePaused, ExpiresAt: FormatTS(p.clock.Now().Add(time.Minute))})
}

func (p *presenceWorld) running(conv string) bool {
	_, ok := p.k.Deadline(conv)
	return ok
}

// total counts every call to either channel.
func (p *presenceWorld) total() int { return len(p.waP.Calls()) + len(p.tgP.Chats()) }

// settle lets a stopped loop's ticks drain, then checks nothing more is sent.
func settle(t *testing.T, count func() int) {
	t.Helper()
	time.Sleep(30 * time.Millisecond)
	before := count()
	time.Sleep(40 * time.Millisecond)
	if after := count(); after != before {
		t.Fatalf("calls kept coming after the end: %d -> %d", before, after)
	}
}

func TestTypingRefreshesUntilTheDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		conv  string
		count func(*presenceWorld) int
		first func(*presenceWorld) bool
	}{
		"whatsapp": {presWA, func(p *presenceWorld) int { return p.waP.composing() }, func(p *presenceWorld) bool {
			c := p.waP.Calls()
			return len(c) > 0 && c[0] == waPresenceCall{"60123456789@s.whatsapp.net", true}
		}},
		"telegram": {presTG, func(p *presenceWorld) int { return len(p.tgP.Chats()) }, func(p *presenceWorld) bool {
			c := p.tgP.Chats()
			return len(c) > 0 && c[0] == 5550001111
		}},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPresenceWorld(t)
			p.typing(tc.conv, time.Minute)
			waitFor(t, "the first indicator", func() bool { return tc.count(p) >= 1 })
			if !tc.first(p) {
				t.Errorf("first call went to the wrong place: %v %v", p.waP.Calls(), p.tgP.Chats())
			}
			waitFor(t, "refreshes", func() bool { return tc.count(p) >= 4 })

			p.clock.Advance(2 * time.Minute) // past expires_at
			waitFor(t, "the loop to end", func() bool { return !p.running(tc.conv) })
			settle(t, p.total)
		})
	}
}

func TestSecondTypingExtendsTheDeadline(t *testing.T) {
	p := newPresenceWorld(t)
	p.typing(presTG, 10*time.Second)
	waitFor(t, "the first indicator", func() bool { return len(p.tgP.Chats()) >= 1 })
	p.typing(presTG, 60*time.Second)
	if d, ok := p.k.Deadline(presTG); !ok || !d.Equal(p.clock.Now().Add(60*time.Second)) {
		t.Fatalf("deadline = %v (running %v), want now+60s", d, ok)
	}

	p.clock.Advance(30 * time.Second) // past the first deadline only
	n := len(p.tgP.Chats())
	waitFor(t, "refreshes to continue", func() bool { return len(p.tgP.Chats()) >= n+3 })
	if !p.running(presTG) {
		t.Fatal("the loop ended at the old deadline")
	}
	p.clock.Advance(31 * time.Second)
	waitFor(t, "the loop to end at the new deadline", func() bool { return !p.running(presTG) })
}

func TestPausedStopsTheLoopAndWhatsAppGetsPaused(t *testing.T) {
	p := newPresenceWorld(t)
	p.typing(presWA, time.Minute)
	waitFor(t, "typing", func() bool { return p.waP.composing() >= 1 })

	p.paused(presWA)
	if p.running(presWA) {
		t.Fatal("loop still running after paused")
	}
	calls := p.waP.Calls()
	if last := calls[len(calls)-1]; last != (waPresenceCall{"60123456789@s.whatsapp.net", false}) {
		t.Errorf("last call = %+v, want paused", last)
	}
	settle(t, p.total)
}

func TestPausedOnTelegramSendsNothingAndStops(t *testing.T) {
	p := newPresenceWorld(t)
	p.typing(presTG, time.Minute)
	waitFor(t, "typing", func() bool { return len(p.tgP.Chats()) >= 1 })
	p.paused(presTG)
	if p.running(presTG) {
		t.Fatal("loop still running after paused")
	}
	settle(t, p.total)
	if len(p.waP.Calls()) != 0 {
		t.Errorf("WhatsApp was touched: %v", p.waP.Calls())
	}
}

func TestWhatsAppIsToldAtTheDeadline(t *testing.T) {
	p := newPresenceWorld(t)
	p.typing(presWA, time.Minute)
	waitFor(t, "typing", func() bool { return p.waP.composing() >= 1 })
	p.clock.Advance(2 * time.Minute)
	waitFor(t, "the loop to end", func() bool { return !p.running(presWA) })
	waitFor(t, "paused at the end", func() bool {
		c := p.waP.Calls()
		return !c[len(c)-1].Composing
	})
}

func TestDeadlineIsCappedAtThreeMinutes(t *testing.T) {
	p := newPresenceWorld(t)
	p.typing(presWA, time.Hour)
	d, ok := p.k.Deadline(presWA)
	if !ok || !d.Equal(p.clock.Now().Add(3*time.Minute)) {
		t.Fatalf("deadline = %v, want now+3m", d)
	}
	// Extending is capped too.
	p.typing(presWA, 24*time.Hour)
	if d, _ := p.k.Deadline(presWA); !d.Equal(p.clock.Now().Add(3 * time.Minute)) {
		t.Fatalf("extended deadline = %v, want now+3m", d)
	}
	// A shorter expires_at wins over the cap.
	p.typing(presWA, 20*time.Second)
	if d, _ := p.k.Deadline(presWA); !d.Equal(p.clock.Now().Add(20 * time.Second)) {
		t.Fatalf("short deadline = %v, want now+20s", d)
	}
}

func TestTypingThatIsAlreadyOverShowsNothing(t *testing.T) {
	p := newPresenceWorld(t)
	p.typing(presWA, -time.Second)
	time.Sleep(30 * time.Millisecond)
	if p.total() != 0 || p.running(presWA) {
		t.Fatalf("an expired typing was shown: %v", p.waP.Calls())
	}
}

func TestWhatsAppLidAndTelegramIdsResolveLikeSends(t *testing.T) {
	p := newPresenceWorld(t)
	p.typing(presLID, time.Minute)
	waitFor(t, "lid typing", func() bool { return p.waP.composing() >= 1 })
	if c := p.waP.Calls()[0]; c.To != "98765432101234@lid" {
		t.Errorf("a @lid chat went to %q", c.To)
	}
	// Telegram ids can exceed 2^53 and stay exact.
	const big = "tg:9007199254740993"
	p.bridge.threads[big] = 1400
	p.typing(big, time.Minute)
	waitFor(t, "big telegram id", func() bool {
		for _, c := range p.tgP.Chats() {
			if c == 9007199254740993 {
				return true
			}
		}
		return false
	})
}

func TestUnknownConversationsAreIgnored(t *testing.T) {
	p := newPresenceWorld(t)
	for name, conv := range map[string]string{
		"no topic":         "wa:60111111111@s.whatsapp.net",
		"lz":               "lz:60123456789",
		"own chat":         "wa:60199999999@s.whatsapp.net",
		"group":            "wa:120363000000000000@g.us",
		"tg without topic": "tg:5550009999",
		"not a chat":       "wa:nonsense",
	} {
		p.typing(conv, time.Minute)
		p.paused(conv)
		if p.running(conv) {
			t.Errorf("%s: started a loop", name)
		}
	}
	time.Sleep(30 * time.Millisecond)
	if p.total() != 0 {
		t.Errorf("channels were called: %v %v", p.waP.Calls(), p.tgP.Chats())
	}
}

func TestChannelThatIsOffIgnoresItsConversations(t *testing.T) {
	w := newWorld(t)
	w.bridge.threads[presTG] = 1250
	k := NewPresenceKeeper(PresenceOptions{WA: &fakeWAPresence{}, Bridge: w.bridge, Clock: w.clock, Log: w.log})
	defer k.StopAll()
	k.HandlePresence(context.Background(), Presence{Conversation: presTG, State: PresenceTyping, ExpiresAt: FormatTS(w.clock.Now().Add(time.Minute))})
	if _, ok := k.Deadline(presTG); ok {
		t.Fatal("a Telegram loop started with the customer bot off")
	}
}

func TestFailingChannelKeepsTheLoopUntilTheDeadline(t *testing.T) {
	p := newPresenceWorld(t)
	p.waP.fail = errors.New("not connected")
	p.typing(presWA, time.Minute)
	waitFor(t, "several attempts despite the errors", func() bool { return len(p.waP.Calls()) >= 4 })
	if !p.running(presWA) {
		t.Fatal("the loop gave up on an error")
	}
}

func TestShutdownStopsEveryLoop(t *testing.T) {
	p := newPresenceWorld(t)
	p.typing(presWA, time.Minute)
	p.typing(presTG, time.Minute)
	waitFor(t, "both running", func() bool { return p.waP.composing() >= 1 && len(p.tgP.Chats()) >= 1 })

	p.k.StopAll()
	if p.running(presWA) || p.running(presTG) {
		t.Fatal("loops survived shutdown")
	}
	settle(t, p.total)
	p.typing(presWA, time.Minute) // nothing starts after shutdown
	if p.running(presWA) {
		t.Fatal("a loop started after shutdown")
	}
}

// A reply stops the typing before its first message goes out and leaves it
// stopped afterwards, whether the send worked or not, on both channels. On
// WhatsApp it needs no paused: sending a message clears the indicator.
func TestReplyStopsTyping(t *testing.T) {
	for name, tc := range map[string]struct {
		conv string
		fail bool
	}{
		"whatsapp":        {presWA, false},
		"whatsapp failed": {presWA, true},
		"telegram":        {presTG, false},
		"telegram failed": {presTG, true},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPresenceWorld(t)
			e := p.executor()
			e.Presence = p.k
			p.typing(tc.conv, time.Minute)
			waitFor(t, "typing", func() bool { return p.total() >= 1 })
			composingBefore := p.waP.composing()

			// Typing must already be off when the first message goes out.
			var mu sync.Mutex
			var runningAtSend []bool
			check := func() {
				mu.Lock()
				defer mu.Unlock()
				runningAtSend = append(runningAtSend, p.running(tc.conv))
			}
			p.world.wa.failIf = func(waCall) error {
				check()
				if tc.fail {
					return errors.New("boom")
				}
				return nil
			}
			p.customer.failIf = func(custSend) error {
				check()
				if tc.fail {
					return ErrCustomerBlocked
				}
				return nil
			}
			res := e.Handle(context.Background(), sendEnv(t, p.world, "CMD1", Send{Conversation: tc.conv, Kind: "reply", Text: "hi"}))
			if res.OK == tc.fail {
				t.Fatalf("result = %+v", res)
			}
			mu.Lock()
			seen := append([]bool(nil), runningAtSend...)
			mu.Unlock()
			if len(seen) == 0 {
				t.Fatal("no message was sent")
			}
			for _, r := range seen {
				if r {
					t.Error("typing was still running when a message went out")
				}
			}
			if p.running(tc.conv) {
				t.Error("typing is running after the reply")
			}
			settle(t, p.total)
			if got := p.waP.composing(); got < composingBefore {
				t.Error("composing count went down")
			}
			for _, c := range p.waP.Calls() {
				if !c.Composing {
					t.Errorf("the reply sent a paused: %+v", c)
				}
			}
		})
	}
}

// A replayed reply is answered from memory and executes nothing, so it must
// not touch a newer typing either.
func TestReplayedReplyLeavesTypingAlone(t *testing.T) {
	p := newPresenceWorld(t)
	e := p.executor()
	e.Presence = p.k
	env := sendEnv(t, p.world, "CMD1", Send{Conversation: presWA, Kind: "reply", Text: "hi"})
	if res := e.Handle(context.Background(), env); !res.OK {
		t.Fatalf("result = %+v", res)
	}
	p.typing(presWA, time.Minute)
	if res := e.Handle(context.Background(), env); !res.OK {
		t.Fatalf("replay result = %+v", res)
	}
	if !p.running(presWA) {
		t.Fatal("a replayed reply stopped a newer typing")
	}
}

func TestOtherCommandsLeaveTypingAlone(t *testing.T) {
	p := newPresenceWorld(t)
	e := p.executor()
	e.Presence = p.k
	p.typing(presWA, time.Minute)
	e.Handle(context.Background(), sendEnv(t, p.world, "CMD1", Send{Conversation: presWA, Kind: "note", Text: "internal"}))
	if !p.running(presWA) {
		t.Fatal("a note stopped the typing")
	}
}

/* ------------------------------------------------------------- the wire -- */

func TestPresenceValidation(t *testing.T) {
	env := func(payload string) Envelope {
		return Envelope{V: 1, ID: "P1", Type: TypePresence, TS: "2026-10-01T09:30:12.345Z", Payload: json.RawMessage(payload)}
	}
	ok, err := DecodePayload(env(`{"conversation":"wa:60123456789@s.whatsapp.net","state":"typing","expires_at":"2026-10-01T09:31:12.345Z","extra":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := *ok.(*Presence), (Presence{Conversation: presWA, State: "typing", ExpiresAt: "2026-10-01T09:31:12.345Z"}); got != want {
		t.Errorf("decoded %+v", got)
	}
	// lz: is a valid shape: the Hub ignores it later, it is not a violation.
	if _, err := DecodePayload(env(`{"conversation":"lz:1","state":"paused","expires_at":"2026-10-01T09:31:12.345Z"}`)); err != nil {
		t.Errorf("lz: rejected: %v", err)
	}
	for name, payload := range map[string]string{
		"empty":            `{}`,
		"no conversation":  `{"state":"typing","expires_at":"2026-10-01T09:31:12.345Z"}`,
		"bad conversation": `{"conversation":"60123","state":"typing","expires_at":"2026-10-01T09:31:12.345Z"}`,
		"bad state":        `{"conversation":"wa:1@s.whatsapp.net","state":"recording","expires_at":"2026-10-01T09:31:12.345Z"}`,
		"no state":         `{"conversation":"wa:1@s.whatsapp.net","expires_at":"2026-10-01T09:31:12.345Z"}`,
		"no expires_at":    `{"conversation":"wa:1@s.whatsapp.net","state":"typing"}`,
		"bad expires_at":   `{"conversation":"wa:1@s.whatsapp.net","state":"typing","expires_at":"soon"}`,
	} {
		if _, err := DecodePayload(env(payload)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHelloFeaturesRoundTrip(t *testing.T) {
	with := `{"hub_id":"pi","protocol":1,"hub_version":"v","channels":["wa","tg"],"features":["presence"]}`
	var h Hello
	if err := json.Unmarshal([]byte(with), &h); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.Features, []string{"presence"}) || !reflect.DeepEqual(asMap(t, []byte(with)), asMap(t, mustJSON(t, h))) {
		t.Errorf("features lost: %s", mustJSON(t, h))
	}
	// An old hello has none, and none is not written.
	h.Features = nil
	if _, has := asMap(t, mustJSON(t, h)).(map[string]any)["features"]; has {
		t.Errorf("empty features must be omitted: %s", mustJSON(t, h))
	}
}

func TestHubDeclaresPresenceInHello(t *testing.T) {
	w := newWorld(t)
	got := make(chan Hello, 1)
	a := newFakeAgent(t, func(s *agentSession) {
		var hp Hello
		_ = json.Unmarshal(s.handshake().Payload, &hp)
		got <- hp
	})
	h := w.hub(a.URL())
	startLink(t, h)
	select {
	case hp := <-got:
		if !reflect.DeepEqual(hp.Features, []string{"presence"}) {
			t.Errorf("hello features = %v", hp.Features)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no hello")
	}
}

// presence is not a command: a good one shows the indicator and gets no
// result, a bad one gets an error frame, and neither disturbs a command sent
// after it, whose result is the very next frame.
func TestPresenceFramesGetNoResult(t *testing.T) {
	w := newWorld(t)
	type seen struct {
		frames []Envelope
	}
	reported := make(chan seen, 1)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		exp := FormatTS(time.Now().Add(time.Minute))
		s.write("PR1", TypePresence, Presence{Conversation: presWA, State: PresenceTyping, ExpiresAt: exp})
		s.write("PR2", TypePresence, Presence{Conversation: "lz:1", State: PresenceTyping, ExpiresAt: exp})
		s.write("PR3", TypePresence, Presence{Conversation: presWA, State: "singing", ExpiresAt: exp})
		s.write("PR4", TypePresence, Presence{Conversation: presWA, State: PresencePaused, ExpiresAt: exp})
		s.write("NOTE1", TypeSend, Send{Conversation: presWA, Kind: "note", Text: "after", Media: []Media{}, ExpiresAt: exp})
		var out seen
		for len(out.frames) < 2 {
			out.frames = append(out.frames, s.mustNext())
		}
		reported <- out
		if env, err := s.next(300 * time.Millisecond); err == nil {
			t.Errorf("unexpected extra frame %q: %s", env.Type, env.Payload)
		}
	})
	d := w.deps()
	d.Clock = realClock{}
	waP := &fakeWAPresence{}
	d.WAPresence = waP
	d.Presence.WATick = 5 * time.Millisecond
	h, err := NewHub(Config{Enabled: true, URL: a.URL(), Token: "secret-token", HubID: "pi-test", OutboxMaxAgeDays: 7}, d)
	if err != nil {
		t.Fatal(err)
	}
	startLink(t, h)

	out := <-reported
	var pe ProtocolError
	if out.frames[0].Type != TypeError {
		t.Fatalf("first frame = %q, want the error for the bad presence", out.frames[0].Type)
	}
	_ = json.Unmarshal(out.frames[0].Payload, &pe)
	if pe.RefID != "PR3" || pe.Code != "schema" {
		t.Errorf("error = %+v", pe)
	}
	var res Result
	if out.frames[1].Type != TypeResult {
		t.Fatalf("second frame = %q, want the note's result", out.frames[1].Type)
	}
	_ = json.Unmarshal(out.frames[1].Payload, &res)
	if res.CommandID != "NOTE1" || !res.OK {
		t.Errorf("result = %+v", res)
	}

	waitFor(t, "the typing to reach WhatsApp and end on paused", func() bool {
		c := waP.Calls()
		return len(c) >= 2 && c[0].Composing && !c[len(c)-1].Composing
	})
	// Nothing was remembered: presence ids are not command ids.
	for _, id := range []string{"PR1", "PR2", "PR3", "PR4"} {
		if _, found, _ := h.mem.Get(id); found {
			t.Errorf("presence %s was remembered as a command", id)
		}
	}
}

func TestStoppingTheLinkStopsTyping(t *testing.T) {
	w := newWorld(t)
	a := newFakeAgent(t, func(s *agentSession) {
		s.handshake()
		s.write("PR1", TypePresence, Presence{Conversation: presWA, State: PresenceTyping, ExpiresAt: FormatTS(time.Now().Add(time.Minute))})
	})
	d := w.deps()
	d.Clock = realClock{}
	waP := &fakeWAPresence{}
	d.WAPresence = waP
	d.Presence.WATick = 5 * time.Millisecond
	h, err := NewHub(Config{Enabled: true, URL: a.URL(), Token: "secret-token", HubID: "pi-test", OutboxMaxAgeDays: 7}, d)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx)
	waitFor(t, "typing", func() bool { return waP.composing() >= 2 })
	cancel()
	h.Wait(3 * time.Second)
	if _, ok := h.presence.Deadline(presWA); ok {
		t.Fatal("the loop survived the link")
	}
	settle(t, func() int { return len(waP.Calls()) })
}
