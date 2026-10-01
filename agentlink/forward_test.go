package agentlink

import (
	"encoding/json"
	"testing"
)

const (
	fwdChat   = "60123456789@s.whatsapp.net"
	fwdSource = int64(555) // the staff-group message the command forwarded
	fwdThread = int64(4321)
)

func fwdInput(chat string) ForwardInput {
	return ForwardInput{
		TgMsgID: fwdSource, TgThreadID: fwdThread, WaChat: chat, ChatKey: chat,
		TgUserID: 704338780, Name: "Ilya", Text: "Sorry for the wait, sending it now",
	}
}

func staffEvents(t *testing.T, h *Hub) []StaffMessage {
	t.Helper()
	var out []StaffMessage
	for _, env := range queuedEvents(t, h) {
		if env.Type != TypeStaffMessage {
			t.Fatalf("unexpected event %q", env.Type)
		}
		var m StaffMessage
		if err := json.Unmarshal(env.Payload, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// A6: /send to a one-to-one JID is reported for that conversation.
func TestSendCommandToOneToOneEmitsStaffMessage(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")

	done := h.WatchForward(fwdInput(fwdChat))
	w.bridge.addForwarded(forwarded{fwdSource, fwdThread, fwdChat, "3EB0NEW"}) // TgSendToWhatsApp stores its pair
	done()

	evs := staffEvents(t, h)
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	got := evs[0]
	wantAuthor := StaffAuthor{Source: "topic", TgUserID: "704338780", Name: "Ilya"}
	if got.Conversation != "wa:"+fwdChat || got.TopicID != "1234" || got.HubMsgID != "3EB0NEW" ||
		got.Author != wantAuthor || got.Text != "Sorry for the wait, sending it now" || got.ReplyTo != "" {
		t.Errorf("staff.message = %+v", got)
	}
}

func TestSendCommandToChatWithoutTopicHasTopicZero(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")
	chat := "60111111111@s.whatsapp.net" // no topic yet
	done := h.WatchForward(fwdInput(chat))
	w.bridge.addForwarded(forwarded{fwdSource, fwdThread, chat, "3EB0X"})
	done()
	if evs := staffEvents(t, h); len(evs) != 1 || evs[0].TopicID != "0" || evs[0].Conversation != "wa:"+chat {
		t.Errorf("events = %+v", evs)
	}
}

// A6: nothing for a group or any other chat that is not one-to-one.
func TestSendCommandToGroupEmitsNothing(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")
	for _, chat := range []string{"120363025246125486@g.us", "status@broadcast", "120363012345678901@newsletter", "60199999999@s.whatsapp.net"} {
		done := h.WatchForward(fwdInput(chat))
		w.bridge.addForwarded(forwarded{fwdSource, fwdThread, chat, "3EB0" + chat[:4]})
		done()
	}
	if n := len(queuedEvents(t, h)); n != 0 {
		t.Errorf("%d events for chats that are not one-to-one", n)
	}
}

// A6: a send that failed leaves no pair behind, so nothing is reported.
func TestFailedSendEmitsNothing(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")
	done := h.WatchForward(fwdInput(fwdChat))
	done() // TgSendToWhatsApp showed its error and stored no pair
	if n := len(queuedEvents(t, h)); n != 0 {
		t.Errorf("%d events for a failed send", n)
	}
}

// An older pair of the same message must not be taken for the new send.
func TestForwardIgnoresPairsThatExistedBefore(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")
	// The message being forwarded was itself a bridged customer message.
	w.bridge.addForwarded(forwarded{fwdSource, fwdThread, fwdChat, "3EB0CUSTOMER"})

	done := h.WatchForward(fwdInput(fwdChat))
	done()
	if n := len(queuedEvents(t, h)); n != 0 {
		t.Errorf("a failed send was reported through an old pair (%d events)", n)
	}

	done = h.WatchForward(fwdInput(fwdChat))
	w.bridge.addForwarded(forwarded{fwdSource, fwdThread, fwdChat, "3EB0NEW"})
	done()
	if evs := staffEvents(t, h); len(evs) != 1 || evs[0].HubMsgID != "3EB0NEW" {
		t.Errorf("events = %+v", evs)
	}
}

// What the Agent itself sent is never reported back, on this path either.
func TestForwardRespectsSentGuard(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")
	h.guard.Mark("3EB0AGENT")
	done := h.WatchForward(fwdInput(fwdChat))
	w.bridge.addForwarded(forwarded{fwdSource, fwdThread, fwdChat, "3EB0AGENT"})
	done()
	if n := len(queuedEvents(t, h)); n != 0 {
		t.Errorf("%d events for an id the Agent sent", n)
	}
}

// A7: a status reply is reported for the contact's one-to-one conversation,
// with the topic of the thread it was forwarded into.
func TestStatusReplyEmitsStaffMessageForContact(t *testing.T) {
	w := newWorld(t)
	w.bridge.threads[fwdChat] = 1234
	h := w.hub("ws://unused")

	// The reply was forwarded into the contact's thread as message 777.
	in := ForwardInput{
		TgMsgID: 777, TgThreadID: 1234, WaChat: fwdChat, ChatKey: fwdChat,
		TgUserID: 704338780, Name: "Ilya", Text: "Thanks, still available!",
	}
	done := h.WatchForward(in)
	w.bridge.addForwarded(forwarded{777, 1234, fwdChat, "3EB0STATUSREPLY"})
	done()

	evs := staffEvents(t, h)
	if len(evs) != 1 || evs[0].Conversation != "wa:"+fwdChat || evs[0].TopicID != "1234" ||
		evs[0].HubMsgID != "3EB0STATUSREPLY" || evs[0].Author.Source != "topic" || evs[0].Text != "Thanks, still available!" {
		t.Errorf("events = %+v", evs)
	}
}

func TestForwardWithLinkDisabledDoesNothing(t *testing.T) {
	w := newWorld(t)
	h, err := NewHub(Config{Enabled: false}, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	h.WatchForward(fwdInput(fwdChat))()
	var none *Hub
	none.WatchForward(fwdInput(fwdChat))()
}
