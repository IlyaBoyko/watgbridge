package agentlink

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// The Hub's side of the staff relay (protocol section 5f.4 and 5f.5).

const (
	relayFailText     = "⚠️ <b>Not sent</b>: the agent is offline, so this wasn't translated."
	relayEditedText   = "✏️ <i>Not sent. Write the corrected message in this topic.</i>"
	relayWaitingText  = "⏳ <i>Not sent yet: waiting for the agent.</i>"
	relayResentText   = "⏳ <i>Sent to the agent again.</i>"
	relayCallbackPref = "hr:"
	relayRetryButton  = "r"
	relayEditButton   = "e"

	// CallbackAgentOffline is the toast of a Retry while the link is still down.
	CallbackAgentOffline = "Agent still offline"
	// CallbackTryAgain is the toast when a Retry could not be carried out.
	CallbackTryAgain = "Try again"
)

// RelayInput is a staff post in a customer's topic as the relay sees it.
type RelayInput struct {
	ChatKey    string // the chat the topic maps to; "" when it is not mapped
	ChatID     int64  // the staff group
	TopicID    int64
	MsgID      int64 // the staff post
	AuthorID   int64
	AuthorName string
	IsStaff    bool
	IsBot      bool
	IsEdit     bool
	Text       string
	// TextOnly is false for anything but plain text: media, a sticker, a poll,
	// a contact, a location.
	TextOnly bool
	ReplyTo  string // hub_msg_id of the customer message the post quoted
}

// RelayTopicMessage takes over a staff post that must not be delivered as
// written because the conversation's relay is on. It returns true when the
// post goes no further: it was handed to the Agent, or refused because the
// Agent is offline. Nothing English staff wrote reaches the customer from here.
func (h *Hub) RelayTopicMessage(in RelayInput) bool {
	if !h.Enabled() || h.relay == nil {
		return false
	}
	if !in.IsStaff || in.IsBot || in.IsEdit || !in.TextOnly || strings.TrimSpace(in.Text) == "" {
		return false
	}
	conv, err := ConversationFor(in.ChatKey, h.deps.Bridge.IsSelf)
	if err != nil {
		return false
	}
	on, err := h.relay.RelayOn(conv)
	if err == nil && !on {
		return false
	}
	if err := h.relay.markRelayPost(in.ChatID, in.MsgID); err != nil {
		h.log.Warn("agent link: could not mark the relayed post", zap.Error(err))
	}

	// A switch that cannot be read is treated like a relay that is on but cannot
	// be served: English staff wrote must never reach a customer who may be
	// getting translations, so the post is refused and Retry decides later.
	if err != nil {
		h.log.Error("agent link: relay switch lookup failed, refusing the post", zap.Error(err))
	} else if h.link.Connected() {
		if err := h.emitRelay(conv, in.TopicID, RelayID(in.ChatID, in.MsgID), in.AuthorID, in.AuthorName, in.Text, in.ReplyTo, 1); err == nil {
			h.startRelayWatchdog(in.ChatID, in.TopicID, in.MsgID)
			return true
		}
	}
	h.postRelayFailure(RelayFailRow{
		Conversation: conv, TgChatID: in.ChatID, TopicID: in.TopicID, StaffMsgID: in.MsgID,
		AuthorID: in.AuthorID, AuthorName: in.AuthorName, Text: in.Text, ReplyTo: in.ReplyTo, Attempt: 2,
	})
	return true
}

func (h *Hub) emitRelay(conv string, topic int64, relayID string, authorID int64, name, text, replyTo string, attempt int) error {
	author := StaffAuthor{Source: "topic", Name: name}
	if authorID != 0 {
		author.TgUserID = strconv.FormatInt(authorID, 10)
	}
	err := h.link.Emit(RelayEventID(conv, relayID, attempt), TypeStaffRelay, StaffRelay{
		Conversation: conv, TopicID: topicString(topic), RelayID: relayID, Author: author, Text: text, ReplyTo: replyTo,
	})
	if err != nil {
		h.log.Error("agent link: could not queue staff relay event", zap.Error(err))
	}
	return err
}

