package lzcustomer

import (
	"context"
	"errors"
	"sort"
	"time"

	"go.uber.org/zap"
)

const (
	pageSize = 20
	// scanSlack is how far behind the last full scan's watermark the next one
	// reads, so a session whose newest message was stamped a little late is not
	// missed.
	scanSlack = 60 * time.Second
	// contextMessages is how many earlier messages a new topic opens with.
	contextMessages = 10

	backoffStart = 30 * time.Second
	backoffMax   = 10 * time.Minute
)

// pendingSet is what the triggers asked for: every session, or these.
type pendingSet struct {
	all      bool
	sessions map[string]bool
}

func (p pendingSet) empty() bool { return !p.all && len(p.sessions) == 0 }

func (s *Service) take() pendingSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := pendingSet{all: s.pendingAll, sessions: s.pending}
	s.pendingAll, s.pending = false, map[string]bool{}
	return p
}

// requeue puts back what a round did not get to (a rate limit hit mid-way).
func (s *Service) requeue(p pendingSet) {
	s.mu.Lock()
	s.pendingAll = s.pendingAll || p.all
	for id := range p.sessions {
		s.pending[id] = true
	}
	s.mu.Unlock()
}

// work is the one goroutine that fetches: there is never more than one fetch at
// a time. Triggers that arrive while it runs set the pending set and wake it
// again, so any number of them cost exactly one more round.
func (s *Service) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		for {
			// During a backoff, triggers are only remembered.
			if wait := s.backoffUntil.Sub(s.clock.Now()); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-s.clock.After(wait):
				}
			}
			if s.blocked.Load() {
				s.take() // nothing to fetch with; /lazada_auth starts a fresh round
				break
			}
			p := s.take()
			if p.empty() {
				break
			}
			s.round(ctx, p)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// round runs one fetch and sorts out how it failed.
func (s *Service) round(ctx context.Context, p pendingSet) {
	defer s.rounds.Add(1)
	err := s.fetch(ctx, p)
	switch {
	case err == nil:
		s.backoff, s.backoffUntil = 0, time.Time{}
	case ctx.Err() != nil:
	case errors.Is(err, ErrRateLimited):
		if s.backoff == 0 {
			s.backoff = backoffStart
		} else if s.backoff *= 2; s.backoff > backoffMax {
			s.backoff = backoffMax
		}
		s.backoffUntil = s.clock.Now().Add(s.backoff)
		s.log.Warn("lazada: rate limited, backing off", zap.Duration("retry_in", s.backoff))
		s.requeue(p)
	case isNotAuthorized(err):
		s.notAuthorized(ctx)
	default:
		// Whatever it was, the next trigger tries again. The cursors only move
		// past messages that were handled, so nothing is lost.
		s.log.Warn("lazada: fetch failed, will retry on the next trigger", zap.Error(err))
	}
}

// notAuthorized stops the fetching and says so once. A later trigger does
// nothing until /lazada_auth succeeds.
func (s *Service) notAuthorized(ctx context.Context) {
	if !s.blocked.CompareAndSwap(false, true) {
		return
	}
	s.log.Warn("lazada: not authorized, fetching stops until /lazada_auth")
	text := "Lazada is not authorized. Approve the app at " + s.authLink() +
		", then send /lazada_auth <code> here."
	if _, err := s.o.Topics.Post(ctx, 0, TopicPost{Kind: "text", Text: text}); err != nil {
		s.log.Error("lazada: could not post the authorization notice", zap.Error(err))
	}
}

// fetch does one round. All sessions are scanned first; single sessions that
// the scan did not cover follow.
func (s *Service) fetch(ctx context.Context, p pendingSet) error {
	meta, _, err := s.store.meta(s.clock.Now().UnixMilli())
	if err != nil {
		return err
	}
	done := map[string]bool{}
	if p.all {
		if err := s.scanAll(ctx, &meta, done); err != nil {
			return err
		}
	}
	ids := make([]string, 0, len(p.sessions))
	for id := range p.sessions {
		if !done[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := s.fetchSession(ctx, meta, id, nil); err != nil {
			return err
		}
	}
	return nil
}

// scanAll pages the session list from now backwards down to the last scan's
// watermark, fetches every session that has something new, and only when all
// of that worked moves the watermark: a failed fetch must stay inside the next
// scan's window.
func (s *Service) scanAll(ctx context.Context, meta *MetaRow, done map[string]bool) error {
	stopAt := meta.StartedAt
	if meta.FullScanWatermark > 0 {
		stopAt = meta.FullScanWatermark - scanSlack.Milliseconds()
	}

	var found []Session
	var newest int64
	var start int64 // 0 asks for "now"
	last := ""
	for {
		page, err := s.o.API.SessionList(ctx, start, pageSize, last)
		if err != nil {
			return err
		}
		reachedEnd := false
		for _, ss := range page.Sessions {
			t := int64(ss.LastMessageTime)
			if t < stopAt {
				reachedEnd = true
				break
			}
			if t > newest {
				newest = t
			}
			found = append(found, ss)
		}
		if reachedEnd || !bool(page.HasMore) || len(page.Sessions) == 0 {
			break
		}
		start, last = int64(page.NextStartTime), string(page.LastSessionID)
	}

	// Oldest first, so that messages of different sessions also reach the
	// Agent roughly in the order they were written.
	sort.SliceStable(found, func(i, j int) bool { return found[i].LastMessageTime < found[j].LastMessageTime })
	for i := range found {
		ss := found[i]
		id := string(ss.SessionID)
		if id == "" {
			continue
		}
		cur, has, err := s.store.cursor(id)
		if err != nil {
			return err
		}
		if has && cur.LastMessageID == string(ss.LastMessageID) {
			continue
		}
		if err := s.fetchSession(ctx, *meta, id, &ss); err != nil {
			return err
		}
		done[id] = true
	}

	if newest > meta.FullScanWatermark {
		meta.FullScanWatermark = newest
		return s.store.saveMeta(*meta)
	}
	return nil
}
