package tgcustomer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"watgbridge/agentlink"
	"watgbridge/retry"

	"go.uber.org/zap"
)

// ErrNotRunning is what the customer channel answers while the bot has not
// logged in yet.
var ErrNotRunning = errors.New("the customer bot is not logged in yet")

// Launcher gets the customer bot running in the background. A failed login or
// start is never fatal: WhatsApp and the staff group must keep working while
// Telegram or the proxy is down, so the Launcher retries with backoff, without
// an upper limit, until the bot is up.
//
// It is also the agentlink.CustomerChannel, and hands the link a stable value
// before the bot exists: sends fail with ErrNotRunning until then, and go to
// the bot afterwards. This is also why the link may advertise the `tg` channel
// from the config alone: the Hub's channels do not change when the bot comes up.
type Launcher struct {
	// Build logs in and returns the bot, not yet polling. An error wrapped in
	// retry.Permanent (a bad config) is logged once and not retried.
	Build  func() (*Service, error)
	Policy retry.Policy  // zero means retry.Forever
	Sleep  retry.Sleeper // nil means the real clock
	Log    *zap.Logger

	svc atomic.Pointer[Service]

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	polling bool
}

var (
	_ agentlink.CustomerChannel = (*Launcher)(nil)
	_ agentlink.TGPresence      = (*Launcher)(nil)
)

// Run starts the background attempts and returns at once.
func (l *Launcher) Run(ctx context.Context) {
	if l.Log == nil {
		l.Log = zap.NewNop()
	}
	if l.Policy == (retry.Policy{}) {
		l.Policy = retry.Forever
	}
	ctx, cancel := context.WithCancel(ctx)
	l.mu.Lock()
	l.cancel, l.done = cancel, make(chan struct{})
	done := l.done
	l.mu.Unlock()

	go func() {
		defer close(done)
		err := retry.Do(ctx, l.Policy, l.Sleep, l.attempt,
			func(attempt int, wait time.Duration, err error) {
				l.Log.Error("customer bot is not up, retrying",
					zap.Int("attempt", attempt), zap.Duration("retry_in", wait), zap.Error(err))
			})
		switch {
		case err == nil:
		case ctx.Err() != nil:
		default:
			l.Log.Error("customer bot will stay off until the Hub is restarted", zap.Error(err))
		}
	}()
}

// attempt builds the bot if that has not worked yet, then starts polling. A
// bot that was built stays: only a failed Start is tried again.
func (l *Launcher) attempt() error {
	s := l.svc.Load()
	if s == nil {
		var err error
		if s, err = l.Build(); err != nil {
			return err
		}
		l.svc.Store(s)
	}
	if err := s.Start(); err != nil {
		return err
	}
	l.mu.Lock()
	l.polling = true
	l.mu.Unlock()
	return nil
}

// Stop ends the attempts and, if the bot is polling, polling too.
func (l *Launcher) Stop() {
	l.mu.Lock()
	cancel, done := l.cancel, l.done
	l.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	l.mu.Lock()
	polling := l.polling
	l.polling = false
	l.mu.Unlock()
	if s := l.svc.Load(); s != nil && polling {
		s.Stop()
	}
}

/* ----------------------------------------------- agentlink.CustomerChannel -- */

func (l *Launcher) SendText(ctx context.Context, chatID int64, m agentlink.CustomerText) (int64, error) {
	s := l.svc.Load()
	if s == nil {
		return 0, ErrNotRunning
	}
	return s.SendText(ctx, chatID, m)
}

func (l *Launcher) SendFile(ctx context.Context, chatID int64, m agentlink.CustomerFile) (int64, error) {
	s := l.svc.Load()
	if s == nil {
		return 0, ErrNotRunning
	}
	return s.SendFile(ctx, chatID, m)
}

func (l *Launcher) RecordPair(chatID, customerMsgID, threadID, topicMsgID int64) error {
	s := l.svc.Load()
	if s == nil {
		return ErrNotRunning
	}
	return s.RecordPair(chatID, customerMsgID, threadID, topicMsgID)
}

func (l *Launcher) HubMsgIDOfTopicMsg(threadID, topicMsgID int64) string {
	s := l.svc.Load()
	if s == nil {
		return ""
	}
	return s.HubMsgIDOfTopicMsg(threadID, topicMsgID)
}

// SendTyping is agentlink.TGPresence: nothing to show until the bot is up.
func (l *Launcher) SendTyping(ctx context.Context, chatID int64) error {
	s := l.svc.Load()
	if s == nil {
		return ErrNotRunning
	}
	return s.SendTyping(ctx, chatID)
}
