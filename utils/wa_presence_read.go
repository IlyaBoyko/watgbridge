package utils

import (
	"context"
	"errors"
	"time"

	"watgbridge/database"
	"watgbridge/state"

	waTypes "go.mau.fi/whatsmeow/types"
	"go.uber.org/zap"
)

// What this file holds is the part of the WhatsApp "ticks" behaviour that more
// than one caller needs: announcing presence (whatsmeow sends inactive delivery
// receipts, shown to the sender as a single tick, unless the client announced
// PresenceAvailable) and marking a chat's unread messages read.

// waPresenceSender is the part of *whatsmeow.Client that announces presence.
type waPresenceSender interface {
	SendPresence(ctx context.Context, presence waTypes.Presence) error
}

// waReadMarker is the part of *whatsmeow.Client that sends read receipts.
type waReadMarker interface {
	MarkRead(ctx context.Context, ids []waTypes.MessageID, ts time.Time, chat, sender waTypes.JID, receiptTypeExtra ...waTypes.ReceiptType) error
}

// staffPresenceWindow is how long the account shows as online after a staff
// message when whatsapp.always_online is off.
const staffPresenceWindow = 10 * time.Second

// WaSendAvailableIfAlwaysOnline announces the account as online when
// whatsapp.always_online is on. It is called after every WhatsApp connect and
// reconnect: whatsmeow forgets the announcement with the connection.
func WaSendAvailableIfAlwaysOnline() {
	cfg, c := state.State.Config, state.State.WhatsAppClient
	if cfg == nil || !cfg.WhatsApp.AlwaysOnline || c == nil {
		return
	}
	waAnnounceAvailable(c, state.State.Logger)
}

func waAnnounceAvailable(s waPresenceSender, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.SendPresence(ctx, waTypes.PresenceAvailable); err != nil {
		logger.Warn("failed to send presence",
			zap.Error(err),
			zap.String("presence", string(waTypes.PresenceAvailable)),
		)
	}
}

// waPresenceForStaffSend is the telegram.send_my_presence behaviour: online now,
// and offline again after a short while. With whatsapp.always_online the offline
// step must not run, or it would undo the always-on announcement. `done` is
// closed once nothing is left to do (for tests); it may be nil.
func waPresenceForStaffSend(s waPresenceSender, alwaysOnline bool, window time.Duration, logger *zap.Logger, done chan<- struct{}) {
	finish := func() {
		if done != nil {
			close(done)
		}
	}
	err := s.SendPresence(context.Background(), waTypes.PresenceAvailable)
	if err != nil {
		logger.Warn("failed to send presence",
			zap.Error(err),
			zap.String("presence", string(waTypes.PresenceAvailable)),
		)
	}
	if alwaysOnline {
		finish()
		return
	}
	go func() {
		defer finish()
		time.Sleep(window)
		err := s.SendPresence(context.Background(), waTypes.PresenceUnavailable)
		if err != nil {
			logger.Warn("failed to send presence",
				zap.Error(err),
				zap.String("presence", string(waTypes.PresenceUnavailable)),
			)
		}
	}()
}

// WaMarkChatRead marks the unread incoming messages of a chat as read, on
// WhatsApp and in the bridge's database. Only a failure to list the unread
// messages is returned; a message that cannot be marked is logged and stays
// unread, to be tried again next time.
func WaMarkChatRead(chat waTypes.JID) error {
	c := state.State.WhatsAppClient
	if c == nil {
		return errors.New("WhatsApp client is not available")
	}
	unread, err := database.MsgIdGetUnread(chat.String())
	if err != nil {
		return err
	}
	selfUser := ""
	if c.Store != nil && c.Store.ID != nil {
		selfUser = c.Store.ID.User
	}
	waMarkUnreadRead(c, selfUser, chat, unread, func(msgId string) {
		database.MsgIdMarkRead(chat.String(), msgId)
	}, state.State.Logger)
	return nil
}

// waMarkUnreadRead sends one read receipt per sender. unread maps a sender JID
// to its message ids. Messages the bridge's own account sent need no receipt
// and are only marked in the database. markDone records one message as read.
func waMarkUnreadRead(m waReadMarker, selfUser string, chat waTypes.JID, unread map[string][]string, markDone func(msgId string), logger *zap.Logger) {
	for sender, msgIds := range unread {
		senderJID, _ := WaParseJID(sender)
		if selfUser != "" && senderJID.User == selfUser {
			for _, msgId := range msgIds {
				markDone(msgId)
			}
			continue
		}
		// In a one-to-one chat the sender is the chat itself and WhatsApp wants
		// no separate sender.
		if senderJID.ToNonAD().String() == chat.ToNonAD().String() {
			senderJID = waTypes.EmptyJID
		}

		err := m.MarkRead(context.Background(), msgIds, time.Now(), chat, senderJID)
		if err != nil {
			logger.Warn(
				"failed to mark messages as read",
				zap.Error(err),
				zap.String("chat_id", chat.String()),
				zap.Any("msg_ids", msgIds),
				zap.String("sender", senderJID.String()),
			)
			continue
		}
		for _, msgId := range msgIds {
			markDone(msgId)
		}
	}
}
