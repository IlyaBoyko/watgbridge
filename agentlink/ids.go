package agentlink

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Clock is the link's only source of time, so expiry, pruning and the
// outbox's age limit can be tested without waiting.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// EventID is the deterministic id of an event that comes from a channel
// message (protocol §2). WhatsApp can deliver one message twice and a restart
// can re-process it; the same id lets the Agent's deduplication catch both.
func EventID(typ, conversation, hubMsgID string) string {
	sum := sha256.Sum256([]byte(typ + "|" + conversation + "|" + hubMsgID))
	return "wa-" + hex.EncodeToString(sum[:])[:26]
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns a 26-character ULID. Ids made in the same millisecond still
// differ because the random part is fresh each time.
func NewULID(now time.Time) string {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic("agentlink: no randomness: " + err.Error())
	}

	// 128 bits -> 26 base32 characters (the first carries only 3 bits).
	out := make([]byte, 26)
	var acc uint32
	bits := 0
	idx := 25
	for i := 15; i >= 0; i-- {
		acc |= uint32(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[idx] = crockford[acc&31]
			idx--
			acc >>= 5
			bits -= 5
		}
	}
	if idx == 0 {
		out[0] = crockford[acc&31]
	}
	return string(out)
}
