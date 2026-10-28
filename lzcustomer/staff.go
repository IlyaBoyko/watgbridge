package lzcustomer

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"watgbridge/agentlink"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"go.uber.org/zap"
)

// What staff are told when something typed in a Lazada topic does not go out.
const (
	MediaNote         = "Images and files can't be sent to Lazada from here yet. Use Seller Center."
	TextOnlyNote      = "Only text can be sent to Lazada from here."
	EditNote          = "Edits can't be sent to Lazada; send a new message."
	NotAuthorizedNote = "Lazada is not authorized; send /lazada_auth first."
	RateLimitedNote   = "Lazada is rate limiting the shop right now; try again in a minute."
	OffNote           = "Lazada is turned off (lazada.enabled), so this was not sent to Lazada."
)

// StaffOutcome is what the topic hook does after a staff message: say Note in
// the topic (when not empty), and show the bridge's usual confirmation when
// Confirmed.
type StaffOutcome struct {
	Note      string
	Confirmed bool
}

func hasMedia(msg *gotgbot.Message) bool {
	return len(msg.Photo) > 0 || msg.Document != nil || msg.Video != nil || msg.Voice != nil ||
		msg.Sticker != nil || msg.Audio != nil || msg.Animation != nil || msg.VideoNote != nil
}

func displayName(u *gotgbot.User) string {
	if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
		return n
	}
	return u.Username
}

// StaffToBuyer sends what a staff member typed in a Lazada topic to the buyer,
// and reports it to the Agent as staff speech with source "topic". Only plain
// text goes out; Lazada's media send is not built yet (A4). A message that
// partly went out is still reported for the part that did.
func (s *Service) StaffToBuyer(ctx context.Context, msg *gotgbot.Message, sessionID string) StaffOutcome {
	if msg.Text == "" {
		if hasMedia(msg) {
			return StaffOutcome{Note: MediaNote}
		}
		return StaffOutcome{Note: TextOnlyNote}
	}

	ids, err := s.SendText(ctx, sessionID, msg.Text, OriginTopic)
	if len(ids) > 0 {
		sent := msg.Text
		var partial *agentlink.LazadaPartialError
		if errors.As(err, &partial) {
			sent = partial.SentText
		}
		s.reportStaff(msg, sessionID, ids[0], sent)
	}
	if err == nil {
		return StaffOutcome{Confirmed: true}
	}
	s.log.Warn("lazada: send from a topic failed", zap.Int("parts_sent", len(ids)), zap.Error(err))
	return StaffOutcome{Note: failureNote(err)}
}

// reportStaff tells the Agent a human answered. The id is the first Lazada
// message the buyer received (protocol section 4).
func (s *Service) reportStaff(msg *gotgbot.Message, sessionID, hubMsgID, text string) {
	in := agentlink.StaffInput{
		ChatKey: agentlink.LzChatKey(sessionID), TopicID: msg.MessageThreadId,
		HubMsgID: hubMsgID, Source: "topic", Text: text,
	}
	if msg.From != nil {
		in.TgUserID, in.Name = msg.From.Id, displayName(msg.From)
	}
	if err := s.o.Events.Staff(in); err != nil {
		s.log.Error("lazada: could not queue the staff message", zap.Error(err))
	}
}

// failureNote words a failed send for staff.
func failureNote(err error) string {
	var partial *agentlink.LazadaPartialError
	if errors.As(err, &partial) {
		return "Only " + strconv.Itoa(partial.Sent) + " of " + strconv.Itoa(partial.Total) +
			" parts reached Lazada: " + oneLine(partial.Err.Error(), 300)
	}
	switch {
	case errors.Is(err, agentlink.ErrCustomerBlocked):
		return NotAuthorizedNote
	case errors.Is(err, agentlink.ErrCustomerRateLimited):
		return RateLimitedNote
	}
	return "Cannot send to Lazada: " + oneLine(err.Error(), 300)
}
