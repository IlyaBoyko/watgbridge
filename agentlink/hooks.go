package agentlink

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"watgbridge/database"
	"watgbridge/state"
	"watgbridge/utils"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
	"golang.org/x/exp/slices"
)

// The hooks below are the only entry points the bridge's handlers call. Each
// returns at once when the link is off; every decision is made in hub.go.

var current atomic.Pointer[Hub]

// Start builds the link from the bridge's config and runs it until ctx ends.
// With `agent.enabled: false` it opens no connection and writes nothing.
// customer is the Telegram customer bot, or nil when that bot is off; lazada is
// the Lazada channel, or nil when it is off.
func Start(ctx context.Context, customer CustomerChannel, lazada LazadaChannel) error {
	cfg := state.State.Config
	h, err := NewHub(Config{
		Enabled:          cfg.Agent.Enabled,
		URL:              cfg.Agent.URL,
		Token:            cfg.Agent.Token,
		HubID:            cfg.Agent.HubID,
		OutboxMaxAgeDays: cfg.Agent.OutboxMaxAgeDays,
	}, Deps{
		DB:              state.State.Database,
		Bridge:          bridgeState{},
		WA:              waSender{},
		Customer:        customer,
		Lazada:          lazada,
		WAPresence:      waPresence{},
		TGPresence:      typingOf(customer),
		MarkRead:        readReceiptHook(cfg),
		Topics:          topicPoster{},
		Notifier:        ownerNotifier{},
		Log:             state.State.Logger.Named("agentlink"),
		HubVersion:      state.WATGBRIDGE_VERSION,
		CustomersChatID: customersChatID(cfg),
	})
	if err != nil {
		return err
	}
	current.Store(h)
	h.Start(ctx)
	return nil
}

// customersChatID is the customers group id for hello; empty when none is set.
func customersChatID(cfg *state.Config) string {
	if cfg.Telegram.TargetChatID == 0 {
		return ""
	}
	return strconv.FormatInt(cfg.Telegram.TargetChatID, 10)
}

// readReceiptHook is the executor's MarkRead: the bridge's read-receipt helper
// when telegram.send_my_read_receipts is on, nil (off) otherwise.
func readReceiptHook(cfg *state.Config) func(chatKey string) error {
	if !cfg.Telegram.SendMyReadReceipts {
		return nil
	}
	return markChatRead
}

// Stop waits briefly for the link to wind down after its context was cancelled.
func Stop(timeout time.Duration) {
	if h := current.Load(); h != nil {
		h.Wait(timeout)
	}
}

// Enabled reports whether the agent link is on.
func Enabled() bool { return current.Load().Enabled() }

// chatKeyFor returns the chat_thread key the bridge used for a one-to-one chat
// and the JID that key was derived from. It repeats the rule of the bridge's
// own (unexported) utils.tgThreadKeyFromWa: the chat's phone number when a LID
// is known to map to one.
func chatKeyFor(info waTypes.MessageInfo) (key string, ok bool) {
	h := current.Load()
	if _, err := OneToOneJID(info.Chat.ToNonAD().String(), h.deps.Bridge.IsSelf); err != nil {
		return "", false
	}
	alt := info.MessageSource.SenderAlt
	if info.IsFromMe {
		alt = info.MessageSource.RecipientAlt
	}
	// Exactly the derivation whatsapp/helpers.go uses to pick a 1:1 chat's
	// topic: prefer the phone JID, then let utils resolve a remaining LID. Both
	// go through the same utils functions so the conversation key can never
	// drift from the ChatThreadPair key the topic was created under.
	key, err := utils.TgThreadKeyFromWa(utils.WaPreferPN(info.Chat, alt))
	if err != nil {
		h.log.Warn("agent link: could not resolve the chat key", zap.Error(err))
		return "", false
	}
	return key, true
}

