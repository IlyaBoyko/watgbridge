package lzcustomer

import (
	"context"
	"errors"
	"strings"
	"time"

	"watgbridge/agentlink"

	"go.uber.org/zap"
)

const (
	defaultTextLimit     = 1000
	defaultPollAfterSend = 5 * time.Second
	// recordAttempts is how often a failed lazada_sent write is retried.
	recordAttempts = 3
)

// SplitText cuts text into parts of at most limit runes, each trimmed and none
// empty. A cut falls, in order of preference, on a paragraph break, a line
// break, a sentence end (". ", "! ", "? "), a space, or, failing all of those,
// exactly at the limit. A limit under 1 means the default.
func SplitText(text string, limit int) []string {
	if limit < 1 {
		limit = defaultTextLimit
	}
	rest := []rune(strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")))
	var parts []string
	for len(rest) > limit {
		cut := cutAt(rest, limit)
		parts = append(parts, strings.TrimSpace(string(rest[:cut])))
		rest = []rune(strings.TrimSpace(string(rest[cut:])))
	}
	if len(rest) > 0 {
		parts = append(parts, string(rest))
	}
	return parts
}

// cutAt returns where to cut rs, which is longer than limit: the part is
// rs[:cut]. Every candidate leaves a part that has something in it.
func cutAt(rs []rune, limit int) int {
	rules := []func(i int) bool{
		func(i int) bool { return rs[i] == '\n' && i+1 < len(rs) && rs[i+1] == '\n' },
		func(i int) bool { return rs[i] == '\n' },
		func(i int) bool { return rs[i] == ' ' && (rs[i-1] == '.' || rs[i-1] == '!' || rs[i-1] == '?') },
		func(i int) bool { return rs[i] == ' ' },
	}
	for _, rule := range rules {
		// i == limit is allowed: the break may sit right after a full part.
		for i := limit; i > 0; i-- {
			if rule(i) && strings.TrimSpace(string(rs[:i])) != "" {
				return i
			}
		}
	}
	return limit
}

// sendFailure is a Lazada failure as the link understands it: errors.Is matches
// the link's sentinel (kind) and the Lazada error underneath (cause), while the
// text stays Lazada's own words for the staff note.
type sendFailure struct {
	kind  error
	cause error
}

func (f *sendFailure) Error() string        { return f.cause.Error() }
func (f *sendFailure) Unwrap() error        { return f.cause }
func (f *sendFailure) Is(target error) bool { return f.kind != nil && target == f.kind }

func mapSendError(err error) error {
	switch {
	case errors.Is(err, ErrRateLimited):
		return &sendFailure{kind: agentlink.ErrCustomerRateLimited, cause: err}
	case isNotAuthorized(err):
		return &sendFailure{kind: agentlink.ErrCustomerBlocked, cause: err}
	}
	return err
}

var errNothingToSend = errors.New("lazada: nothing to send")

// SendText is agentlink.LazadaChannel: it delivers text to a session as one or
// more messages and returns their ids in order.
//
// The fetcher skips seller messages listed in lazada_sent; any other seller
// message it finds is staff speaking from Seller Center, which the Agent takes
// as a human takeover. A message the Hub sent must therefore be in lazada_sent
// before any fetch can see it. A send holds opMu from its first API call to its
// last record, and every fetch round holds the same lock, so a fetch that is
// due simply waits. Inside, each part is recorded right after it is sent, so
// even a part that is followed by a failure is never mistaken for staff.
func (s *Service) SendText(ctx context.Context, sessionID, text, origin string) ([]string, error) {
	if s.life.Err() != nil {
		return nil, agentlink.ErrCustomerBlocked
	}
	parts := SplitText(text, s.textLimit())
	if len(parts) == 0 {
		return nil, errNothingToSend
	}

	s.opMu.Lock()
	ids, err := s.sendParts(ctx, sessionID, parts, origin)
	if err == nil {
		// A reply went out, so the session is answered (A7). Not worth failing
		// the send for.
		if rerr := s.o.API.ReadSession(ctx, sessionID, ids[len(ids)-1]); rerr != nil {
			s.log.Debug("lazada: could not mark the session read", zap.Error(rerr))
		}
	}
	s.opMu.Unlock()

	if len(ids) > 0 {
		s.pollSoon(sessionID)
	}
	return ids, err
}

func (s *Service) sendParts(ctx context.Context, sessionID string, parts []string, origin string) ([]string, error) {
	var ids []string
	for i, part := range parts {
		id, err := s.o.API.SendText(ctx, sessionID, part)
		if err != nil {
			err = mapSendError(err)
			if len(ids) == 0 {
				return nil, err
			}
			return ids, &agentlink.LazadaPartialError{
				Sent: len(ids), Total: len(parts), SentText: strings.Join(parts[:i], "\n\n"), Err: err,
			}
		}
		// Straight away, before the next part and before the lock is let go.
		s.recordSent(sessionID, id, origin)
		ids = append(ids, id)
	}
	return ids, nil
}

// recordSent writes the id to lazada_sent. The message is on Lazada already, so
// there is nothing to undo if it keeps failing: that is logged loudly instead.
func (s *Service) recordSent(sessionID, id, origin string) {
	var err error
	for range recordAttempts {
		if err = s.store.RecordSent(sessionID, id, origin); err == nil {
			return
		}
	}
	s.log.Error("lazada: could not record a sent message; it may be reported as staff speech",
		zap.String("message_id", id), zap.Error(err))
}

func (s *Service) textLimit() int {
	if s.o.TextLimit > 0 {
		return s.o.TextLimit
	}
	return defaultTextLimit
}

// pollSoon fetches the session once a few seconds after a send. Lazada's
// content filter judges a message after the send call has answered, so only
// this later fetch sees process_msg, and the fetcher then posts the warning
// (A6).
func (s *Service) pollSoon(sessionID string) {
	d := s.o.PollAfterSend
	if d < 0 {
		return
	}
	if d == 0 {
		d = defaultPollAfterSend
	}
	after := s.clock.After(d)
	go func() {
		select {
		case <-s.life.Done():
		case <-after:
			s.Poll(sessionID)
		}
	}()
}