func (h *Hub) startRelayWatchdog(chatID, topic, msgID int64) {
	relayID := RelayID(chatID, msgID)
	h.relay.startWatchdog(relayID, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := h.postReply(ctx, topic, msgID, TopicText{Text: relayWaitingText, HTML: true}); err != nil {
			h.log.Warn("agent link: could not post the relay reminder", zap.Error(err))
		}
	})
}

// postReply posts into a topic as a reply, or alone when the post is gone.
func (h *Hub) postReply(ctx context.Context, topic, replyTo int64, text TopicText) (int64, error) {
	id, err := h.deps.Topics.PostTextReply(ctx, topic, replyTo, text)
	if isReplyGone(err) {
		return h.deps.Topics.PostText(ctx, topic, text)
	}
	return id, err
}

func relayFailKeyboard(rowID uint) Keyboard {
	id := strconv.FormatUint(uint64(rowID), 10)
	return Keyboard{{
		{Label: "Retry", CallbackData: relayCallbackPref + id + ":" + relayRetryButton},
		{Label: "Edit", CallbackData: relayCallbackPref + id + ":" + relayEditButton},
	}}
}

// postRelayFailure replies to the staff post with the failure post and its
// Hub-owned Retry and Edit buttons.
func (h *Hub) postRelayFailure(row RelayFailRow) {
	row, err := h.relay.createFail(row)
	if err != nil {
		h.log.Error("agent link: could not record the refused staff post", zap.Error(err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	text := TopicText{Text: relayFailText, HTML: true}
	kb := relayFailKeyboard(row.ID)
	_, msgID, err := h.deps.Topics.PostCardReply(ctx, row.TopicID, row.StaffMsgID, text, kb)
	if isReplyGone(err) {
		_, msgID, err = h.deps.Topics.PostCard(ctx, row.TopicID, text, kb)
	}
	if err != nil {
		h.log.Error("agent link: could not post the relay failure", zap.Error(err))
		_ = h.relay.deleteFail(row.ID)
		return
	}
	if err := h.relay.setFailMsg(row.ID, msgID); err != nil {
		h.log.Error("agent link: could not record the relay failure post", zap.Error(err))
	}
}

// IsRelayCallback reports whether callback data belongs to a relay failure post.
func IsRelayCallback(data string) bool { return strings.HasPrefix(data, relayCallbackPref) }

func decodeRelayCallback(data string) (rowID uint, button string, ok bool) {
	rest, found := strings.CutPrefix(data, relayCallbackPref)
	if !found {
		return 0, "", false
	}
	idPart, button, found := strings.Cut(rest, ":")
	n, err := strconv.ParseUint(idPart, 10, 64)
	if !found || err != nil || n == 0 || (button != relayRetryButton && button != relayEditButton) {
		return 0, "", false
	}
	return uint(n), button, true
}

// HandleRelayCallback runs a press on a failure post's Retry or Edit and
// returns the text for the callback-query answer.
func (h *Hub) HandleRelayCallback(in CallbackInput) string {
	if !in.Authorized {
		return CallbackNotAllowed
	}
	if !h.Enabled() || h.relay == nil {
		return CallbackNotConnected
	}
	rowID, button, ok := decodeRelayCallback(in.Data)
	if !ok {
		return CallbackExpired
	}
	row, found, err := h.relay.fail(rowID)
	if err != nil {
		h.log.Error("agent link: relay failure lookup failed", zap.Error(err))
		return callbackRetry
	}
	if !found {
		return CallbackExpired
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if button == relayEditButton {
		err := h.deps.Topics.EditCardMessage(ctx, row.TgChatID, row.FailMsgID, TopicText{Text: relayEditedText, HTML: true}, nil)
		if err != nil && !isNotModified(err) {
			h.log.Warn("agent link: could not edit the relay failure post", zap.Error(err))
			return callbackRetry
		}
		_ = h.relay.deleteFail(row.ID)
		return ""
	}

	conv := row.Conversation
	on, err := h.relay.RelayOn(conv)
	if err != nil {
		h.log.Error("agent link: relay switch lookup failed on retry", zap.Error(err))
		return CallbackTryAgain
	}
	if !on {
		// Translation was turned off meanwhile: the text goes out as written, as
		// it would have without the relay.
		if err := h.deliverAsWritten(ctx, row); err != nil {
			h.log.Warn("agent link: could not deliver the held staff post", zap.Error(err))
			return CallbackTryAgain
		}
		h.dropFailurePost(ctx, row)
		return ""
	}
	if !h.link.Connected() {
		return CallbackAgentOffline
	}
	// A repeated press makes the same event id, which the outbox keeps once.
	if err := h.emitRelay(row.Conversation, row.TopicID, RelayID(row.TgChatID, row.StaffMsgID),
		row.AuthorID, row.AuthorName, row.Text, row.ReplyTo, row.Attempt); err != nil {
		return callbackRetry
	}
	h.startRelayWatchdog(row.TgChatID, row.TopicID, row.StaffMsgID)
	h.dropFailurePost(ctx, row)
	return ""
}

// dropFailurePost deletes the failure post, or strips its buttons when Telegram
// will not delete it, and forgets the row.
func (h *Hub) dropFailurePost(ctx context.Context, row RelayFailRow) {
	err := h.deps.Topics.DeleteMessage(ctx, row.TgChatID, row.FailMsgID)
	if err != nil && !isMessageGone(err) {
		h.log.Warn("agent link: could not delete the relay failure post, editing it instead", zap.Error(err))
		_ = h.deps.Topics.EditCardMessage(ctx, row.TgChatID, row.FailMsgID, TopicText{Text: relayResentText, HTML: true}, nil)
	}
	_ = h.relay.deleteFail(row.ID)
}

// deliverAsWritten sends a held staff post to the customer exactly as the
// bridge would have without the relay, and reports it as an ordinary
// staff.message. It is only used when the switch is off at Retry time.
func (h *Hub) deliverAsWritten(ctx context.Context, row RelayFailRow) error {
	e := h.exec
	var hubID string
	switch {
	case strings.HasPrefix(row.Conversation, tgConvPrefix):
		userID, ok := ParseTgChatKey(row.Conversation)
		if !ok || e.Customer == nil {
			return errNoChannel
		}
		id, err := e.Customer.SendText(ctx, userID, CustomerText{HTML: esc(row.Text), ReplyTo: quotedCustomerMsg(userID, row.ReplyTo)})
		if err != nil {
			return err
		}
		if err := e.Customer.RecordPair(userID, id, row.TopicID, row.StaffMsgID); err != nil {
			h.log.Warn("agent link: could not pair the delivered staff post", zap.Error(err))
		}
		hubID = HubMsgIDForTelegram(userID, id)
	case strings.HasPrefix(row.Conversation, lzConvPrefix):
		session, ok := ParseLzChatKey(row.Conversation)
		if !ok || e.Lazada == nil {
			return errNoChannel
		}
		ids, err := e.Lazada.SendText(ctx, session, row.Text, "topic")
		if len(ids) == 0 {
			if err == nil {
				err = errNoChannel
			}
			return err
		}
		hubID = ids[0]
	default:
		key, err := ChatKeyFor(row.Conversation, e.Bridge.IsSelf)
		if err != nil {
			return err
		}
		var quote *Quote
		if row.ReplyTo != "" {
			participant := e.Bridge.ParticipantOf(row.ReplyTo)
			if participant == "" {
				participant = key
			}
			quote = &Quote{StanzaID: row.ReplyTo, Participant: participant}
		}
		sent, err := e.WA.SendText(ctx, key, row.Text, quote)
		if err != nil {
			return err
		}
		if err := e.Bridge.RecordPair(sent.ID, key, row.StaffMsgID, row.TopicID); err != nil {
			h.log.Warn("agent link: could not pair the delivered staff post", zap.Error(err))
		}
		hubID = sent.ID
	}
	key := strings.TrimPrefix(row.Conversation, waConvPrefix)
	if err := h.EmitStaffMessage(StaffInput{
		ChatKey: key, TopicID: row.TopicID, HubMsgID: hubID, Source: "topic",
		TgUserID: row.AuthorID, Name: row.AuthorName, Text: row.Text, ReplyTo: row.ReplyTo,
	}); err != nil {
		h.log.Error("agent link: could not queue staff message", zap.Error(err))
	}
	return nil
}

var errNoChannel = errors.New("the customer's channel is not available")