// OnWhatsAppMessage is called after the bridge has handled a message event. It
// reports customer messages and staff messages sent from the WhatsApp phone
// app. text is the text the bridge extracted.
func OnWhatsAppMessage(v *events.Message, text string, isEdited bool) {
	h := current.Load()
	if !h.Enabled() {
		return
	}
	logger := h.log
	cfg := state.State.Config
	if slices.Contains(cfg.WhatsApp.IgnoreChats, v.Info.Chat.User) {
		return
	}
	key, ok := chatKeyFor(v.Info)
	if !ok {
		return
	}

	msg := v.Message
	editOf := ""
	if isEdited {
		pm := msg.GetProtocolMessage()
		msg, editOf = pm.GetEditedMessage(), pm.GetKey().GetID()
		if msg == nil {
			return
		}
	}

	var thread int64
	if id, found, err := database.ChatThreadGetTgFromWa(key, cfg.Telegram.TargetChatID); err != nil {
		logger.Warn("agent link: topic lookup failed", zap.Error(err))
	} else if found {
		thread = id
	}

	// WhatsApp events are handled one at a time, so a download that hangs
	// would stall the whole bridge. On timeout the event still goes out, with
	// the media's metadata and no data.
	dlCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	media, caption := ExtractMedia(dlCtx, state.State.WhatsAppClient, msg, logger)
	if len(media) > 0 {
		text = caption
	}
	replyTo := utils.WaContextInfoReplyMessageID(ContextInfoOf(msg))

	if v.Info.IsFromMe {
		// An edit of something the staff already said is not a new message.
		if isEdited {
			return
		}
		err := h.EmitStaffMessage(StaffInput{
			ChatKey: key, TopicID: thread, HubMsgID: v.Info.ID, Source: "phone",
			Text: text, Media: media, ReplyTo: replyTo,
		})
		if err != nil {
			logger.Error("agent link: could not queue staff message", zap.Error(err))
		}
		return
	}

	jid, _ := waTypes.ParseJID(key)
	err := h.EmitCustomerMessage(CustomerInput{
		ChatKey: key, TopicID: thread, HubMsgID: v.Info.ID, ContactName: utils.WaGetContactName(jid),
		Text: text, Media: media, ReplyTo: replyTo, EditOf: editOf,
	})
	if err != nil {
		logger.Error("agent link: could not queue customer message", zap.Error(err))
	}
}

// EmitCustomerMessage queues a customer.message from the Telegram customer
// bot. It does nothing when the link is off.
func EmitCustomerMessage(in CustomerInput) error {
	h := current.Load() // may be nil: a nil Hub is a disabled one
	if !h.Enabled() {
		return nil
	}
	return h.EmitCustomerMessage(in)
}

// EmitStaffMessage queues a staff.message for a reply a human sent in a
// customer-bot topic. It does nothing when the link is off.
func EmitStaffMessage(in StaffInput) error {
	h := current.Load()
	if !h.Enabled() {
		return nil
	}
	return h.EmitStaffMessage(in)
}

