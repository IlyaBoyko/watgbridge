package agentlink

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"watgbridge/database"
	"watgbridge/state"
	"watgbridge/utils"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waTypes "go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// This file is glue: it implements the link's small interfaces with the real
// whatsmeow client and the bridge's own state. It holds no decisions.

type waSender struct{}

func waClient() (*whatsmeow.Client, error) {
	c := state.State.WhatsAppClient
	if c == nil || c.Store == nil || c.Store.ID == nil {
		return nil, fmt.Errorf("WhatsApp is not connected")
	}
	return c, nil
}

// contextInfo builds the quote and disappearing-message settings for a message
// to jid, the same way the Telegram -> WhatsApp path does.
func contextInfo(jid waTypes.JID, q *Quote) *waE2E.ContextInfo {
	ci := &waE2E.ContextInfo{}
	if q != nil {
		utils.WaSetReplyContext(ci, q.StanzaID, q.Participant, "")
	}
	if isEphemeral, timer, _, err := database.GetEphemeralSettings(jid.String()); err == nil && isEphemeral {
		ci.Expiration = &timer
	}
	return ci
}

func (waSender) SendText(ctx context.Context, to, text string, q *Quote) (SentMessage, error) {
	c, err := waClient()
	if err != nil {
		return SentMessage{}, err
	}
	jid, err := waTypes.ParseJID(to)
	if err != nil {
		return SentMessage{}, err
	}
	msg := &waE2E.Message{}
	ci := contextInfo(jid, q)
	if q != nil || ci.Expiration != nil {
		msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: ci}
	} else {
		msg.Conversation = proto.String(text)
	}
	resp, err := c.SendMessage(ctx, jid, msg)
	return SentMessage{ID: resp.ID}, err
}

func (waSender) SendImage(ctx context.Context, to string, data []byte, mime, caption string, q *Quote) (SentMessage, error) {
	c, err := waClient()
	if err != nil {
		return SentMessage{}, err
	}
	jid, err := waTypes.ParseJID(to)
	if err != nil {
		return SentMessage{}, err
	}
	up, err := c.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return SentMessage{}, err
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	resp, err := c.SendMessage(ctx, jid, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption:           proto.String(caption),
		URL:               proto.String(up.URL),
		DirectPath:        proto.String(up.DirectPath),
		MediaKey:          up.MediaKey,
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		Mimetype:          proto.String(mime),
		FileEncSHA256:     up.FileEncSHA256,
		FileSHA256:        up.FileSHA256,
		FileLength:        proto.Uint64(uint64(len(data))),
		ContextInfo:       contextInfo(jid, q),
	}})
	return SentMessage{ID: resp.ID}, err
}

func (waSender) SendDocument(ctx context.Context, to string, data []byte, mime, filename, caption string, q *Quote) (SentMessage, error) {
	c, err := waClient()
	if err != nil {
		return SentMessage{}, err
	}
	jid, err := waTypes.ParseJID(to)
	if err != nil {
		return SentMessage{}, err
	}
	up, err := c.Upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return SentMessage{}, err
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	resp, err := c.SendMessage(ctx, jid, &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Caption:       proto.String(caption),
		Title:         proto.String(filename),
		FileName:      proto.String(filename),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		Mimetype:      proto.String(mime),
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(uint64(len(data))),
		ContextInfo:   contextInfo(jid, q),
	}})
	return SentMessage{ID: resp.ID}, err
}

// markChatRead is Executor.MarkRead on the bridge's own read-receipt helper,
// the one the Telegram send path uses.
func markChatRead(chatKey string) error {
	jid, err := waTypes.ParseJID(chatKey)
	if err != nil {
		return err
	}
	return utils.WaMarkChatRead(jid)
}

// bridgeState implements Bridge on the bridge's own database and clients.
type bridgeState struct{}

func (bridgeState) IsSelf(jid waTypes.JID) bool {
	c := state.State.WhatsAppClient
	if c == nil || c.Store == nil {
		return false
	}
	if c.Store.ID != nil && jid.User == c.Store.ID.User && jid.Server == c.Store.ID.Server {
		return true
	}
	if lid := c.Store.GetLID(); !lid.IsEmpty() && jid.User == lid.User && jid.Server == lid.Server {
		return true
	}
	return false
}

func (bridgeState) ThreadFor(chatKey string) (int64, bool, error) {
	return database.ChatThreadGetTgFromWa(chatKey, state.State.Config.Telegram.TargetChatID)
}

func (bridgeState) ParticipantOf(waMsgID string) string {
	var pairs []database.MsgIdPair
	if err := state.State.Database.Where("id = ?", waMsgID).Limit(1).Find(&pairs).Error; err != nil || len(pairs) == 0 {
		return ""
	}
	return pairs[0].ParticipantId
}

func (bridgeState) RecordPair(waMsgID, chatKey string, tgMsgID, threadID int64) error {
	c, err := waClient()
	if err != nil {
		return err
	}
	return database.MsgIdAddNewPair(waMsgID, c.Store.ID.String(), chatKey,
		state.State.Config.Telegram.TargetChatID, tgMsgID, threadID)
}

func (bridgeState) PairIDsFor(tgMsgID, tgThreadID int64, waChat string) ([]string, error) {
	var pairs []database.MsgIdPair
	err := state.State.Database.
		Where("tg_chat_id = ? AND tg_msg_id = ? AND tg_thread_id = ? AND wa_chat_id = ?",
			state.State.Config.Telegram.TargetChatID, tgMsgID, tgThreadID, waChat).
		Find(&pairs).Error
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(pairs))
	for i, p := range pairs {
		ids[i] = p.ID
	}
	return ids, nil
}
