package tgcustomer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"strings"

	"watgbridge/agentlink"
	"watgbridge/database"
	"watgbridge/state"
	"watgbridge/utils"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"go.uber.org/zap"
)

// Glue between the bridge and the two Telegram bots. No decisions live here.

func replyParams(id int64) *gotgbot.ReplyParameters {
	if id == 0 {
		return nil
	}
	// A quote of a message the user deleted must not stop the answer.
	return &gotgbot.ReplyParameters{MessageId: id, AllowSendingWithoutReply: true}
}

func markup(rows [][]agentlink.CustomerButton) gotgbot.ReplyMarkup {
	if len(rows) == 0 {
		return nil
	}
	kb := make([][]gotgbot.InlineKeyboardButton, len(rows))
	for i, row := range rows {
		kb[i] = make([]gotgbot.InlineKeyboardButton, len(row))
		for j, b := range row {
			btn := gotgbot.InlineKeyboardButton{Text: b.Text}
			switch {
			case b.CopyText != "":
				btn.CopyText = &gotgbot.CopyTextButton{Text: b.CopyText}
			case b.URL != "":
				btn.Url = b.URL
			}
			kb[i][j] = btn
		}
	}
	return gotgbot.InlineKeyboardMarkup{InlineKeyboard: kb}
}

// mapSendErr names the failures the Agent must tell apart. The customer bot
// has no retry middleware, so a 429 reaches here.
func mapSendErr(err error) error {
	var te *gotgbot.TelegramError
	if !errors.As(err, &te) {
		return err
	}
	desc := strings.ToLower(te.Description)
	switch {
	case te.Code == 429:
		return fmt.Errorf("%w: %s", agentlink.ErrCustomerRateLimited, te.Description)
	case te.Code == 403,
		te.Code == 400 && (strings.Contains(desc, "chat not found") || strings.Contains(desc, "peer_id_invalid")):
		return fmt.Errorf("%w: %s", agentlink.ErrCustomerBlocked, te.Description)
	}
	return err
}

// download fetches a file a bot can see, up to the Bot API's limit.
func download(ctx context.Context, bot *gotgbot.Bot, hc *http.Client, selfHosted bool, fileID string, size int64) ([]byte, error) {
	if size > MaxDownloadBytes {
		return nil, ErrTooBig
	}
	f, err := bot.GetFileWithContext(ctx, fileID, nil)
	if err != nil {
		var te *gotgbot.TelegramError
		if errors.As(err, &te) && strings.Contains(strings.ToLower(te.Description), "file is too big") {
			return nil, ErrTooBig
		}
		return nil, err
	}
	if selfHosted {
		return os.ReadFile(f.FilePath)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL(bot, nil), nil)
	if err != nil {
		return nil, err
	}
	res, err := hc.Do(req)
	if err != nil {
		// The URL holds the bot token.
		return nil, fmt.Errorf("download failed: %s", strings.ReplaceAll(err.Error(), bot.Token, "<token>"))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download answered %s", res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxDownloadBytes {
		return nil, ErrTooBig
	}
	return data, nil
}

/* ------------------------------------------------------- the customer bot -- */

type customerBot struct {
	bot        *gotgbot.Bot
	hc         *http.Client
	selfHosted bool
	log        *zap.Logger // may be nil
}

func (c *customerBot) Download(ctx context.Context, fileID string, size int64) ([]byte, error) {
	return download(ctx, c.bot, c.hc, c.selfHosted, fileID, size)
}

// isParseError is Telegram refusing HTML it cannot parse ("can't parse
// entities: Unsupported start tag ...").
func isParseError(err error) bool {
	var te *gotgbot.TelegramError
	return errors.As(err, &te) && te.Code == 400 && strings.Contains(strings.ToLower(te.Description), "can't parse entities")
}

func (c *customerBot) warnPlainFallback(err error) {
	log := c.log
	if log == nil {
		log = zap.NewNop()
	}
	log.Warn("Telegram refused the Agent's HTML, sending it once as plain text", zap.Error(err))
}

