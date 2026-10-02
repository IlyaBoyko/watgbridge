package agentlink

import (
	"fmt"
	"strconv"
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
// A Telegram customer's key (`tg:<user_id>`) is its own conversation id.
func ConversationFor(chatKey string, isSelf func(waTypes.JID) bool) (string, error) {
	if _, ok := ParseTgChatKey(chatKey); ok {
		return chatKey, nil
	}
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

/* ------------------------------------------------------ telegram customers -- */

const tgConvPrefix = "tg:"

// TgChatKey is the chat_thread key (and the conversation id) of a customer of
// the Telegram customer bot. The private chat id equals the user id.
func TgChatKey(userID int64) string { return tgConvPrefix + strconv.FormatInt(userID, 10) }

// ParseTgChatKey recognises "tg:<user_id>" and returns the user id. Ids are
// positive: negative ones are groups and channels, which are never customers.
func ParseTgChatKey(s string) (int64, bool) {
	rest, ok := strings.CutPrefix(s, tgConvPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != rest {
		return 0, false
	}
	return id, true
}
