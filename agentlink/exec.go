package agentlink

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"time"

	waTypes "go.mau.fi/whatsmeow/types"
	"go.uber.org/zap"
)

// Quote names the WhatsApp message a reply quotes.
type Quote struct {
	StanzaID    string
	Participant string
}

// SentMessage is what WhatsApp returned for a message the Hub sent.
type SentMessage struct {
	ID string
}

// WhatsAppSender is the sending side of WhatsApp. `to` is a chat JID string.
// Only the glue in glue_wa.go implements it against whatsmeow.
type WhatsAppSender interface {
	SendText(ctx context.Context, to, text string, quote *Quote) (SentMessage, error)
	SendImage(ctx context.Context, to string, data []byte, mime, caption string, quote *Quote) (SentMessage, error)
	SendDocument(ctx context.Context, to string, data []byte, mime, filename, caption string, quote *Quote) (SentMessage, error)
}

// OutMedia is one picture or file to post into a topic.
type OutMedia struct {
	Kind     string // "image" or "document"
	Data     []byte
	Mime     string
	Filename string
	Caption  string
}

// TopicPoster posts into a forum topic of the staff group as the bot. Text is
// plain; the implementation does whatever escaping Telegram needs. Only the
// glue in glue_tg.go implements it against gotgbot.
type TopicPoster interface {
	PostText(ctx context.Context, threadID int64, text string) (tgMsgID int64, err error)
	PostMedia(ctx context.Context, threadID int64, m OutMedia) (tgMsgID int64, err error)
}

// Bridge is the part of the bridge's own state the link needs.
type Bridge interface {
	// IsSelf reports whether jid is the bridge's own account (its chat is
	// never a conversation).
	IsSelf(jid waTypes.JID) bool
	// ThreadFor finds the existing topic of a chat. It never creates one.
	ThreadFor(chatKey string) (threadID int64, found bool, err error)
	// ParticipantOf returns the sender stored for a bridged WhatsApp message,
	// or "" when it is unknown.
	ParticipantOf(waMsgID string) string
	// RecordPair stores the WhatsApp message <-> topic message mapping for a
	// message the Hub itself sent, so staff replies to its mirror work like
	// replies to any bridged message.
	RecordPair(waMsgID, chatKey string, tgMsgID, threadID int64) error
}

// OwnerNotifier tells the bridge's owner something on Telegram.
type OwnerNotifier interface {
	NotifyOwner(text string)
}

// SentGuard remembers the WhatsApp ids this Hub sent for the Agent, so
// nothing the Agent sent can ever come back as a staff.message.
type SentGuard struct {
	mu    sync.Mutex
	clock Clock
	ids   map[string]time.Time
}

const sentGuardTTL = time.Hour

func NewSentGuard(c Clock) *SentGuard { return &SentGuard{clock: c, ids: map[string]time.Time{}} }

func (g *SentGuard) Mark(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	g.ids[id] = now
	for k, t := range g.ids {
		if now.Sub(t) > sentGuardTTL {
			delete(g.ids, k)
		}
	}
}

func (g *SentGuard) Has(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.ids[id]
	return ok && g.clock.Now().Sub(t) <= sentGuardTTL
}

// Executor runs the Agent's commands. Everything it touches is an interface.
type Executor struct {
	WA     WhatsAppSender
	Topics TopicPoster
	Bridge Bridge
	Memory *CommandMemory
	Clock  Clock
	Guard  *SentGuard
	Log    *zap.Logger
}

const (
	mirrorPrefix = "🤖 "
	notePrefix   = "ℹ️ "
)

// Handle executes one command envelope and returns its single result. A command
// id seen before returns the stored result and executes nothing.
func (e *Executor) Handle(ctx context.Context, env Envelope) Result {
	if prev, ok, err := e.Memory.Get(env.ID); err != nil {
		e.Log.Error("agent link: command memory read failed", zap.String("command_id", env.ID), zap.Error(err))
	} else if ok {
		return prev
	}

	res := e.run(ctx, env)
	res.CommandID = env.ID
	if err := e.Memory.Put(res); err != nil {
		e.Log.Error("agent link: command memory write failed", zap.String("command_id", env.ID), zap.Error(err))
	}
	return res
}

