// Package retry runs an operation again with a growing, capped pause between
// attempts. The bridge uses it for the Telegram logins: through the Pi's
// socks5 proxy the first request after a container start can fail, and a
// crash-loop on that would stop WhatsApp forwarding too.
package retry

import (
	"context"
	"errors"
	"time"
)

// Policy is the pause schedule: Initial, doubled after every failure, never
// above Max. Budget is the most time the pauses may add up to before Do gives
// up; zero means never give up.
type Policy struct {
	Initial time.Duration
	Max     time.Duration
	Budget  time.Duration
}

// Default is 1s, 2s, 4s ... capped at 30s, giving up after about 5 minutes.
var Default = Policy{Initial: time.Second, Max: 30 * time.Second, Budget: 5 * time.Minute}

// Forever is Default without the upper limit.
var Forever = Policy{Initial: time.Second, Max: 30 * time.Second}

// Sleeper pauses for d, or returns early with the context's error. Tests pass
// a fake so no real time goes by.
type Sleeper func(ctx context.Context, d time.Duration) error

// Sleep is the real Sleeper.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type permanent struct{ err error }

func (p *permanent) Error() string { return p.err.Error() }
func (p *permanent) Unwrap() error { return p.err }

// Permanent marks err as one that retrying cannot fix (a bad config): Do
// returns it at once.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanent{err}
}

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool {
	var p *permanent
	return errors.As(err, &p)
}

// Do calls op until it succeeds. After a failure it calls onRetry (may be
// nil) with the attempt that failed, the pause that follows and the error,
// then sleeps. It returns the last error when the budget would be exceeded,
// when the error is Permanent, or the context's error if ctx ends first.
func Do(ctx context.Context, p Policy, sleep Sleeper, op func() error, onRetry func(attempt int, wait time.Duration, err error)) error {
	if sleep == nil {
		sleep = Sleep
	}
	wait := p.Initial
	var waited time.Duration
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		if IsPermanent(err) {
			return err
		}
		if p.Budget > 0 && waited+wait > p.Budget {
			return err
		}
		if onRetry != nil {
			onRetry(attempt, wait, err)
		}
		if serr := sleep(ctx, wait); serr != nil {
			return serr
		}
		waited += wait
		wait *= 2
		if p.Max > 0 && wait > p.Max {
			wait = p.Max
		}
	}
}
