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

	chatID, msgID, err := e.Topics.PostCard(ctx, thread, p.Text, kb)
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

	if err := e.Topics.EditCardMessage(ctx, row.TgChatID, row.TgMsgID, p.Text, kb); err != nil && !isNotModified(err) {
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

// isNotModified recognises Telegram's refusal to edit a message to what it
// already says, which is the outcome the Agent asked for.
func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}
