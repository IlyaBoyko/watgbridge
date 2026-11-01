package agentlink

import (
	"context"

	"go.uber.org/zap"
)

// TopicFinder is the optional side of a channel that can say which topic post
// shows one of its messages. The Telegram customer bot and the Lazada channel
// implement it; WhatsApp is found through Bridge.TopicMsgOf.
type TopicFinder interface {
	// TopicMsgOf returns the topic post of the message a hub_msg_id names in a
	// conversation. found is false when the channel has none.
	TopicMsgOf(conversation, hubMsgID string) (topicMsgID int64, found bool, err error)
}

// setRelay stores the switch. It answers ok whether or not the value changed.
func (e *Executor) setRelay(p *SetRelay) Result {
	if e.Relay == nil {
		return fail(ErrInternal)
	}
	if err := e.Relay.SetRelay(p.Conversation, p.On); err != nil {
		e.Log.Error("agent link: could not store the relay switch", zap.Error(err))
		return fail(ErrInternal)
	}
	return Result{OK: true}
}

// findTopicPost finds the topic post of a hub_msg_id in a conversation: the
// post of a customer's message, or of a staff message sent from the phone.
func (e *Executor) findTopicPost(conversation, key string, thread int64, hubMsgID string) (int64, bool, error) {
	if _, ok := ParseTgChatKey(conversation); ok {
		if f, ok := e.Customer.(TopicFinder); ok {
			return f.TopicMsgOf(conversation, hubMsgID)
		}
		return 0, false, nil
	}
	if _, ok := ParseLzChatKey(conversation); ok {
		if f, ok := e.Lazada.(TopicFinder); ok {
			return f.TopicMsgOf(conversation, hubMsgID)
		}
		return 0, false, nil
	}
	msgID, postThread, found, err := e.Bridge.TopicMsgOf(hubMsgID, key)
	if err != nil || !found {
		return 0, false, err
	}
	// A message id of another chat is not this conversation's message.
	if postThread != 0 && thread != 0 && postThread != thread {
		return 0, false, nil
	}
	return msgID, true, nil
}

// annotate adds the English of a message to its topic post (protocol section
// 5f.2): by editing the post when the Hub made it and can rebuild it, else by
// replying to it. The customer is never touched.
func (e *Executor) annotate(ctx context.Context, p *Annotate) Result {
	if e.Relay == nil {
		return fail(ErrInternal)
	}
	key, ok := e.chatFor(p.Conversation)
	if !ok {
		return fail(ErrUnknownConversation)
	}
	thread, found, err := e.Bridge.ThreadFor(key)
	if err != nil {
		e.Log.Error("agent link: topic lookup failed", zap.Error(err))
		return fail(ErrInternal)
	}
	if !found {
		return fail(ErrUnknownConversation)
	}
	msgID, found, err := e.findTopicPost(p.Conversation, key, thread, p.HubMsgID)
	if err != nil {
		e.Log.Error("agent link: topic post lookup failed", zap.Error(err))
		return fail(ErrInternal)
	}
	if !found {
		return fail(ErrInvalid)
	}

	row, have, err := e.Relay.topicPost(msgID)
	if err != nil {
		e.Log.Warn("agent link: stored topic post lookup failed", zap.Error(err))
	}
	if have {
		limit := tgTextLimit
		if row.Caption {
			limit = tgCaptionLimit
		}
		edited := appendLine(row.HTML, translationLine(p.Text, true))
		if tgLen(edited) <= limit {
			err := e.Topics.EditPost(ctx, row.TgChatID, msgID, TopicText{Text: edited, HTML: true}, row.Caption)
			if err == nil || isNotModified(err) {
				row.HTML = edited
				if err := e.Relay.SaveTopicPost(row); err != nil {
					e.Log.Warn("agent link: could not store the edited topic post", zap.Error(err))
				}
				return Result{OK: true}
			}
			e.Log.Warn("agent link: could not edit the topic post, replying to it instead", zap.Error(err))
		}
	}

	// Older posts, posts that would grow too long, and posts the Hub did not
	// make: the translation goes under the post as a reply, shortened to fit.
	line, _ := fitTranslation(p.Text, tgTextLimit, func(tr string) string { return translationLine(tr, true) })
	if _, err := e.postText(ctx, thread, msgID, TopicText{Text: line, HTML: true}); err != nil {
		e.Log.Error("agent link: could not post the translation", zap.Error(err))
		return fail(ErrInternal)
	}
	return Result{OK: true}
}
