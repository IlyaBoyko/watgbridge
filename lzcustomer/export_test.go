package lzcustomer

import (
	"context"
	"time"
)

// Handles for the external tests (package lzcustomer_test), which cannot live
// in this package because lzfake imports it.

func (s *Service) Meta(startedAt int64) (MetaRow, bool, error) { return s.store.meta(startedAt) }
func (s *Service) Cursor(id string) (CursorRow, bool, error)   { return s.store.cursor(id) }

// ScanAll is one all-sessions fetch, in the caller's goroutine.
func (s *Service) ScanAll(ctx context.Context) error {
	return s.fetch(ctx, pendingSet{all: true})
}

// FetchSessions is one fetch of just these sessions, in the caller's goroutine.
func (s *Service) FetchSessions(ctx context.Context, ids ...string) error {
	p := pendingSet{sessions: map[string]bool{}}
	for _, id := range ids {
		p.sessions[id] = true
	}
	return s.fetch(ctx, p)
}

// RunRound is a whole round including its error handling.
func (s *Service) RunRound(ctx context.Context) { s.round(ctx, pendingSet{all: true}) }

func (s *Service) Rounds() int64          { return s.rounds.Load() }
func (s *Service) Backoff() time.Duration { return s.backoff }
func (s *Service) DropPending()           { s.take() }
func (s *Service) PendingAll() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingAll
}
func (s *Service) Blocked() bool                    { return s.blocked.Load() }
func (s *Service) Housekeeping(ctx context.Context) { s.housekeeping(ctx) }

// RecordSent notes a message as sent by the Hub (H14 does this for real).
func (s *Service) RecordSent(sessionID, messageID string) error {
	return s.store.RecordSent(sessionID, messageID, OriginAgent)
}

// SetCurrent installs s as the channel the message hooks talk to.
func SetCurrent(s *Service) { current.Store(s) }