func tgUserName(u *gotgbot.User) string {
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

// quotedWaID returns the WhatsApp id of the bridged message a topic message
// replied to, or "".
func quotedWaID(msg *gotgbot.Message) string {
	if msg.ReplyToMessage == nil || msg.ReplyToMessage.ForumTopicCreated != nil {
		return ""
	}
	id, _, _, err := database.MsgIdGetWaFromTg(msg.Chat.Id, msg.ReplyToMessage.MessageId, msg.MessageThreadId)
	if err != nil {
		return ""
	}
	return id
}

func topicText(msg *gotgbot.Message) string {
	if msg.Text != "" {
		return msg.Text
	}
	return msg.Caption
}

// HandleTopicCommand intercepts /ai_* messages in a topic. It returns true when
// the message was an /ai_ command, which the caller must then not forward to
// WhatsApp. This holds with the link disabled too.
func HandleTopicCommand(b *gotgbot.Bot, c *ext.Context) bool {
	msg := c.EffectiveMessage
	if msg == nil {
		return false
	}
	text := topicText(msg)
	if _, _, ok := ParseAICommand(text); !ok {
		return false
	}

	h := current.Load() // may be nil: a nil Hub is a disabled one
	in := ControlInput{Text: text, TopicID: msg.MessageThreadId}
	if h.Enabled() {
		if key, err := database.ChatThreadGetWaFromTg(msg.Chat.Id, msg.MessageThreadId); err == nil {
			in.ChatKey = key
		}
		in.TargetWAMsgID = quotedWaID(msg)
		if _, isTg := ParseTgChatKey(in.ChatKey); isTg && h.deps.Customer != nil && msg.ReplyToMessage != nil {
			in.TargetWAMsgID = h.deps.Customer.HubMsgIDOfTopicMsg(msg.MessageThreadId, msg.ReplyToMessage.MessageId)
		}
		if msg.From != nil {
			in.AuthorID, in.AuthorName = msg.From.Id, tgUserName(msg.From)
		}
	}

	_, reply := h.InterceptAI(in)
	if reply != "" {
		if _, err := utils.TgReplyTextByContext(b, c, reply, nil, false); err != nil {
			state.State.Logger.Warn("agent link: could not answer an /ai_ command", zap.Error(err))
		}
	}
	return true
}

// OnTopicMessage is called after a topic message was sent on to WhatsApp. It
// reports it to the Agent as a staff message.
func OnTopicMessage(c *ext.Context) {
	h := current.Load()
	if !h.Enabled() {
		return
	}
	msg := c.EffectiveMessage
	// The bot's own posts (mirrors, notes) are not staff speech.
	if msg == nil || msg.From == nil || msg.From.IsBot {
		return
	}
	key, err := database.ChatThreadGetWaFromTg(msg.Chat.Id, msg.MessageThreadId)
	if err != nil || key == "" {
		return
	}
	// The pair TgSendToWhatsApp just stored: no pair means nothing reached
	// WhatsApp (a reaction, or an error already shown to the user).
	waID, _, _, err := database.MsgIdGetWaFromTg(msg.Chat.Id, msg.MessageId, msg.MessageThreadId)
	if err != nil || waID == "" {
		return
	}
	if err := h.EmitStaffMessage(StaffInput{
		ChatKey: key, TopicID: msg.MessageThreadId, HubMsgID: waID, Source: "topic",
		TgUserID: msg.From.Id, Name: tgUserName(msg.From), Text: topicText(msg), ReplyTo: quotedWaID(msg),
	}); err != nil {
		h.log.Error("agent link: could not queue staff message", zap.Error(err))
	}
}

// isStaffUser is the rule of utils.TgUpdateIsAuthorized (owner, sudo users
// and staff-group admins) without its side effect: that function answers a
// denied callback query itself, and the link needs to word that answer.
func isStaffUser(b *gotgbot.Bot, u *gotgbot.User) bool {
	return u != nil && utils.IsStaffUserID(b, u.Id)
}

// IsCardCallbackQuery tells the dispatcher which callback queries are for the
// Agent's cards.
func IsCardCallbackQuery(cq *gotgbot.CallbackQuery) bool {
	return cq != nil && (IsCardCallback(cq.Data) || IsRelayCallback(cq.Data))
}

// CardCallbackHandler handles a press on a button of one of the Agent's
// cards. It answers every press, with no text when it was passed on.
func CardCallbackHandler(b *gotgbot.Bot, c *ext.Context) error {
	cq := c.CallbackQuery
	if cq == nil {
		return nil
	}
	h := current.Load() // may be nil: a nil Hub is a disabled one
	in := CallbackInput{Data: cq.Data}
	if u := c.EffectiveSender.User; isStaffUser(b, u) {
		in.Authorized, in.UserID, in.UserName = true, u.Id, tgUserName(u)
	}
	answer := h.HandleCallback(in)
	if IsRelayCallback(cq.Data) {
		answer = h.HandleRelayCallback(in)
	}
	_, err := cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: answer})
	return err
}

