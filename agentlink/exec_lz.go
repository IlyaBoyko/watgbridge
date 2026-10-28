package agentlink

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// sendReplyLazada delivers a `reply` to a Lazada buyer (LAZADA.md A4). Lazada
// takes plain text only, so media, copyables and a link are refused up front:
// nothing is sent, and the Agent learns that from `invalid`. reply_to and title
// mean nothing there and are ignored.
func (e *Executor) sendReplyLazada(ctx context.Context, p *Send) Result {
	session, _ := ParseLzChatKey(p.Conversation)
	if e.Lazada == nil {
		return fail(ErrUnknownConversation)
	}
	if len(p.Media) > 0 || (p.Copyables != nil && len(*p.Copyables) > 0) || p.Link != nil {
		return fail(ErrInvalid)
	}
	text := p.Text
	if p.Format == FormatHTML {
		// Lazada shows no markup; the tags would arrive as characters.
		text = StripHTML(text)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fail(ErrInvalid)
	}
	// Without a topic there is nowhere to mirror to; the Hub does not know this
	// session.
	thread, found, err := e.Bridge.ThreadFor(p.Conversation)
	if err != nil {
		e.Log.Error("agent link: topic lookup failed", zap.Error(err))
		return fail(ErrInternal)
	}
	if !found {
		return fail(ErrUnknownConversation)
	}

	ids, sendErr := e.Lazada.SendText(ctx, session, text, "agent")
	if sendErr != nil {
		e.Log.Warn("agent link: Lazada send failed", zap.Int("parts_sent", len(ids)), zap.Error(sendErr))
	}
	if len(ids) == 0 {
		if sendErr == nil {
			sendErr = errors.New("lazada sent nothing")
		}
		return fail(customerErrCode(sendErr))
	}
	if e.Guard != nil {
		for _, id := range ids {
			e.Guard.Mark(id)
		}
	}

	// Mirror what actually went out: on a partial send that is the parts that
	// made it.
	sent := text
	var partial *LazadaPartialError
	if errors.As(sendErr, &partial) {
		sent = partial.SentText
	}
	mirrored := e.mirrorLazada(ctx, thread, sent, p.Signature)

	if sendErr != nil {
		e.noteLazadaPartial(ctx, thread, len(ids), partial, sendErr)
		// The card stays: the reply did not complete.
		return Result{OK: false, Error: customerErrCode(sendErr), HubMsgID: ids[0], DeliveredAt: FormatTS(e.Clock.Now())}
	}
	e.replaceCard(ctx, p, mirrored)
	return Result{OK: true, HubMsgID: ids[0], DeliveredAt: FormatTS(e.Clock.Now())}
}

// mirrorLazada posts what was sent into the topic, the way the Telegram path
// does: the robot prefix, then the signature on a line of its own. A failure is
// logged and nothing more, since the buyer already has the message. It reports
// whether the mirror is in the topic.
func (e *Executor) mirrorLazada(ctx context.Context, thread int64, text, signature string) bool {
	post, _ := renderMirror(mirrorParts{Text: text, Signature: signature}, tgTextLimit)
	if _, err := e.Topics.PostText(ctx, thread, post); err != nil {
		e.Log.Error("agent link: could not mirror the Lazada reply into the topic", zap.Error(err))
		return false
	}
	return true
}

// noteLazadaPartial tells staff how much of a reply reached Lazada, so they can
// send the rest from Seller Center.
func (e *Executor) noteLazadaPartial(ctx context.Context, thread int64, sent int, partial *LazadaPartialError, err error) {
	total, reason := sent+1, err
	if partial != nil {
		total, reason = partial.Total, partial.Err
	}
	text := notePrefix + "Only " + strconv.Itoa(sent) + " of " + strconv.Itoa(total) + " parts reached Lazada: " + oneLineReason(reason)
	if _, err := e.Topics.PostText(ctx, thread, plainTopic(text)); err != nil {
		e.Log.Error("agent link: could not post the note about a partial Lazada reply", zap.Error(err))
	}
}

// oneLineReason is an error's words on one line.
func oneLineReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	return strings.Join(strings.Fields(err.Error()), " ")
}
