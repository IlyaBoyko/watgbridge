package utils

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	waTypes "go.mau.fi/whatsmeow/types"
	"go.uber.org/zap"
)

type fakePresence struct {
	mu   sync.Mutex
	sent []waTypes.Presence
	err  error
}

func (f *fakePresence) SendPresence(_ context.Context, p waTypes.Presence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, p)
	return f.err
}

func (f *fakePresence) Sent() []waTypes.Presence {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]waTypes.Presence(nil), f.sent...)
}

func TestAnnounceAvailableSendsAvailableAndSurvivesAnError(t *testing.T) {
	f := &fakePresence{err: errors.New("no push name")}
	waAnnounceAvailable(f, zap.NewNop())
	if got := f.Sent(); !reflect.DeepEqual(got, []waTypes.Presence{waTypes.PresenceAvailable}) {
		t.Errorf("sent = %v", got)
	}
}

func TestStaffSendPresenceGoesUnavailableAgainByDefault(t *testing.T) {
	f := &fakePresence{}
	done := make(chan struct{})
	waPresenceForStaffSend(f, false, time.Millisecond, zap.NewNop(), done)
	<-done
	want := []waTypes.Presence{waTypes.PresenceAvailable, waTypes.PresenceUnavailable}
	if got := f.Sent(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v", got, want)
	}
}

func TestStaffSendPresenceNeverGoesUnavailableWhenAlwaysOnline(t *testing.T) {
	f := &fakePresence{}
	done := make(chan struct{})
	waPresenceForStaffSend(f, true, time.Millisecond, zap.NewNop(), done)
	<-done
	time.Sleep(20 * time.Millisecond) // long past the window
	for _, p := range f.Sent() {
		if p == waTypes.PresenceUnavailable {
			t.Fatalf("an always-online account was announced unavailable: %v", f.Sent())
		}
	}
}

type markCall struct {
	ids    []string
	chat   waTypes.JID
	sender waTypes.JID
}

type fakeMarker struct {
	calls []markCall
	fail  map[string]bool // by first message id
}

func (f *fakeMarker) MarkRead(_ context.Context, ids []waTypes.MessageID, _ time.Time, chat, sender waTypes.JID, _ ...waTypes.ReceiptType) error {
	f.calls = append(f.calls, markCall{ids: ids, chat: chat, sender: sender})
	if f.fail[ids[0]] {
		return errors.New("send failed")
	}
	return nil
}

func TestMarkUnreadReadOneToOneChatUsesEmptySender(t *testing.T) {
	chat := waTypes.NewJID("60123456789", waTypes.DefaultUserServer)
	m := &fakeMarker{}
	var done []string
	waMarkUnreadRead(m, "60100000000", chat,
		map[string][]string{"60123456789@s.whatsapp.net": {"A1", "A2"}},
		func(id string) { done = append(done, id) }, zap.NewNop())

	if len(m.calls) != 1 {
		t.Fatalf("calls = %+v", m.calls)
	}
	c := m.calls[0]
	if !reflect.DeepEqual(c.ids, []string{"A1", "A2"}) || c.chat != chat || c.sender != waTypes.EmptyJID {
		t.Errorf("call = %+v", c)
	}
	if !reflect.DeepEqual(done, []string{"A1", "A2"}) {
		t.Errorf("marked in database = %v", done)
	}
}

func TestMarkUnreadReadGroupKeepsSenderAndSkipsOwnMessages(t *testing.T) {
	group := waTypes.NewJID("120363000000000000", waTypes.GroupServer)
	m := &fakeMarker{}
	var done []string
	waMarkUnreadRead(m, "60100000000", group, map[string][]string{
		"60123456789@s.whatsapp.net":   {"G1"},
		"60100000000:7@s.whatsapp.net": {"OWN1"}, // our own account, other device
	}, func(id string) { done = append(done, id) }, zap.NewNop())

	if len(m.calls) != 1 || m.calls[0].sender != waTypes.NewJID("60123456789", waTypes.DefaultUserServer) {
		t.Fatalf("calls = %+v", m.calls)
	}
	sort.Strings(done)
	if !reflect.DeepEqual(done, []string{"G1", "OWN1"}) {
		t.Errorf("marked in database = %v", done)
	}
}

func TestMarkUnreadReadLeavesFailedBatchUnread(t *testing.T) {
	chat := waTypes.NewJID("60123456789", waTypes.DefaultUserServer)
	m := &fakeMarker{fail: map[string]bool{"F1": true}}
	var done []string
	waMarkUnreadRead(m, "", chat,
		map[string][]string{"60123456789@s.whatsapp.net": {"F1"}},
		func(id string) { done = append(done, id) }, zap.NewNop())
	if len(done) != 0 {
		t.Errorf("a message whose receipt failed was marked read: %v", done)
	}
}
