package retry

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// fakeSleep records the pauses and returns at once.
type fakeSleep struct{ waits []time.Duration }

func (f *fakeSleep) sleep(_ context.Context, d time.Duration) error {
	f.waits = append(f.waits, d)
	return nil
}

func failing(n int) (func() error, *int) {
	calls := 0
	return func() error {
		calls++
		if calls <= n {
			return errors.New("boom")
		}
		return nil
	}, &calls
}

func TestSucceedsAfterFailures(t *testing.T) {
	op, calls := failing(3)
	fs := &fakeSleep{}
	var logged []int
	err := Do(t.Context(), Default, fs.sleep, op, func(attempt int, _ time.Duration, err error) {
		if err == nil {
			t.Error("onRetry without an error")
		}
		logged = append(logged, attempt)
	})
	if err != nil || *calls != 4 {
		t.Fatalf("err=%v calls=%d", err, *calls)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !reflect.DeepEqual(fs.waits, want) {
		t.Errorf("waits = %v, want %v", fs.waits, want)
	}
	if !reflect.DeepEqual(logged, []int{1, 2, 3}) {
		t.Errorf("onRetry attempts = %v", logged)
	}
}

func TestFirstTrySucceedsWithoutSleeping(t *testing.T) {
	op, calls := failing(0)
	fs := &fakeSleep{}
	if err := Do(t.Context(), Default, fs.sleep, op, nil); err != nil || *calls != 1 || len(fs.waits) != 0 {
		t.Fatalf("err=%v calls=%d waits=%v", err, *calls, fs.waits)
	}
}

func TestGivesUpAfterTheBudgetWithTheLastError(t *testing.T) {
	fs := &fakeSleep{}
	last := errors.New("still down")
	calls := 0
	err := Do(t.Context(), Default, fs.sleep, func() error { calls++; return last }, nil)
	if !errors.Is(err, last) {
		t.Fatalf("err = %v", err)
	}
	var total time.Duration
	for _, w := range fs.waits {
		total += w
	}
	if total > Default.Budget || total < Default.Budget-Default.Max {
		t.Errorf("slept %v in total, want just under the %v budget", total, Default.Budget)
	}
	if calls != len(fs.waits)+1 {
		t.Errorf("calls = %d with %d pauses", calls, len(fs.waits))
	}
}

func TestBackoffDoublesAndIsCapped(t *testing.T) {
	fs := &fakeSleep{}
	op, _ := failing(9)
	if err := Do(t.Context(), Forever, fs.sleep, op, nil); err != nil {
		t.Fatal(err)
	}
	s := time.Second
	want := []time.Duration{s, 2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s, 30 * s, 30 * s}
	if !reflect.DeepEqual(fs.waits, want) {
		t.Errorf("waits = %v, want %v", fs.waits, want)
	}
}

func TestForeverNeverGivesUp(t *testing.T) {
	fs := &fakeSleep{}
	op, calls := failing(1000)
	if err := Do(t.Context(), Forever, fs.sleep, op, nil); err != nil || *calls != 1001 {
		t.Fatalf("err=%v calls=%d", err, *calls)
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	fs := &fakeSleep{}
	calls := 0
	bad := errors.New("bad token")
	err := Do(t.Context(), Default, fs.sleep, func() error { calls++; return Permanent(bad) }, nil)
	if calls != 1 || len(fs.waits) != 0 || !errors.Is(err, bad) || err.Error() != "bad token" {
		t.Fatalf("calls=%d waits=%v err=%v", calls, fs.waits, err)
	}
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) must stay nil")
	}
}

func TestStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	err := Do(ctx, Forever, Sleep, func() error {
		calls++
		cancel()
		return errors.New("boom")
	}, nil)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
