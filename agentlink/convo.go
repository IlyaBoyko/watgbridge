package agentlink

import (
	"fmt"
	"strings"

	waTypes "go.mau.fi/whatsmeow/types"
)

const waConvPrefix = "wa:"

// OneToOneJID parses a WhatsApp JID and accepts it only when it names a
// one-to-one chat: a phone-number JID or a LID, with no device part. Groups,
// status broadcasts, newsletters and the bridge's own chat are rejected, so
// the Agent can neither hear about them nor send to them.
//
// isSelf may be nil (tests, or before the WhatsApp client exists).
func OneToOneJID(s string, isSelf func(waTypes.JID) bool) (waTypes.JID, error) {
	jid, err := waTypes.ParseJID(s)
	if err != nil {
		return waTypes.EmptyJID, fmt.Errorf("not a JID: %w", err)
	}
	if jid.Server != waTypes.DefaultUserServer && jid.Server != waTypes.HiddenUserServer {
		return waTypes.EmptyJID, fmt.Errorf("%s is not a one-to-one chat", s)
	}
	if jid.User == "" || jid.Device != 0 || jid.RawAgent != 0 {
		return waTypes.EmptyJID, fmt.Errorf("%s is not a plain user JID", s)
	}
	if isSelf != nil && isSelf(jid) {
		return waTypes.EmptyJID, fmt.Errorf("%s is the bridge's own chat", s)
	}
	return jid, nil
}

// ConversationFor maps a chat_thread key (what the bridge stores for a chat)
// to the protocol's conversation id. The "wa:" prefix exists only on the wire.
func ConversationFor(chatKey string, isSelf func(waTypes.JID) bool) (string, error) {
	jid, err := OneToOneJID(chatKey, isSelf)
	if err != nil {
		return "", err
	}
	return waConvPrefix + jid.String(), nil
}

// ChatKeyFor is the inverse of ConversationFor.
func ChatKeyFor(conversation string, isSelf func(waTypes.JID) bool) (string, error) {
	rest, ok := strings.CutPrefix(conversation, waConvPrefix)
	if !ok {
		return "", fmt.Errorf("%q is not a WhatsApp conversation", conversation)
	}
	jid, err := OneToOneJID(rest, isSelf)
	if err != nil {
		return "", err
	}
	return jid.String(), nil
}

// PhoneFor returns "+<digits>" for a phone-number JID and "" for anything else
// (a LID that has no known phone number must not be presented as one).
func PhoneFor(chatKey string) string {
	jid, err := waTypes.ParseJID(chatKey)
	if err != nil || jid.Server != waTypes.DefaultUserServer || jid.User == "" {
		return ""
	}
	for _, r := range jid.User {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return "+" + jid.User
}
