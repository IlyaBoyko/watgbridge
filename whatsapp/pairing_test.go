package whatsapp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type pairHarness struct {
	requests chan struct{}

	mu       sync.Mutex
	notes    []string
	attempts int
	clock    time.Time
}

func newPairHarness() *pairHarness {
	return &pairHarness{requests: make(chan struct{}, 1), clock: time.Unix(1_800_000_000, 0)}
}

func (h *pairHarness) notify(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notes = append(h.notes, s)
}

func (h *pairHarness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock
}

func (h *pairHarness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = h.clock.Add(d)
}

func (h *pairHarness) attemptCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.attempts
}

func (h *pairHarness) noteCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.notes)
}

func (h *pairHarness) lastNote() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.notes[len(h.notes)-1]
}

func (h *pairHarness) ask() { h.requests <- struct{}{} }

// waitNotes waits until at least n notices were sent.
func (h *pairHarness) waitNotes(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.noteCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("got %d notices, want %d", h.noteCount(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// run starts pairLoop with attempt results taken from results in order;
// each attempt calls during(n) first (n counts from 0), so tests can act
// while an attempt is running.
func (h *pairHarness) run(ctx context.Context, results []error, during func(n int)) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- pairLoop(ctx, h.requests, func() error {
			n := h.attemptCount()
			if during != nil {
				during(n)
			}
			h.mu.Lock()
			h.attempts++
			h.mu.Unlock()
			return results[n]
		}, h.notify, h.now)
	}()
	return done
}

func TestPairLoopNeverAttemptsWithoutARequest(t *testing.T) {
	h := newPairHarness()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := <-h.run(ctx, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline", err)
	}
	if h.attemptCount() != 0 {
		t.Fatalf("attempts = %d without any /wa_pair", h.attemptCount())
	}
	if h.noteCount() != 1 {
		t.Fatalf("notes = %q, want only the 'not linked' notice", h.notes)
	}
}

func TestPairLoopSucceedsOnRequest(t *testing.T) {
	h := newPairHarness()
	h.ask()
	err := <-h.run(context.Background(), []error{nil}, nil)
	if err != nil || h.attemptCount() != 1 {
		t.Fatalf("err = %v, attempts = %d", err, h.attemptCount())
	}
	if h.lastNote() != "WhatsApp is linked." {
		t.Fatalf("last note = %q", h.lastNote())
	}
}

func TestPairLoopEnforcesGapAndDropsAsksDuringAttempt(t *testing.T) {
	h := newPairHarness()
	h.ask()
	// The owner asks again while the first attempt runs: that ask must not
	// start a second attempt by itself.
	done := h.run(context.Background(), []error{errors.New("pairing ended: timeout"), nil}, func(n int) {
		if n == 0 {
			h.ask()
		}
	})
	h.waitNotes(t, 2) // "not linked", then "was not linked"
	time.Sleep(20 * time.Millisecond)
	if got := h.attemptCount(); got != 1 {
		t.Fatalf("attempts = %d after the first, want 1", got)
	}

	// Asking again inside the gap is refused with a notice.
	h.advance(time.Minute)
	h.ask()
	h.waitNotes(t, 3)
	if got := h.attemptCount(); got != 1 {
		t.Fatalf("an attempt ran inside the gap (attempts = %d)", got)
	}

	// After the gap, the next ask runs and links.
	h.advance(pairMinGap)
	h.ask()
	if err := <-done; err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := h.attemptCount(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}
