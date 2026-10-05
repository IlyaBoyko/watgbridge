package tgcustomer

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"watgbridge/agentlink"
	"watgbridge/retry"

	"go.uber.org/zap"
)

// instantSleep records the pauses and returns at once.
type instantSleep struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *instantSleep) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return nil
}

func (s *instantSleep) Waits() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.waits...)
}

func (w *world) buildAgainst(t *testing.T, cust *fakeTG) func() (*Service, error) {
	return func() (*Service, error) {
		return NewService(Options{
			Token: "222:CUST", APIURL: cust.URL(), DB: newTestDB(t), Topics: w.topics, Threads: w.threads,
			Events: w.events, Log: zap.NewNop(),
		})
	}
}

// H9: the customer bot's login fails twice (the first request through the
// proxy after a container start), then works. The Hub must keep running, retry
// with backoff, and start polling once the login succeeds.
func TestLauncherStartsTheBotAfterALaterLogin(t *testing.T) {
	w := newWorld(t)
	cust := newFakeTG(t)
	var getMe atomic.Int32
	cust.respond["getMe"] = func(tgCall) (int, string) {
		if getMe.Add(1) <= 2 {
			return 502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`
		}
		return 200, `{"ok":true,"result":{"id":123456,"is_bot":true,"first_name":"Customers","username":"customers_bot"}}`
	}
	sl := &instantSleep{}
	l := &Launcher{Build: w.buildAgainst(t, cust), Sleep: sl.sleep, Log: zap.NewNop()}

	// Until the bot is up the link's channel fails honestly, and nothing is sent.
	if _, err := l.SendText(t.Context(), 5550001111, agentlink.CustomerText{HTML: "hi"}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("before login: err = %v", err)
	}
	if got := l.HubMsgIDOfTopicMsg(1250, 1); got != "" {
		t.Errorf("before login: hub msg id = %q", got)
	}

	l.Run(t.Context())
	defer l.Stop()
	eventually(t, "the first poll", func() bool { return len(cust.CallsTo("getUpdates")) > 0 })

	if got, want := sl.Waits(), []time.Duration{time.Second, 2 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
	if n := len(cust.CallsTo("getMe")); n != 3 {
		t.Errorf("getMe calls = %d, want 3", n)
	}
	if del, poll := indexOf(cust.Methods(), "deleteWebhook"), indexOf(cust.Methods(), "getUpdates"); del < 0 || del > poll {
		t.Errorf("methods = %v, want deleteWebhook before getUpdates", cust.Methods())
	}
	// And now the channel reaches the customer.
	if _, err := l.SendText(t.Context(), 5550001111, agentlink.CustomerText{HTML: "hi"}); err != nil {
		t.Errorf("after login: err = %v", err)
	}
	if n := len(cust.CallsTo("sendMessage")); n != 1 {
		t.Errorf("sendMessage calls = %d, want 1", n)
	}
}

// A bot that logged in but whose start failed (deleteWebhook through the
// proxy) is not built again: only the start is retried.
func TestLauncherRetriesOnlyTheStartOfABuiltBot(t *testing.T) {
	w := newWorld(t)
	cust := newFakeTG(t)
	var del atomic.Int32
	cust.respond["deleteWebhook"] = func(tgCall) (int, string) {
		if del.Add(1) == 1 {
			return 502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`
		}
		return 200, `{"ok":true,"result":true}`
	}
	var builds atomic.Int32
	build := w.buildAgainst(t, cust)
	l := &Launcher{
		Build: func() (*Service, error) { builds.Add(1); return build() },
		Sleep: (&instantSleep{}).sleep, Log: zap.NewNop(),
	}
	l.Run(t.Context())
	defer l.Stop()
	eventually(t, "the first poll", func() bool { return len(cust.CallsTo("getUpdates")) > 0 })
	if builds.Load() != 1 || len(cust.CallsTo("deleteWebhook")) != 2 {
		t.Errorf("builds=%d deleteWebhook calls=%d", builds.Load(), len(cust.CallsTo("deleteWebhook")))
	}
}

// The attempts have no upper limit: after far more failures than the staff
// bot's budget allows, a later success still starts the bot.
func TestLauncherNeverGivesUp(t *testing.T) {
	w := newWorld(t)
	cust := newFakeTG(t)
	var builds atomic.Int32
	sl := &instantSleep{}
	build := w.buildAgainst(t, cust)
	l := &Launcher{
		Build: func() (*Service, error) {
			if builds.Add(1) <= 200 {
				return nil, errors.New("customer bot: could not log in")
			}
			return build()
		},
		Sleep: sl.sleep, Log: zap.NewNop(),
	}
	l.Run(t.Context())
	defer l.Stop()
	eventually(t, "the first poll", func() bool { return len(cust.CallsTo("getUpdates")) > 0 })
	waits := sl.Waits()
	if len(waits) != 200 {
		t.Fatalf("pauses = %d, want 200", len(waits))
	}
	for _, d := range waits {
		if d > 30*time.Second {
			t.Fatalf("pause %v above the 30s cap", d)
		}
	}
}

// A configuration error cannot be fixed by waiting: it is tried once, and the
// Hub carries on without the bot.
func TestLauncherDoesNotRetryAPermanentError(t *testing.T) {
	var builds atomic.Int32
	sl := &instantSleep{}
	l := &Launcher{
		Build: func() (*Service, error) {
			builds.Add(1)
			return nil, retry.Permanent(errors.New("customer_bot.bot_token is not set"))
		},
		Sleep: sl.sleep, Log: zap.NewNop(),
	}
	l.Run(t.Context())
	l.Stop() // waits for the attempts to end
	if builds.Load() != 1 || len(sl.Waits()) != 0 {
		t.Errorf("builds=%d waits=%v", builds.Load(), sl.Waits())
	}
}

// Shutdown while the bot is still down must not hang.
func TestLauncherStopEndsTheAttempts(t *testing.T) {
	asleep := make(chan struct{})
	var once sync.Once
	l := &Launcher{
		Build: func() (*Service, error) { return nil, errors.New("down") },
		Sleep: func(ctx context.Context, _ time.Duration) error {
			once.Do(func() { close(asleep) })
			<-ctx.Done()
			return ctx.Err()
		},
		Log: zap.NewNop(),
	}
	l.Run(t.Context())
	<-asleep
	done := make(chan struct{})
	go func() { l.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
}
