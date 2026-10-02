package tgcustomer

import (
	"context"
	"errors"
	"fmt"

	"watgbridge/agentlink"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"go.uber.org/zap"
)

// ErrUnsupported is returned for a staff message of a kind that cannot be sent
// to a customer.
var ErrUnsupported = errors.New("only text, photos, videos, documents, voice messages and stickers can be sent to a customer")

// IsServiceMessage reports a message that is the group's own housekeeping (a
// pin, a topic rename, a member joining), which is never meant for a customer.
func IsServiceMessage(msg *gotgbot.Message) bool {
	return msg.PinnedMessage != nil || msg.ForumTopicCreated != nil || msg.ForumTopicEdited != nil ||
		msg.ForumTopicClosed != nil || msg.ForumTopicReopened != nil || len(msg.NewChatMembers) > 0 ||
		msg.LeftChatMember != nil || msg.NewChatTitle != "" || len(msg.NewChatPhoto) > 0 ||
		msg.DeleteChatPhoto || msg.MessageAutoDeleteTimerChanged != nil
}

// StaffToCustomer sends a message a staff member wrote in a customer's topic to
// that customer, from the customer bot. Files belong to the Hub bot, so they
// are downloaded from it and uploaded again. On success it records the pair and
// reports the message to the Agent as staff speech; a failure is returned for
// the caller to show in the topic.
func (b *Bridge) StaffToCustomer(ctx context.Context, msg *gotgbot.Message, userID int64) error {
	thread := msg.MessageThreadId

	// Quoting a bridged message quotes the customer's original.
	var replyTo int64
	replyHub := ""
	if r := msg.ReplyToMessage; r != nil && r.ForumTopicCreated == nil {
		chat, id, found, err := b.Pairs.CustomerMsgFor(thread, r.MessageId)
		if err != nil {
			b.Log.Warn("customer bot: pair lookup", zap.Error(err))
		} else if found && chat == userID {
			replyTo, replyHub = id, agentlink.HubMsgIDForTelegram(chat, id)
		}
	}

	var (
		sentID int64
		err    error
		plain  string
	)
	switch {
	case msg.Text != "":
		plain = msg.Text
		sentID, err = b.Sender.SendText(ctx, userID, agentlink.CustomerText{HTML: msg.OriginalHTML(), ReplyTo: replyTo})
	case len(msg.Photo) > 0:
		p := largestPhoto(msg.Photo)
		sentID, err = b.sendFile(ctx, userID, msg, "image", p.FileId, p.FileSize, "photo.jpg", "image/jpeg", replyTo)
		plain = msg.Caption
	case msg.Video != nil:
		v := msg.Video
		sentID, err = b.sendFile(ctx, userID, msg, "video", v.FileId, v.FileSize, orDefault(v.FileName, "video.mp4"), orDefault(v.MimeType, "video/mp4"), replyTo)
		plain = msg.Caption
	case msg.Document != nil:
		d := msg.Document
		sentID, err = b.sendFile(ctx, userID, msg, "document", d.FileId, d.FileSize, orDefault(d.FileName, "file"), orDefault(d.MimeType, "application/octet-stream"), replyTo)
		plain = msg.Caption
	case msg.Voice != nil:
		v := msg.Voice
		sentID, err = b.sendFile(ctx, userID, msg, "voice", v.FileId, v.FileSize, "voice.ogg", orDefault(v.MimeType, "audio/ogg"), replyTo)
		plain = msg.Caption
	case msg.Sticker != nil:
		s := msg.Sticker
		name := "sticker.webp"
		switch {
		case s.IsAnimated:
			name = "sticker.tgs"
		case s.IsVideo:
			name = "sticker.webm"
		}
		sentID, err = b.sendFile(ctx, userID, msg, "sticker", s.FileId, s.FileSize, name, "", replyTo)
	default:
		return ErrUnsupported
	}
	if err != nil {
		return err
	}

	if err := b.Pairs.Record(userID, sentID, thread, msg.MessageId); err != nil {
		b.Log.Error("customer bot: could not record the message pair", zap.Error(err))
	}
	in := agentlink.StaffInput{
		ChatKey: agentlink.TgChatKey(userID), TopicID: thread,
		HubMsgID: agentlink.HubMsgIDForTelegram(userID, sentID), Source: "topic",
		Text: plain, ReplyTo: replyHub,
	}
	if msg.From != nil {
		in.TgUserID, in.Name = msg.From.Id, displayName(msg.From)
	}
	// Even a reply with no words (a photo) is reported: it is the signal that
	// a human answered.
	if err := b.Events.Staff(in); err != nil {
		b.Log.Error("customer bot: could not queue the staff message", zap.Error(err))
	}
	return nil
}

func (b *Bridge) sendFile(ctx context.Context, userID int64, msg *gotgbot.Message, kind, fileID string, size int64, filename, mime string, replyTo int64) (int64, error) {
	data, err := b.Topics.Download(ctx, fileID, size)
	if errors.Is(err, ErrTooBig) || (err == nil && int64(len(data)) > MaxDownloadBytes) {
		return 0, fmt.Errorf("the file is over the Bot API's 20 MB limit: %w", ErrTooBig)
	}
	if err != nil {
		return 0, fmt.Errorf("could not download the file from the staff group: %w", err)
	}
	return b.Sender.SendFile(ctx, userID, agentlink.CustomerFile{
		Kind: kind, Data: data, Filename: filename, Mime: mime, HTMLCaption: msg.OriginalCaptionHTML(), ReplyTo: replyTo,
	})
}
