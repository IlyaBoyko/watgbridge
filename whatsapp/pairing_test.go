package whatsapp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"watgbridge/state"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
)

type pairHarness struct {
	state *pairingState

	mu       sync.Mutex
	notes    []string
	attempts int
	clock    time.Time
}

func newPairHarness() *pairHarness {
	return &pairHarness{state: newPairingState(), clock: time.Unix(1_800_000_000, 0)}
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

func (h *pairHarness) ask() { h.state.requests <- struct{}{} }

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
	return h.runFrom(ctx, false, results, during)
}

func (h *pairHarness) runFrom(ctx context.Context, tryFirst bool, results []error, during func(n int)) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- pairLoop(ctx, h.state, tryFirst, func() error {
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
		t.Fatalf("attempts = %d without any request", h.attemptCount())
	}
	if h.noteCount() != 1 {
		t.Fatalf("notes = %q, want only the 'not linked' notice", h.notes)
	}
}

func TestPairLoopStartupTriesOnceThenStops(t *testing.T) {
	h := newPairHarness()
	ctx, cancel := context.WithCancel(context.Background())
	done := h.runFrom(ctx, true, []error{errors.New("pairing ended: timeout"), nil}, nil)
	h.waitNotes(t, 2) // "QR codes follow", then "stopping"
	time.Sleep(20 * time.Millisecond)
	if got := h.attemptCount(); got != 1 {
		t.Fatalf("attempts = %d after an unscanned startup attempt, want exactly 1", got)
	}

	// The gap counts from the automatic attempt too.
	h.ask()
	h.waitNotes(t, 3)
	if got := h.attemptCount(); got != 1 {
		t.Fatalf("an attempt ran inside the gap (attempts = %d)", got)
	}

	h.advance(pairMinGap)
	h.ask()
	if err := <-done; err != nil {
		t.Fatalf("err = %v", err)
	}
	cancel()
	if got := h.attemptCount(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
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

func TestAskQueuesOnlyWhileWaiting(t *testing.T) {
	p := newPairingState()

	queued := func() bool {
		select {
		case <-p.requests:
			return true
		default:
			return false
		}
	}

	if got := p.ask(true); !strings.Contains(got, "already linked") || queued() {
		t.Fatalf("linked: %q", got)
	}
	if got := p.ask(false); !strings.Contains(got, "still starting") || queued() {
		t.Fatalf("before the hold: %q", got)
	}

	if !p.beginHold() {
		t.Fatal("first hold refused")
	}
	if p.beginHold() {
		t.Fatal("second hold allowed while one runs")
	}
	p.attemptStarted(time.Unix(1, 0))
	if got := p.ask(false); !strings.Contains(got, "already running") || queued() {
		t.Fatalf("during an attempt: %q", got)
	}
	p.attemptEnded(errors.New("pairing ended: timeout"))
	if got := p.ask(false); got != "Pairing requested." || !queued() {
		t.Fatalf("while waiting: %q", got)
	}
}

func TestBeginHoldDropsAStaleAsk(t *testing.T) {
	p := newPairingState()
	p.requests <- struct{}{}
	p.beginHold()
	select {
	case <-p.requests:
		t.Fatal("an ask from before the hold survived and would start an attempt")
	default:
	}
}

func TestStatusText(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)

	linked := newPairingState().statusText(waLink{
		linked: true, connected: true, loggedIn: true, name: "A<b", jid: "1@s.whatsapp.net",
	}, start)
	if !strings.Contains(linked, "linked and connected") || !strings.Contains(linked, "A&lt;b") {
		t.Fatalf("linked: %q", linked)
	}

	reconnecting := newPairingState().statusText(waLink{linked: true}, start)
	if !strings.Contains(reconnecting, "not connected") {
		t.Fatalf("reconnecting: %q", reconnecting)
	}

	p := newPairingState()
	p.beginHold()
	if got := p.statusText(waLink{}, start); !strings.Contains(got, "not contacting WhatsApp") || strings.Contains(got, "Last attempt") {
		t.Fatalf("waiting, no attempt yet: %q", got)
	}

	p.attemptStarted(start)
	if got := p.statusText(waLink{}, start.Add(time.Minute)); !strings.Contains(got, "attempt is running") || strings.Contains(got, "Pair available") {
		t.Fatalf("attempting: %q", got)
	}

	p.attemptEnded(errors.New("pairing ended: timeout"))
	got := p.statusText(waLink{}, start.Add(time.Minute))
	for _, want := range []string{"1m0s ago", "pairing ended: timeout", "Pair available in:</b> 2m0s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("after a missed scan, %q lacks %q", got, want)
		}
	}
	if got := p.statusText(waLink{}, start.Add(pairMinGap)); strings.Contains(got, "Pair available") {
		t.Fatalf("after the gap: %q", got)
	}
}

func TestClientForPairingReplacesADeletedDevice(t *testing.T) {
	container, err := sqlstore.New(context.Background(), "sqlite3",
		"file:"+filepath.Join(t.TempDir(), "wa.db")+"?_foreign_keys=on", nil)
	if err != nil {
		t.Fatal(err)
	}
	prevContainer, prevClient := waContainer, state.State.WhatsAppClient
	t.Cleanup(func() { waContainer, state.State.WhatsAppClient = prevContainer, prevClient })
	waContainer = container

	live := whatsmeow.NewClient(container.NewDevice(), nil)
	state.State.WhatsAppClient = live
	if got := clientForPairing(); got != live {
		t.Fatal("a device that was not deleted must keep its client")
	}

	// What whatsmeow does on a logout: its client then refuses to connect.
	live.Store.Deleted = true
	got := clientForPairing()
	if got == live || got.Store.Deleted || got.Store.ID != nil {
		t.Fatalf("no fresh unlinked client after a logout: same=%v deleted=%v", got == live, got.Store.Deleted)
	}
	if state.State.WhatsAppClient != got {
		t.Fatal("the bridge still points at the deleted client")
	}
	if _, err := got.GetQRChannel(context.Background()); err != nil {
		t.Fatalf("the new client cannot start pairing: %v", err)
	}
}
