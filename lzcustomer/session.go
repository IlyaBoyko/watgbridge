package lzcustomer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"watgbridge/agentlink"

	"go.uber.org/zap"
)

// sessionState is what handling one session's batch carries from message to
// message.
type sessionState struct {
	id     string
	key    string // lz:<session_id>
	title  string
	cursor CursorRow
	has    bool // the cursor is stored
	thread int64
	// first is the oldest message of the batch: a new topic's context note is
	// what came before it.
	first Message
}

func (sc *sessionState) buyerName() string {
	if sc.title != "" {
		return sc.title
	}
	return "Lazada buyer"
}

// fetchSession reads one session's new messages and handles them oldest first.
// info is the session as the list gave it, or nil for a poll of one session.
func (s *Service) fetchSession(ctx context.Context, meta MetaRow, id string, info *Session) error {
	cur, has, err := s.store.cursor(id)
	if err != nil {
		return err
	}
	sc := &sessionState{id: id, key: agentlink.LzChatKey(id), cursor: cur, has: has, title: cur.Title}
	sc.cursor.SessionID = id
	if info != nil && info.Title != "" {
		sc.title = string(info.Title)
	}

	fresh, newest, err := s.collect(ctx, id, sc.cursor, has, meta.StartedAt)
	if err != nil {
		return err
	}

	if len(fresh) > 0 && sc.title == "" {
		sc.title = s.lookupTitle(ctx, id)
	}
	if len(fresh) > 0 {
		sc.first = fresh[0]
	}
	for _, m := range fresh {
		if err := s.handle(ctx, sc, m); err != nil {
			return fmt.Errorf("session %s, message %s: %w", id, m.MessageID, err)
		}
	}

	// Messages that were seen but not kept (older than the first run) still
	// move the cursor, or the session would be fetched again on every scan.
	switch {
	case newest != nil && (!sc.has || (int64(newest.SendTime) >= sc.cursor.LastSendTime &&
		string(newest.MessageID) != sc.cursor.LastMessageID)):
		sc.cursor.LastMessageID, sc.cursor.LastSendTime = string(newest.MessageID), int64(newest.SendTime)
	case newest == nil && !sc.has && info != nil && info.LastMessageID != "":
		sc.cursor.LastMessageID, sc.cursor.LastSendTime = string(info.LastMessageID), int64(info.LastMessageTime)
	case sc.has && sc.cursor.Title == sc.title:
		return nil
	case !sc.has:
		return nil // nothing to anchor a cursor to yet
	}
	sc.cursor.Title = sc.title
	return s.store.saveCursor(sc.cursor)
}

// collect pages a session's messages from now backwards and returns the new
// ones oldest first, plus the newest message seen at all.
func (s *Service) collect(ctx context.Context, id string, cur CursorRow, has bool, startedAt int64) (fresh []Message, newest *Message, err error) {
	var seen []Message
	idFound := false
	var start int64 // 0 asks for "now"
	last := ""
	for {
		page, err := s.o.API.MessageList(ctx, id, start, pageSize, last)
		if err != nil {
			return nil, nil, err
		}
		stop := false
		for _, m := range page.Messages {
			if newest == nil {
				mm := m
				newest = &mm
			}
			t := int64(m.SendTime)
			if has {
				if string(m.MessageID) == cur.LastMessageID {
					idFound, stop = true, true
					break
				}
				if t < cur.LastSendTime {
					stop = true
					break
				}
			} else if t < startedAt {
				stop = true
				break
			}
			seen = append(seen, m)
		}
		if stop || !bool(page.HasMore) || len(page.Messages) == 0 {
			break
		}
		start, last = int64(page.NextStartTime), string(page.LastMessageID)
	}

	for i := len(seen) - 1; i >= 0; i-- { // oldest first
		m := seen[i]
		t := int64(m.SendTime)
		if t < startedAt {
			continue
		}
		// Without the cursor's own id in sight a message stamped at the cursor's
		// time cannot be told from the one already handled.
		if has && !idFound && t <= cur.LastSendTime {
			continue
		}
		fresh = append(fresh, m)
	}
	return fresh, newest, nil
}