// WatchForwardedMessage is called before a command sends a staff-group message
// on to WhatsApp (/send, a status reply). `forwarded` is the message as the
// bridge pairs it and `target` the chat it goes to. Call the returned function
// once the send is over: it reports a staff.message when the send worked and
// the target is a one-to-one chat. It does nothing when the link is off.
func WatchForwardedMessage(c *ext.Context, forwarded *gotgbot.Message, target waTypes.JID) (done func()) {
	h := current.Load()
	if !h.Enabled() || forwarded == nil {
		return func() {}
	}
	key, err := utils.TgThreadKeyFromWa(target)
	if err != nil {
		h.log.Warn("agent link: could not resolve the chat key", zap.Error(err))
		return func() {}
	}
	in := ForwardInput{
		TgMsgID: forwarded.MessageId, TgThreadID: forwarded.MessageThreadId,
		WaChat: target.String(), ChatKey: key, Text: topicText(forwarded),
	}
	if from := c.EffectiveMessage.From; from != nil {
		in.TgUserID, in.Name = from.Id, tgUserName(from)
	}
	return h.WatchForward(in)
}

// HandleTopicRelay intercepts staff text in a customer's topic while the
// translation relay is on for it (protocol section 5f.4). It returns true when
// the message was taken over, which the caller must then not deliver. The
// relay never delivers anything itself: the Agent answers with a `send`.
func HandleTopicRelay(b *gotgbot.Bot, c *ext.Context) bool {
	h := current.Load() // may be nil: a nil Hub is a disabled one
	if !h.Enabled() {
		return false
	}
	msg := c.EffectiveMessage
	if msg == nil || msg.MessageThreadId == 0 || msg.From == nil || msg.From.IsBot || msg.Text == "" {
		return false
	}
	if msg.Poll != nil || msg.Contact != nil || msg.Location != nil || msg.Venue != nil {
		return false
	}
	key, err := database.ChatThreadGetWaFromTg(msg.Chat.Id, msg.MessageThreadId)
	if err != nil || key == "" {
		return false
	}
	in := RelayInput{
		ChatKey: key, ChatID: msg.Chat.Id, TopicID: msg.MessageThreadId, MsgID: msg.MessageId,
		AuthorID: msg.From.Id, AuthorName: tgUserName(msg.From),
		IsStaff: isStaffUser(b, msg.From), IsBot: msg.From.IsBot,
		IsEdit: c.EditedMessage != nil, Text: msg.Text, TextOnly: true,
	}
	// The same resolution as control.target_hub_msg_id in HandleTopicCommand.
	in.ReplyTo = quotedWaID(msg)
	if _, isTg := ParseTgChatKey(key); isTg && h.deps.Customer != nil && msg.ReplyToMessage != nil {
		in.ReplyTo = h.deps.Customer.HubMsgIDOfTopicMsg(msg.MessageThreadId, msg.ReplyToMessage.MessageId)
	}
	return h.RelayTopicMessage(in)
}

// IsRelayedPost reports whether the relay took over a staff post. An edit of
// such a post changes nothing: no text of it was ever sent to the customer.
func IsRelayedPost(chatID, msgID int64) bool {
	h := current.Load()
	if !h.Enabled() || h.relay == nil || !h.relay.IsRelayPost(chatID, msgID) {
		return false
	}
	h.log.Debug("agent link: an edit of a relayed staff post is ignored")
	return true
}

// RecordTopicPost remembers the HTML of a post the bridge bot made in a topic,
// so `annotate` can add a line to it later. Posts with buttons are skipped (an
// edit would have to repeat them), and so are posts without text or caption.
func RecordTopicPost(msg *gotgbot.Message) {
	h := current.Load()
	if !h.Enabled() || h.relay == nil || msg == nil || msg.MessageId == 0 || msg.ReplyMarkup != nil {
		return
	}
	row := TopicPostRow{TgMsgID: msg.MessageId, TgChatID: msg.Chat.Id}
	switch {
	case msg.Text != "":
		row.HTML = msg.OriginalHTML()
	case msg.Caption != "":
		row.HTML, row.Caption = msg.OriginalCaptionHTML(), true
	default:
		return
	}
	if err := h.relay.SaveTopicPost(row); err != nil {
		h.log.Warn("agent link: could not remember a topic post", zap.Error(err))
	}
}
