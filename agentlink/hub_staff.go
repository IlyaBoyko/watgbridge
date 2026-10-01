package agentlink

import (
	"strconv"

	"go.uber.org/zap"
)

/* ------------------------------------------------------- card callbacks -- */

// What staff see in the toast when a button press cannot be passed on.
const (
	CallbackNotAllowed   = "Not allowed"
	CallbackExpired      = "This card has expired"
	CallbackNotConnected = "Agent not connected"
	callbackRetry        = "Could not hand that to the agent, try again."
)

// CallbackInput is a button press on one of the Agent's cards.
type CallbackInput struct {
	Data       string // the button's callback data
	Authorized bool   // the presser is the owner or a sudo user
	UserID     int64
	UserName   string
}

// HandleCallback decides what a button press does and returns the text for
// the callback-query answer ("" for none, as the protocol asks). A press from
// an authorized user becomes a `callback` event.
func (h *Hub) HandleCallback(in CallbackInput) (answer string) {
	if !in.Authorized {
		return CallbackNotAllowed
	}
	if !h.Enabled() {
		return CallbackNotConnected
	}
	rowID, buttonID, ok := DecodeCallbackData(in.Data)
	if !ok {
		return CallbackExpired
	}
	row, found, err := h.cards.ByRowID(rowID)
	if err != nil {
		h.log.Error("agent link: card lookup failed", zap.Error(err))
		return callbackRetry
	}
	if !found {
		return CallbackExpired
	}
	p := Callback{
		Conversation: row.Conversation,
		CardID:       row.CardID,
		ButtonID:     buttonID,
		Author:       TgAuthor{TgUserID: strconv.FormatInt(in.UserID, 10), Name: in.UserName},
	}
	// A press has no source message to derive an id from, so it gets a ULID.
	if err := h.link.Emit(NewULID(h.clock.Now()), TypeCallback, p); err != nil {
		h.log.Error("agent link: could not queue callback event", zap.Error(err))
		return callbackRetry
	}
	return ""
}

/* ------------------------------------------------- forwarded by command -- */

// ForwardInput describes a staff-group message that a command (/send, a
// status reply) sends on to WhatsApp, as the Hub needs to report it.
type ForwardInput struct {
	TgMsgID    int64  // the message as the bridge pairs it (in the staff group)
	TgThreadID int64  // ... and the topic it lives in
	WaChat     string // chat JID the bridge sends it to
	ChatKey    string // the chat_thread key of that chat (its conversation)
	TgUserID   int64  // the staff member
	Name       string
	Text       string
}

// WatchForward snapshots the pairs of a message before the bridge sends it.
// The returned function, called once the send is over, reports a staff.message
// for every WhatsApp id that is new. Nothing new means nothing reached WhatsApp.
//
// The bridge's send helper shows its own errors to the user and returns nil, so
// a new pair, not an error value, is the only sign of a send that worked. It is
// also what tells the new send apart from older pairs of the same message.
func (h *Hub) WatchForward(in ForwardInput) (done func()) {
	if !h.Enabled() {
		return func() {}
	}
	// A group or broadcast chat is not a conversation: nothing to report.
	if _, err := ConversationFor(in.ChatKey, h.deps.Bridge.IsSelf); err != nil {
		return func() {}
	}
	before, err := h.deps.Bridge.PairIDsFor(in.TgMsgID, in.TgThreadID, in.WaChat)
	if err != nil {
		h.log.Warn("agent link: could not read message pairs", zap.Error(err))
		return func() {}
	}
	known := make(map[string]bool, len(before))
	for _, id := range before {
		known[id] = true
	}
	return func() {
		after, err := h.deps.Bridge.PairIDsFor(in.TgMsgID, in.TgThreadID, in.WaChat)
		if err != nil {
			h.log.Warn("agent link: could not read message pairs", zap.Error(err))
			return
		}
		var topic int64
		if id, found, err := h.deps.Bridge.ThreadFor(in.ChatKey); err != nil {
			h.log.Warn("agent link: topic lookup failed", zap.Error(err))
		} else if found {
			topic = id
		}
		for _, id := range after {
			if known[id] {
				continue
			}
			err := h.EmitStaffMessage(StaffInput{
				ChatKey: in.ChatKey, TopicID: topic, HubMsgID: id, Source: "topic",
				TgUserID: in.TgUserID, Name: in.Name, Text: in.Text,
			})
			if err != nil {
				h.log.Error("agent link: could not queue staff message", zap.Error(err))
			}
		}
	}
}
