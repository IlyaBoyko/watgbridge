package agentlink

import (
	"context"
	"strings"

	"go.uber.org/zap"
)

// sendCard posts a card in the conversation's topic. Nothing reaches the
// customer, so there is no hub_msg_id in the result.
func (e *Executor) sendCard(ctx context.Context, p *Send) Result {
	key, ok := e.chatFor(p.Conversation)
	if !ok {
		return fail(ErrUnknownConversation)
	}
	if p.CardID == "" || strings.TrimSpace(p.Text) == "" || len(p.Media) > 0 {
		return fail(ErrInvalid)
	}
	thread, found, err := e.Bridge.ThreadFor(key)
	if err != nil {
		e.Log.Error("agent link: topic lookup failed", zap.Error(err))
		return fail(ErrInternal)
	}
	if !found {
		return fail(ErrUnknownConversation)
	}

	var buttons Buttons
	if p.Buttons != nil {
		buttons = *p.Buttons
	}

	// The row comes first because its id is in the buttons' callback data. A
	// card id seen before is an Agent bug, not a retry: genuine retries are
	// answered by the command memory before they get here.
	row, err := e.Cards.Create(CardRow{
		CardID: p.CardID, Conversation: p.Conversation, TgThreadID: thread, Keyboard: encodeButtons(buttons),
	})
	if err == ErrCardExists {
		return fail(ErrInvalid)
	}
	if err != nil {
		e.Log.Error("agent link: could not record the card", zap.Error(err))
		return fail(ErrInternal)
	}
	kb, err := BuildKeyboard(row.ID, buttons)
	if err != nil {
		e.dropCard(row.ID)
		e.Log.Warn("agent link: card buttons do not fit Telegram", zap.Error(err))
		return fail(ErrInvalid)
	}

	chatID, msgID, err := e.Topics.PostCard(ctx, thread, renderCard(p.Title, p.Text), kb)
	if err != nil {
		// Nothing was posted, so the card id must stay usable.
		e.dropCard(row.ID)
		e.Log.Error("agent link: could not post the card", zap.Error(err))
		return fail(ErrInternal)
	}
	if err := e.Cards.SetMessage(row.ID, chatID, msgID); err != nil {
		e.Log.Error("agent link: could not record where the card was posted", zap.Error(err))
		return fail(ErrInternal)
	}
	return Result{OK: true}
}

func (e *Executor) dropCard(id uint) {
	if err := e.Cards.Delete(id); err != nil {
		e.Log.Error("agent link: could not remove the card record", zap.Error(err))
	}
}

// editCard replaces a card's text, and its buttons when the command carries
// them. `buttons: []` removes the keyboard; no `buttons` keeps it.
func (e *Executor) editCard(ctx context.Context, p *EditCard) Result {
	if strings.TrimSpace(p.Text) == "" {
		return fail(ErrInvalid)
	}
	row, found, err := e.Cards.ByCardID(p.CardID)
	if err != nil {
		e.Log.Error("agent link: card lookup failed", zap.Error(err))
		return fail(ErrInternal)
	}
	// A card id under another conversation is not the card the Agent means.
	if !found || row.TgMsgID == 0 || row.Conversation != p.Conversation {
		return fail(ErrUnknownCard)
	}

	buttons := decodeButtons(row.Keyboard)
	if p.Buttons != nil {
		buttons = *p.Buttons
	}
	kb, err := BuildKeyboard(row.ID, buttons)
	if err != nil {
		e.Log.Warn("agent link: card buttons do not fit Telegram", zap.Error(err))
		return fail(ErrInvalid)
	}

	if err := e.Topics.EditCardMessage(ctx, row.TgChatID, row.TgMsgID, renderCard(p.Title, p.Text), kb); err != nil && !isNotModified(err) {
		e.Log.Error("agent link: could not edit the card", zap.Error(err))
		return fail(ErrInternal)
	}
	if p.Buttons != nil {
		if err := e.Cards.SetKeyboard(row.ID, encodeButtons(buttons)); err != nil {
			e.Log.Error("agent link: could not record the card's new buttons", zap.Error(err))
		}
	}
	return Result{OK: true}
}

// replaceCard retires the draft a reply was sent from (protocol section 5b),
// once the reply is out: the topic then shows one signed mirror instead of a
// card plus a mirror. It never fails the command, since the customer already
// has the reply.
//
// When Telegram refuses the delete (or the mirror did not make it into the
// topic, so deleting would leave no trace of the reply), the card is edited to
// the signature with no buttons, so it can no longer be acted on.
func (e *Executor) replaceCard(ctx context.Context, p *Send, mirrored bool) {
	if p.ReplacesCard == "" {
		return
	}
	row, found, err := e.Cards.ByCardID(p.ReplacesCard)
	if err != nil {
		e.Log.Error("agent link: card lookup failed", zap.String("card_id", p.ReplacesCard), zap.Error(err))
		return
	}
	// Same rule as edit_card: a card id under another conversation is not the
	// card the Agent means.
	if !found || row.TgMsgID == 0 || row.Conversation != p.Conversation {
		e.Log.Warn("agent link: replaces_card names no known card", zap.String("card_id", p.ReplacesCard))
		return
	}

	if mirrored {
		err := e.Topics.DeleteMessage(ctx, row.TgChatID, row.TgMsgID)
		if err == nil || isMessageGone(err) {
			e.dropCard(row.ID)
			return
		}
		e.Log.Warn("agent link: could not delete the replaced card, editing it instead", zap.String("card_id", p.ReplacesCard), zap.Error(err))
	}
	if err := e.Topics.EditCardMessage(ctx, row.TgChatID, row.TgMsgID, renderSigned(p.Signature), nil); err != nil && !isNotModified(err) {
		e.Log.Error("agent link: could not edit the replaced card", zap.String("card_id", p.ReplacesCard), zap.Error(err))
		return
	}
	if err := e.Cards.SetKeyboard(row.ID, ""); err != nil {
		e.Log.Error("agent link: could not record the replaced card's empty buttons", zap.Error(err))
	}
}

// isMessageGone recognises Telegram saying the message is already deleted
// (staff removed the card by hand), which is the outcome the Agent asked for.
func isMessageGone(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message to delete not found")
}

// isNotModified recognises Telegram's refusal to edit a message to what it
// already says, which is the outcome the Agent asked for.
func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}