func (c *customerBot) SendText(ctx context.Context, chatID int64, m agentlink.CustomerText) (int64, error) {
	send := func(text, parseMode string) (*gotgbot.Message, error) {
		return c.bot.SendMessageWithContext(ctx, chatID, text, &gotgbot.SendMessageOpts{
			ParseMode: parseMode, ReplyMarkup: markup(m.Buttons), ReplyParameters: replyParams(m.ReplyTo),
		})
	}
	msg, err := send(m.HTML, "HTML")
	if err != nil && m.Formatted && isParseError(err) {
		c.warnPlainFallback(err)
		msg, err = send(agentlink.StripHTML(m.HTML), "")
	}
	if err != nil {
		return 0, mapSendErr(err)
	}
	return msg.MessageId, nil
}

func fileName(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

func (c *customerBot) SendFile(ctx context.Context, chatID int64, m agentlink.CustomerFile) (int64, error) {
	file := func(fallback string) *gotgbot.FileReader {
		return &gotgbot.FileReader{Name: fileName(m.Filename, fallback), Data: bytes.NewReader(m.Data)}
	}
	send := func(caption, parseMode string) (*gotgbot.Message, error) {
		switch m.Kind {
		case "image":
			return c.bot.SendPhotoWithContext(ctx, chatID, file("photo.jpg"), &gotgbot.SendPhotoOpts{
				Caption: caption, ParseMode: parseMode, ReplyMarkup: markup(m.Buttons), ReplyParameters: replyParams(m.ReplyTo)})
		case "video":
			return c.bot.SendVideoWithContext(ctx, chatID, file("video.mp4"), &gotgbot.SendVideoOpts{
				Caption: caption, ParseMode: parseMode, ReplyMarkup: markup(m.Buttons), ReplyParameters: replyParams(m.ReplyTo)})
		case "voice":
			return c.bot.SendVoiceWithContext(ctx, chatID, file("voice.ogg"), &gotgbot.SendVoiceOpts{
				Caption: caption, ParseMode: parseMode, ReplyMarkup: markup(m.Buttons), ReplyParameters: replyParams(m.ReplyTo)})
		case "audio":
			return c.bot.SendAudioWithContext(ctx, chatID, file("audio.mp3"), &gotgbot.SendAudioOpts{
				Caption: caption, ParseMode: parseMode, ReplyMarkup: markup(m.Buttons), ReplyParameters: replyParams(m.ReplyTo)})
		case "sticker":
			return c.bot.SendStickerWithContext(ctx, chatID, file("sticker.webp"), &gotgbot.SendStickerOpts{
				ReplyMarkup: markup(m.Buttons), ReplyParameters: replyParams(m.ReplyTo)})
		case "document":
			return c.bot.SendDocumentWithContext(ctx, chatID, file("file"), &gotgbot.SendDocumentOpts{
				Caption: caption, ParseMode: parseMode, ReplyMarkup: markup(m.Buttons), ReplyParameters: replyParams(m.ReplyTo)})
		}
		return nil, fmt.Errorf("cannot send a %q to a customer", m.Kind)
	}
	msg, err := send(m.HTMLCaption, "HTML")
	if err != nil && m.FormattedCaption && isParseError(err) {
		c.warnPlainFallback(err)
		msg, err = send(agentlink.StripHTML(m.HTMLCaption), "")
	}
	if err != nil {
		return 0, mapSendErr(err)
	}
	return msg.MessageId, nil
}

/* ----------------------------------------------------------- the Hub bot -- */

// hubTopics posts into the staff group with the bridge's own bot. Its client
// sends everything as HTML (see telegram/client.go), so plain text is escaped
// here, as agentlink's topic poster does.
type hubTopics struct{}

func hubBot() (*gotgbot.Bot, error) {
	if state.State.TelegramBot == nil {
		return nil, fmt.Errorf("Telegram bot is not running")
	}
	return state.State.TelegramBot, nil
}

func (hubTopics) Download(ctx context.Context, fileID string, size int64) ([]byte, error) {
	b, err := hubBot()
	if err != nil {
		return nil, err
	}
	return download(ctx, b, http.DefaultClient, state.State.Config.Telegram.SelfHostedAPI, fileID, size)
}

func (hubTopics) CreateTopic(ctx context.Context, name string) (int64, error) {
	b, err := hubBot()
	if err != nil {
		return 0, err
	}
	topic, err := b.CreateForumTopicWithContext(ctx, state.State.Config.Telegram.TargetChatID, name, &gotgbot.CreateForumTopicOpts{})
	if err != nil {
		return 0, err
	}
	return topic.MessageThreadId, nil
}

// topicText is the HTML for a post: Prefix and Text escaped, Text in bold when
// it is the customer's own and the post still fits limit.
func topicText(p TopicPost, limit int) string {
	prefix, body := html.EscapeString(p.Prefix), html.EscapeString(p.Text)
	if p.Bold {
		body = utils.BoldCustomerBody(prefix, body, limit)
	}
	return prefix + body
}

func (hubTopics) Post(ctx context.Context, thread int64, p TopicPost) (int64, error) {
	b, err := hubBot()
	if err != nil {
		return 0, err
	}
	chat := state.State.Config.Telegram.TargetChatID
	reply := replyParams(p.ReplyTo)
	// What the customer wrote may be bold; the bridge's own words never are.
	caption := topicText(p, utils.TgCaptionLimit)
	file := func() *gotgbot.FileReader {
		return &gotgbot.FileReader{Name: fileName(p.Filename, "file"), Data: bytes.NewReader(p.Data)}
	}

	var msg *gotgbot.Message
	switch p.Kind {
	case "text":
		msg, err = b.SendMessageWithContext(ctx, chat, topicText(p, utils.TgTextLimit), &gotgbot.SendMessageOpts{MessageThreadId: thread, ReplyParameters: reply})
	case "photo":
		msg, err = b.SendPhotoWithContext(ctx, chat, file(), &gotgbot.SendPhotoOpts{Caption: caption, MessageThreadId: thread, ReplyParameters: reply})
	case "video":
		msg, err = b.SendVideoWithContext(ctx, chat, file(), &gotgbot.SendVideoOpts{Caption: caption, MessageThreadId: thread, ReplyParameters: reply})
	case "animation":
		msg, err = b.SendAnimationWithContext(ctx, chat, file(), &gotgbot.SendAnimationOpts{Caption: caption, MessageThreadId: thread, ReplyParameters: reply})
	case "voice":
		msg, err = b.SendVoiceWithContext(ctx, chat, file(), &gotgbot.SendVoiceOpts{Caption: caption, MessageThreadId: thread, ReplyParameters: reply})
	case "audio":
		msg, err = b.SendAudioWithContext(ctx, chat, file(), &gotgbot.SendAudioOpts{Caption: caption, MessageThreadId: thread, ReplyParameters: reply})
	case "document":
		msg, err = b.SendDocumentWithContext(ctx, chat, file(), &gotgbot.SendDocumentOpts{Caption: caption, MessageThreadId: thread, ReplyParameters: reply})
	case "sticker":
		msg, err = b.SendStickerWithContext(ctx, chat, file(), &gotgbot.SendStickerOpts{MessageThreadId: thread, ReplyParameters: reply})
	case "location":
		msg, err = b.SendLocationWithContext(ctx, chat, p.Lat, p.Lon, &gotgbot.SendLocationOpts{MessageThreadId: thread, ReplyParameters: reply})
	case "contact":
		msg, err = b.SendContactWithContext(ctx, chat, p.Phone, p.First, &gotgbot.SendContactOpts{LastName: p.Last, MessageThreadId: thread, ReplyParameters: reply})
	default:
		return 0, fmt.Errorf("unknown topic post kind %q", p.Kind)
	}
	if err != nil {
		return 0, err
	}
	return msg.MessageId, nil
}

/* ----------------------------------------------------- the bridge's state -- */

// dbThreads keeps customers' topics in ChatThreadPair, under `tg:<user_id>`.
type dbThreads struct{}

func (dbThreads) Find(key string) (int64, bool, error) {
	return database.ChatThreadGetTgFromWa(key, state.State.Config.Telegram.TargetChatID)
}

func (dbThreads) Add(key string, thread int64) error {
	return database.ChatThreadAddNewPair(key, state.State.Config.Telegram.TargetChatID, thread)
}

// agentEvents hands events to the agent link (a no-op while it is off).
type agentEvents struct{}

func (agentEvents) Customer(in agentlink.CustomerInput) error {
	return agentlink.EmitCustomerMessage(in)
}
func (agentEvents) Staff(in agentlink.StaffInput) error { return agentlink.EmitStaffMessage(in) }
