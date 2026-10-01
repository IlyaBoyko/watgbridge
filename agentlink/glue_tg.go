package agentlink

import (
	"bytes"
	"context"
	"fmt"
	"html"

	"watgbridge/state"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Glue between the link and the Telegram bot. No decisions live here.

type topicPoster struct{}

func tgBot() (*gotgbot.Bot, error) {
	if state.State.TelegramBot == nil {
		return nil, fmt.Errorf("Telegram bot is not running")
	}
	return state.State.TelegramBot, nil
}

// The bot client sends everything as HTML, so plain text is escaped here.
func (topicPoster) PostText(_ context.Context, threadID int64, text string) (int64, error) {
	b, err := tgBot()
	if err != nil {
		return 0, err
	}
	msg, err := b.SendMessage(state.State.Config.Telegram.TargetChatID, html.EscapeString(text),
		&gotgbot.SendMessageOpts{MessageThreadId: threadID})
	if err != nil {
		return 0, err
	}
	return msg.MessageId, nil
}

func (topicPoster) PostMedia(_ context.Context, threadID int64, m OutMedia) (int64, error) {
	b, err := tgBot()
	if err != nil {
		return 0, err
	}
	chat := state.State.Config.Telegram.TargetChatID
	file := &gotgbot.FileReader{Name: m.Filename, Data: bytes.NewReader(m.Data)}
	caption := html.EscapeString(m.Caption)

	var msg *gotgbot.Message
	if m.Kind == "image" {
		msg, err = b.SendPhoto(chat, file, &gotgbot.SendPhotoOpts{Caption: caption, MessageThreadId: threadID})
	} else {
		if file.Name == "" {
			file.Name = "file"
		}
		msg, err = b.SendDocument(chat, file, &gotgbot.SendDocumentOpts{Caption: caption, MessageThreadId: threadID})
	}
	if err != nil {
		return 0, err
	}
	return msg.MessageId, nil
}

type ownerNotifier struct{}

func (ownerNotifier) NotifyOwner(text string) {
	b, err := tgBot()
	if err != nil {
		return
	}
	_, _ = b.SendMessage(state.State.Config.Telegram.OwnerID, html.EscapeString(text), &gotgbot.SendMessageOpts{})
}

func inlineMarkup(kb Keyboard) gotgbot.InlineKeyboardMarkup {
	rows := make([][]gotgbot.InlineKeyboardButton, len(kb))
	for i, row := range kb {
		rows[i] = make([]gotgbot.InlineKeyboardButton, len(row))
		for j, b := range row {
			rows[i][j] = gotgbot.InlineKeyboardButton{Text: b.Label, CallbackData: b.CallbackData}
		}
	}
	return gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (topicPoster) PostCard(_ context.Context, threadID int64, text string, kb Keyboard) (int64, int64, error) {
	b, err := tgBot()
	if err != nil {
		return 0, 0, err
	}
	opts := &gotgbot.SendMessageOpts{MessageThreadId: threadID}
	if len(kb) > 0 {
		opts.ReplyMarkup = inlineMarkup(kb)
	}
	chat := state.State.Config.Telegram.TargetChatID
	msg, err := b.SendMessage(chat, html.EscapeString(text), opts)
	if err != nil {
		return 0, 0, err
	}
	return chat, msg.MessageId, nil
}

// EditCardMessage always sends the keyboard it wants: Telegram removes the
// keyboard of an edited message when the edit does not carry one.
func (topicPoster) EditCardMessage(_ context.Context, chatID, msgID int64, text string, kb Keyboard) error {
	b, err := tgBot()
	if err != nil {
		return err
	}
	_, _, err = b.EditMessageText(html.EscapeString(text), &gotgbot.EditMessageTextOpts{
		ChatId: chatID, MessageId: msgID, ReplyMarkup: inlineMarkup(kb),
	})
	return err
}
