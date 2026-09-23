package utils

import (
	"context"
	"fmt"
	"html"
	"log"
	"strings"

	"watgbridge/database"
	"watgbridge/state"

	"github.com/lithammer/fuzzysearch/fuzzy"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

const querySetStatusMessage = "9152604461510864"

func WaSetStatusMessage(ctx context.Context, waClient *whatsmeow.Client, msg string) error {
	duration := state.State.Config.WhatsApp.StatusMessageDurationSeconds
	if duration == 0 {
		duration = 86400
	}

	input := map[string]any{
		"text":                   msg,
		"ephemeral_duration_sec": duration,
	}

	variables := map[string]any{
		"input": input,
	}

	_, mexErr := waClient.DangerousInternals().SendMexIQ(ctx, querySetStatusMessage, variables)
	if mexErr != nil && !strings.Contains(mexErr.Error(), "argo decoding") {
		state.State.Logger.Debug("SendMexIQ returned error when setting status message", zap.Error(mexErr))
	}

	return waClient.SetStatusMessage(ctx, msg)
}

func CleanInviteLink(input string) string {
	input = strings.TrimSpace(input)
	if idx := strings.Index(input, "chat.whatsapp.com/"); idx != -1 {
		input = input[idx+len("chat.whatsapp.com/"):]
	} else if idx := strings.Index(input, "whatsapp.com/"); idx != -1 {
		input = input[idx+len("whatsapp.com/"):]
	}
	if idx := strings.IndexAny(input, "?#"); idx != -1 {
		input = input[:idx]
	}
	return strings.Trim(input, "/")
}

func WaParseJID(s string) (types.JID, bool) {
	if s[0] == '+' {
		s = SubString(s, 1, len(s)-1)
	}

	if !strings.ContainsRune(s, '@') {
		return types.NewJID(s, types.DefaultUserServer).ToNonAD(), true
	}

	recipient, err := types.ParseJID(s)

	recipient = recipient.ToNonAD()
	if err != nil || recipient.User == "" {
		return recipient, false
	}

	return recipient, true
}

func WaContextInfoReplyChatID(contextInfo *waE2E.ContextInfo, fallback string) string {
	if contextInfo == nil {
		return fallback
	}

	if placeholderKey := contextInfo.GetPlaceholderKey(); placeholderKey != nil && placeholderKey.GetRemoteJID() != "" {
		return placeholderKey.GetRemoteJID()
	}
	if remoteJID := contextInfo.GetRemoteJID(); remoteJID != "" {
		return remoteJID
	}
	if parentGroupJID := contextInfo.GetParentGroupJID(); parentGroupJID != "" {
		return parentGroupJID
	}
	return fallback
}

func WaContextInfoReplyMessageID(contextInfo *waE2E.ContextInfo) string {
	if contextInfo == nil {
		return ""
	}

	if stanzaID := contextInfo.GetStanzaID(); stanzaID != "" {
		return stanzaID
	}
	if placeholderKey := contextInfo.GetPlaceholderKey(); placeholderKey != nil {
		return placeholderKey.GetID()
	}
	return ""
}

func WaSetReplyContext(contextInfo *waE2E.ContextInfo, stanzaID, participant, remoteJID string) {
	contextInfo.StanzaID = proto.String(stanzaID)
	contextInfo.Participant = proto.String(participant)
	contextInfo.QuotedMessage = &waE2E.Message{Conversation: proto.String("")}
	if remoteJID != "" {
		contextInfo.RemoteJID = proto.String(remoteJID)
	}
}

func WaReplyContextAllowed(destinationChatID, quotedChatID, quotedParticipantID string) bool {
	destinationChatID = waNormalizeChatID(destinationChatID)
	quotedChatID = waNormalizeChatID(quotedChatID)
	quotedParticipantID = waNormalizeChatID(quotedParticipantID)

	if quotedChatID == "" || destinationChatID == quotedChatID {
		return true
	}

	if quotedChatID == "status@broadcast" {
		return destinationChatID == quotedParticipantID
	}

	quotedChatJID, ok := WaParseJID(quotedChatID)
	if !ok || quotedChatJID.Server != types.GroupServer {
		return false
	}
	return destinationChatID == quotedParticipantID
}

func waNormalizeChatID(chatID string) string {
	if chatID == "" {
		return ""
	}

	jid, ok := WaParseJID(chatID)
	if !ok {
		return chatID
	}
	if jid.Server == types.HiddenUserServer {
		pn, err := state.State.WhatsAppClient.Store.LIDs.GetPNForLID(context.Background(), jid)
		if err == nil {
			jid = pn
		}
	}
	return jid.ToNonAD().String()
}

func WaFuzzyFindContacts(query string) (map[string]string, int, error) {
	var (
		results      = make(map[string]string)
		resultsCount = 0
	)

	contacts, err := database.ContactGetAll()
	if err != nil {
		return nil, 0, err
	}

	var searchSpace []string
	for _, contact := range contacts {
		jid := contact.ID
		if contact.FirstName != "" {
			searchSpace = append(searchSpace, jid+"||"+strings.ToLower(contact.FirstName))
		}
		if contact.FullName != "" {
			searchSpace = append(searchSpace, jid+"||"+strings.ToLower(contact.FullName))
		}
		if contact.PushName != "" {
			searchSpace = append(searchSpace, jid+"||"+strings.ToLower(contact.PushName))
		}
		if contact.BusinessName != "" {
			searchSpace = append(searchSpace, jid+"||"+strings.ToLower(contact.BusinessName))
		}
	}

	fuzzyResults := fuzzy.Find(strings.ToLower(query), searchSpace)
	for _, res := range fuzzyResults {
		info := strings.SplitN(res, "||", 2)

		contact := contacts[info[0]]
		if _, exists := results[info[0]]; exists {
			continue
		}

		resultsCount += 1
		name := ""
		if contact.FullName != "" {
			name += (contact.FullName + " (s)")
		}
		if contact.BusinessName != "" {
			if name != "" {
				name += ", "
			}
			name += (contact.BusinessName + " (b)")
		}
		if contact.PushName != "" {
			if name != "" {
				name += ", "
			}
			name += (contact.PushName + " (p)")
		}
		results[contact.ID] = name
	}

	return results, resultsCount, nil
}

func WaGetGroupName(jid types.JID) string {
	waClient := state.State.WhatsAppClient

	groupInfo, err := waClient.GetGroupInfo(context.Background(), jid)
	if err != nil {
		return jid.User
	}
	return groupInfo.Name
}

// WaPreferPN returns the phone-number JID for a user when one is known.
// primary is the JID WhatsApp addressed the message with (may be a LID);
// alt is the alternative address WhatsApp sent alongside it (SenderAlt /
// RecipientAlt), which carries the phone number for LID-addressed messages.
// A LID->PN mapping learned from alt is persisted for later lookups.
func WaPreferPN(primary, alt types.JID) types.JID {
	primary = primary.ToNonAD()
	if primary.Server != types.HiddenUserServer {
		return primary
	}
	waClient := state.State.WhatsAppClient
	if !alt.IsEmpty() && alt.Server == types.DefaultUserServer {
		pn := alt.ToNonAD()
		_ = waClient.Store.LIDs.PutLIDMapping(context.Background(), primary, pn)
		return pn
	}
	if pn, err := waClient.Store.LIDs.GetPNForLID(context.Background(), primary); err == nil && !pn.IsEmpty() {
		return pn.ToNonAD()
	}
	return primary
}

func WaGetContactName(jid types.JID) string {
	jid = jid.ToNonAD()
	if jid == state.State.WhatsAppClient.Store.ID.ToNonAD() || !jid.IsEmpty() && jid == state.State.WhatsAppClient.Store.GetLID().ToNonAD() {
		return "You"
	}

	var name string
	waClient := state.State.WhatsAppClient

	var (
		firstName    string
		fullName     string
		pushName     string
		businessName string
		found        bool
		err          error
	)

	// display is what we show in brackets: the phone number whenever the
	// LID can be resolved, otherwise the raw id.
	display := WaPreferPN(jid, types.EmptyJID)

	if display != jid {
		firstName, fullName, pushName, businessName, found, err = database.ContactNameGet(display.User, display.Server)
	}

	if !found {
		firstName, fullName, pushName, businessName, found, err = database.ContactNameGet(jid.User, jid.Server)
	}

	if err == nil && found {
		if fullName != "" {
			name = fullName + " (" + display.User + ")"
		} else if businessName != "" {
			name = businessName + " (" + display.User + ")"
		} else if pushName != "" {
			name = pushName + " (" + display.User + ")"
		} else if firstName != "" {
			name = firstName + " (" + display.User + ")"
		}
	} else {
		for _, lookup := range []types.JID{display, jid} {
			contact, cErr := waClient.Store.Contacts.GetContact(context.Background(), lookup)
			if cErr != nil || !contact.Found {
				continue
			}
			if contact.FullName != "" {
				name = contact.FullName + " (" + display.User + ")"
			} else if contact.BusinessName != "" {
				name = contact.BusinessName + " (" + display.User + ")"
			} else if contact.PushName != "" {
				name = contact.PushName + " (" + display.User + ")"
			} else if contact.FirstName != "" {
				name = contact.FirstName + " (" + display.User + ")"
			}
			if name != "" {
				break
			}
		}
	}

	if name == "" {
		name = display.User
	}

	return name
}

func WaTagAll(group types.JID, msg *waE2E.Message, msgId, msgSender string, msgIsFromMe bool) {
	var (
		cfg      = state.State.Config
		waClient = state.State.WhatsAppClient
		tgBot    = state.State.TelegramBot
	)

	groupInfo, err := waClient.GetGroupInfo(context.Background(), group)
	if err != nil {
		log.Printf("[whatsapp] failed to get group info of '%s': %s\n", group.String(), err)
		return
	}

	var (
		replyText = ""
		mentioned = []string{}
	)

	for _, participant := range groupInfo.Participants {
		if participant.JID.User == waClient.Store.ID.User {
			continue
		}

		replyText += fmt.Sprintf("@%s ", participant.JID.User)
		mentioned = append(mentioned, participant.JID.String())
	}

	contextInfo := &waE2E.ContextInfo{
		StanzaID:      proto.String(msgId),
		Participant:   proto.String(msgSender),
		QuotedMessage: msg,
		MentionedJID:  mentioned,
	}

	// Apply ephemeral settings if the chat has disappearing messages enabled
	isEphemeral, ephemeralTimer, _, err := database.GetEphemeralSettings(group.String())
	if err == nil && isEphemeral && ephemeralTimer > 0 {
		contextInfo.Expiration = &ephemeralTimer
	}

	_, err = waClient.SendMessage(context.Background(), group, &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String(replyText),
			ContextInfo: contextInfo,
		},
	})
	if err != nil {
		log.Printf("[whatsapp] failed to reply to '@all/@everyone': %s\n", err)
		return
	}

	if !msgIsFromMe {
		tagsThreadId, err := TgGetOrMakeThreadFromWa_String("mentions", cfg.Telegram.TargetChatID, "Mentions")
		if err != nil {
			TgSendErrorById(tgBot, cfg.Telegram.TargetChatID, 0, "Failed to create/retreive corresponding thread id for status/calls/tags", err)
			return
		}

		bridgedText := fmt.Sprintf("#tagall\n\nEveryone was mentioned in a group\n\n👥: <i>%s</i>",
			html.EscapeString(groupInfo.Name))

		TgSendTextById(tgBot, cfg.Telegram.TargetChatID, tagsThreadId, bridgedText)
	}
}

func WaSendText(chat types.JID, text, stanzaId, participantId string, quotedMsg *waE2E.Message, isReply bool) (whatsmeow.SendResponse, error) {
	waClient := state.State.WhatsAppClient

	msgToSend := &waE2E.Message{}
	if isReply {
		msgToSend.ExtendedTextMessage = &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String(stanzaId),
				Participant:   proto.String(participantId),
				QuotedMessage: quotedMsg,
			},
		}
	} else {
		msgToSend.Conversation = proto.String(text)
	}

	return waClient.SendMessage(context.Background(), chat, msgToSend)
}
