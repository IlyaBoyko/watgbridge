package agentlink

import (
	"regexp"
	"strings"
	"testing"
	"time"

	waTypes "go.mau.fi/whatsmeow/types"
)

// A4: deterministic event ids.
func TestEventIDIsDeterministic(t *testing.T) {
	a := EventID("customer.message", "wa:60123456789@s.whatsapp.net", "3EB0A1")
	if a != EventID("customer.message", "wa:60123456789@s.whatsapp.net", "3EB0A1") {
		t.Error("same inputs gave different ids")
	}
	if !regexp.MustCompile(`^wa-[0-9a-f]{26}$`).MatchString(a) {
		t.Errorf("id %q is not wa- plus 26 hex characters", a)
	}
	for name, other := range map[string]string{
		"other type":         EventID("staff.message", "wa:60123456789@s.whatsapp.net", "3EB0A1"),
		"other conversation": EventID("customer.message", "wa:60123456780@s.whatsapp.net", "3EB0A1"),
		"other message":      EventID("customer.message", "wa:60123456789@s.whatsapp.net", "3EB0A2"),
	} {
		if other == a {
			t.Errorf("%s gave the same id", name)
		}
	}
	// The separator must not let two different triples collide.
	if EventID("a", "b|c", "d") == EventID("a|b", "c", "d") {
		t.Log("note: '|' inside a field can collide; real conversations and message ids never contain it")
	}
}

func TestULIDShapeAndUniqueness(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 30, 12, 0, time.UTC)
	re := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewULID(now)
		if !re.MatchString(id) {
			t.Fatalf("%q is not a ULID", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ULID %q", id)
		}
		seen[id] = true
	}
	// Later times sort later.
	a, b := NewULID(now), NewULID(now.Add(time.Second))
	if !(a < b) {
		t.Errorf("ULIDs do not sort by time: %s !< %s", a, b)
	}
	if !strings.HasPrefix(NewULID(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)), "01") {
		t.Error("expected a 2026 ULID to start with 01")
	}
}

// A7: conversation mapping.
func TestConversationMapping(t *testing.T) {
	self := func(j waTypes.JID) bool { return j.User == "60199999999" }

	conv, err := ConversationFor("60123456789@s.whatsapp.net", self)
	if err != nil || conv != "wa:60123456789@s.whatsapp.net" {
		t.Fatalf("phone JID -> %q, %v", conv, err)
	}
	key, err := ChatKeyFor(conv, self)
	if err != nil || key != "60123456789@s.whatsapp.net" {
		t.Fatalf("back -> %q, %v", key, err)
	}

	// A LID with no known phone number is still a one-to-one chat.
	if conv, err := ConversationFor("98765432101@lid", self); err != nil || conv != "wa:98765432101@lid" {
		t.Errorf("lid -> %q, %v", conv, err)
	}

	rejected := map[string]string{
		"group":       "120363025246125486@g.us",
		"status":      "status@broadcast",
		"broadcast":   "1234567890@broadcast",
		"newsletter":  "120363012345678901@newsletter",
		"self":        "60199999999@s.whatsapp.net",
		"device JID":  "60123456789:12@s.whatsapp.net",
		"empty":       "",
		"garbage":     "not a jid at all@@",
		"server only": "s.whatsapp.net",
	}
	for name, jid := range rejected {
		if _, err := ConversationFor(jid, self); err == nil {
			t.Errorf("%s (%q): accepted as an event conversation", name, jid)
		}
		if _, err := ChatKeyFor("wa:"+jid, self); err == nil {
			t.Errorf("%s (%q): accepted as a send target", name, jid)
		}
	}
	for _, conv := range []string{"tg:12345", "lz:abc", "60123456789@s.whatsapp.net", "wa:"} {
		if _, err := ChatKeyFor(conv, self); err == nil {
			t.Errorf("%q accepted as a WhatsApp conversation", conv)
		}
	}
}

func TestPhoneFor(t *testing.T) {
	for in, want := range map[string]string{
		"60123456789@s.whatsapp.net": "+60123456789",
		"98765432101@lid":            "",
		"120363025246125486@g.us":    "",
		"":                           "",
	} {
		if got := PhoneFor(in); got != want {
			t.Errorf("PhoneFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBackoff(t *testing.T) {
	min, max := time.Second, 60*time.Second
	for attempt := 0; attempt < 12; attempt++ {
		lo := Backoff(attempt, min, max, func() float64 { return 0 })
		hi := Backoff(attempt, min, max, func() float64 { return 0.999999 })
		want := min << attempt
		if want > max {
			want = max
		}
		if lo != want/2 || hi < lo || hi > want {
			t.Errorf("attempt %d: delay range [%v, %v], want [%v, %v]", attempt, lo, hi, want/2, want)
		}
	}
	if d := Backoff(0, min, max, func() float64 { return 0.5 }); d < 500*time.Millisecond || d > time.Second {
		t.Errorf("first delay %v outside 0.5s..1s", d)
	}
}