// lookupTitle finds the buyer's name for a session the list did not hand over
// (a poll of one session). A chat that just had a message is at the top of the
// list; failing to find it only costs the topic's name.
func (s *Service) lookupTitle(ctx context.Context, id string) string {
	page, err := s.o.API.SessionList(ctx, 0, pageSize, "")
	if err != nil {
		s.log.Debug("lazada: could not look up a session title", zap.Error(err))
		return ""
	}
	for _, ss := range page.Sessions {
		if string(ss.SessionID) == id {
			return string(ss.Title)
		}
	}
	return ""
}

// topicFor finds the session's topic or creates it, with the context note a new
// topic starts with. Several callers never make two topics.
func (s *Service) topicFor(ctx context.Context, sc *sessionState) (int64, error) {
	if sc.thread != 0 {
		return sc.thread, nil
	}
	s.topicMu.Lock()
	defer s.topicMu.Unlock()
	thread, found, err := s.o.Threads.Find(sc.key)
	if err != nil {
		return 0, fmt.Errorf("topic lookup: %w", err)
	}
	if found {
		sc.thread = thread
		return thread, nil
	}

	// The note's messages are fetched before the topic exists: a failed fetch
	// must not leave a topic without its note.
	note, err := s.contextNote(ctx, sc)
	if err != nil {
		return 0, err
	}
	thread, err = s.o.Topics.CreateTopic(ctx, topicName(sc.title))
	if err != nil {
		return 0, fmt.Errorf("could not create the topic: %w", err)
	}
	if err := s.o.Threads.Add(sc.key, thread); err != nil {
		return 0, fmt.Errorf("could not store the topic: %w", err)
	}
	sc.thread = thread
	if note != "" {
		if _, err := s.o.Topics.Post(ctx, thread, TopicPost{Kind: "text", Prefix: notePrefix, Text: note}); err != nil {
			s.log.Warn("lazada: could not post the context note", zap.Error(err))
		}
	}
	return thread, nil
}

func topicName(title string) string {
	if title == "" {
		title = "Lazada buyer"
	}
	name := "Lazada · " + title
	if r := []rune(name); len(r) > topicNameMax {
		name = string(r[:topicNameMax])
	}
	return name
}

// contextNote is A8: the last messages before the first one handled, so staff
// see what came before. It emits no events. "" when there is nothing to show.
func (s *Service) contextNote(ctx context.Context, sc *sessionState) (string, error) {
	before := int64(sc.first.SendTime)
	if before == 0 {
		return "", nil
	}
	// One more than shown: whether start_time is inclusive is not known, and the
	// first message itself would take a place.
	page, err := s.o.API.MessageList(ctx, sc.id, before, contextMessages+1, "")
	if err != nil {
		if isNotAuthorized(err) || errors.Is(err, ErrRateLimited) {
			return "", err
		}
		s.log.Warn("lazada: could not fetch the earlier messages for a new topic", zap.Error(err))
		return "", nil
	}
	var older []Message
	for _, m := range page.Messages {
		if int64(m.SendTime) < before {
			older = append(older, m)
		}
	}
	if len(older) > contextMessages {
		older = older[:contextMessages]
	}
	if len(older) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("Earlier in this chat:")
	for i := len(older) - 1; i >= 0; i-- { // the page is newest first
		m := older[i]
		who := "Shop"
		switch {
		case int64(m.Type) == TypeSystem:
			who = "Lazada"
		case int64(m.FromAccountType) == AccountBuyer:
			who = "Buyer"
		}
		b.WriteString("\n" + who + ": " + oneLine(s.describe(m), 300))
	}
	return b.String(), nil
}