func fail(code string) Result { return Result{OK: false, Error: code} }

func (e *Executor) run(ctx context.Context, env Envelope) Result {
	payload, err := DecodePayload(env)
	if err != nil {
		e.Log.Warn("agent link: invalid command", zap.String("command_id", env.ID), zap.Error(err))
		return fail(ErrInvalid)
	}

	switch p := payload.(type) {
	case *Send:
		if e.expired(p.ExpiresAt) {
			return fail(ErrExpired)
		}
		switch p.Kind {
		case "reply":
			return e.sendReply(ctx, p)
		case "note":
			return e.sendNote(ctx, p)
		default:
			// Cards and buttons arrive with H3.
			return fail(ErrInvalid)
		}
	case *EditCard:
		if e.expired(p.ExpiresAt) {
			return fail(ErrExpired)
		}
		return fail(ErrInvalid)
	default:
		return fail(ErrInvalid)
	}
}

func (e *Executor) expired(expiresAt string) bool {
	t, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return true
	}
	return !e.Clock.Now().Before(t)
}

func (e *Executor) chatFor(conversation string) (string, bool) {
	key, err := ChatKeyFor(conversation, e.Bridge.IsSelf)
	if err != nil {
		return "", false
	}
	return key, true
}

func (e *Executor) sendNote(ctx context.Context, p *Send) Result {
	key, ok := e.chatFor(p.Conversation)
	if !ok {
		return fail(ErrUnknownConversation)
	}
	if strings.TrimSpace(p.Text) == "" {
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
	if _, err := e.Topics.PostText(ctx, thread, notePrefix+p.Text); err != nil {
		e.Log.Error("agent link: could not post note", zap.Error(err))
		return fail(ErrInternal)
	}
	return Result{OK: true}
}

func decodeOutMedia(ms []Media) ([]OutMedia, bool) {
	out := make([]OutMedia, 0, len(ms))
	for _, m := range ms {
		if (m.Kind != "image" && m.Kind != "document") || m.TooLarge || m.Data == "" {
			return nil, false
		}
		data, err := base64.StdEncoding.DecodeString(m.Data)
		if err != nil || len(data) == 0 {
			return nil, false
		}
		out = append(out, OutMedia{Kind: m.Kind, Data: data, Mime: m.Mime, Filename: m.Filename, Caption: m.Caption})
	}
	return out, true
}

func (e *Executor) sendReply(ctx context.Context, p *Send) Result {
	key, ok := e.chatFor(p.Conversation)
	if !ok {
		return fail(ErrUnknownConversation)
	}
	media, ok := decodeOutMedia(p.Media)
	if !ok || (len(media) == 0 && strings.TrimSpace(p.Text) == "") {
		return fail(ErrInvalid)
	}
	// Without a topic there is nowhere to mirror to; the conversation is not
	// one this Hub knows.
	thread, found, err := e.Bridge.ThreadFor(key)
	if err != nil {
		e.Log.Error("agent link: topic lookup failed", zap.Error(err))
		return fail(ErrInternal)
	}
	if !found {
		return fail(ErrUnknownConversation)
	}

	var copyables []Copyable
	if p.Copyables != nil {
		copyables = *p.Copyables
	}

	var quote *Quote
	if p.ReplyTo != "" {
		participant := e.Bridge.ParticipantOf(p.ReplyTo)
		if participant == "" {
			participant = key
		}
		quote = &Quote{StanzaID: p.ReplyTo, Participant: participant}
	}

	type item struct {
		media   *OutMedia
		caption string
	}
	var items []item
	if len(media) == 0 {
		items = []item{{caption: p.Text}}
	}
	for i := range media {
		m := media[i]
		caption := m.Caption
		if i == 0 && p.Text != "" {
			caption = p.Text
		}
		m.Caption = caption
		items = append(items, item{media: &m, caption: caption})
	}

	var first string
	for i, it := range items {
		// Only the first message quotes; the rest are part of the same answer.
		q := quote
		if i > 0 {
			q = nil
		}
		var sent SentMessage
		var err error
		switch {
		case it.media == nil:
			sent, err = e.WA.SendText(ctx, key, it.caption, q)
		case it.media.Kind == "image":
			sent, err = e.WA.SendImage(ctx, key, it.media.Data, it.media.Mime, it.caption, q)
		default:
			sent, err = e.WA.SendDocument(ctx, key, it.media.Data, it.media.Mime, it.media.Filename, it.caption, q)
		}
		if err != nil {
			e.Log.Warn("agent link: WhatsApp send failed", zap.Error(err))
			if i == 0 {
				return fail(ErrCustomerUnreachable)
			}
			// The customer already has part of the answer. Say so, and say that
			// the copyables (not sent yet) are still owed.
			e.noteUnsent(ctx, thread, copyables)
			return Result{OK: false, Error: ErrInternal, HubMsgID: first, DeliveredAt: FormatTS(e.Clock.Now())}
		}
		if i == 0 {
			first = sent.ID
		}
		if e.Guard != nil {
			e.Guard.Mark(sent.ID)
		}
		// One mirror post: the main content, plus the copyables as label: value lines.
		body := it.caption
		if i == 0 && len(copyables) > 0 {
			if body != "" {
				body += "\n"
			}
			body += copyableLines(copyables)
		}
		e.mirror(ctx, key, thread, sent.ID, it.media, body)
	}

	// Each copyable is its own message holding only the value, so a long-press
	// copies exactly it. Its failure does not undo the answer already sent.
	var unsent []Copyable
	for i, c := range copyables {
		if err := e.sendCopyable(ctx, key, c); err != nil {
			e.Log.Warn("agent link: WhatsApp send of a copyable failed", zap.Int("index", i), zap.Error(err))
			unsent = append(unsent, c)
		}
	}
	e.noteUnsent(ctx, thread, unsent)
	return Result{OK: true, HubMsgID: first, DeliveredAt: FormatTS(e.Clock.Now())}
}

func copyableLines(cs []Copyable) string {
	lines := make([]string, len(cs))
	for i, c := range cs {
		lines[i] = c.Label + ": " + c.Value
	}
	return strings.Join(lines, "\n")
}

func (e *Executor) sendCopyable(ctx context.Context, key string, c Copyable) error {
	sent, err := e.WA.SendText(ctx, key, c.Value, nil)
	if err != nil {
		return err
	}
	if e.Guard != nil {
		e.Guard.Mark(sent.ID)
	}
	return nil
}

// noteUnsent tells staff which values never reached the customer, so they can
// send them by hand.
func (e *Executor) noteUnsent(ctx context.Context, thread int64, unsent []Copyable) {
	if len(unsent) == 0 {
		return
	}
	text := notePrefix + "The customer did not get these values, please send them by hand:\n" + copyableLines(unsent)
	if _, err := e.Topics.PostText(ctx, thread, text); err != nil {
		e.Log.Error("agent link: could not post the note about unsent values", zap.Error(err))
	}
}

// mirror posts what was just sent to the customer into the topic. A failure
// here is logged and nothing more: the customer already has the message.
func (e *Executor) mirror(ctx context.Context, key string, thread int64, waID string, m *OutMedia, caption string) {
	var tgID int64
	var err error
	if m == nil {
		tgID, err = e.Topics.PostText(ctx, thread, mirrorPrefix+caption)
	} else {
		mm := *m
		mm.Caption = strings.TrimSpace(mirrorPrefix + caption)
		tgID, err = e.Topics.PostMedia(ctx, thread, mm)
	}
	if err != nil {
		e.Log.Error("agent link: could not mirror the reply into the topic", zap.String("wa_msg_id", waID), zap.Error(err))
		return
	}
	if err := e.Bridge.RecordPair(waID, key, tgID, thread); err != nil {
		e.Log.Error("agent link: could not record the mirror's message pair", zap.String("wa_msg_id", waID), zap.Error(err))
	}
}
