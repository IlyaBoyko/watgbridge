package agentlink

import (
	"context"
	"errors"
	"testing"

	"watgbridge/state"
)

// markRecorder is Executor.MarkRead for tests.
type markRecorder struct {
	chats []string
	err   error
}

func (m *markRecorder) mark(chat string) error {
	m.chats = append(m.chats, chat)
	return m.err
}

func TestSuccessfulWhatsAppReplyMarksTheChatRead(t *testing.T) {
	w := newWorld(t)
	rec := &markRecorder{}
	e := w.executor()
	e.MarkRead = rec.mark

	res := e.Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "hi"}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	if len(rec.chats) != 1 || rec.chats[0] != "60123456789@s.whatsapp.net" {
		t.Errorf("marked read = %v, want the replied chat once", rec.chats)
	}
}

func TestMarkReadFailureDoesNotFailTheReply(t *testing.T) {
	w := newWorld(t)
	rec := &markRecorder{err: errors.New("whatsapp down")}
	e := w.executor()
	e.MarkRead = rec.mark

	res := e.Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "hi"}))
	if !res.OK || res.Error != "" || res.HubMsgID != "WA001" {
		t.Fatalf("result = %+v", res)
	}
	if len(rec.chats) != 1 {
		t.Errorf("mark-read was not attempted: %v", rec.chats)
	}
}

func TestNoMarkReadWhenTheOptionIsOff(t *testing.T) {
	w := newWorld(t)
	e := w.executor() // MarkRead is nil: telegram.send_my_read_receipts is off
	res := e.Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "hi"}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
}

func TestNoMarkReadWhenTheWhatsAppSendFailed(t *testing.T) {
	w := newWorld(t)
	w.wa.fail = errors.New("not on whatsapp")
	rec := &markRecorder{}
	e := w.executor()
	e.MarkRead = rec.mark

	res := e.Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "hi"}))
	if res.OK {
		t.Fatalf("result = %+v", res)
	}
	if len(rec.chats) != 0 {
		t.Errorf("marked read after a failed send: %v", rec.chats)
	}
}

func TestNoMarkReadForNotesCardsOrTelegramReplies(t *testing.T) {
	w := tgWorld(t)
	rec := &markRecorder{}
	e := w.executor()
	e.MarkRead = rec.mark

	e.Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "note", Text: "note"}))
	e.Handle(context.Background(), tgSend(t, w, "CMD2", Send{Kind: "reply", Text: "hi"}))
	if len(rec.chats) != 0 {
		t.Errorf("marked read: %v", rec.chats)
	}
}

func TestReadReceiptHookFollowsTheConfigOption(t *testing.T) {
	cfg := &state.Config{}
	if readReceiptHook(cfg) != nil {
		t.Error("hook is set although telegram.send_my_read_receipts is off")
	}
	cfg.Telegram.SendMyReadReceipts = true
	if readReceiptHook(cfg) == nil {
		t.Error("hook is missing although telegram.send_my_read_receipts is on")
	}
}
