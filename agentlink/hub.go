package agentlink

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Config is the link's settings (the `agent:` section of the config file).
type Config struct {
	Enabled          bool
	URL              string
	Token            string
	HubID            string
	OutboxMaxAgeDays int
}

// Deps is everything the link needs from the outside world.
type Deps struct {
	DB         *gorm.DB
	Bridge     Bridge
	WA         WhatsAppSender
	Topics     TopicPoster
	Notifier   OwnerNotifier
	Clock      Clock
	Log        *zap.Logger
	HubVersion string
	// Link timing overrides, for tests. Zero means the protocol defaults.
	Link LinkOptions
}

// Hub is the link plus the event side the bridge's hooks call. A disabled Hub
// exists too: it opens no connection and writes nothing, but still recognises
// /ai_* commands so they are never forwarded to a customer.
type Hub struct {
	cfg    Config
	deps   Deps
	clock  Clock
	log    *zap.Logger
	outbox *Outbox
	mem    *CommandMemory
	cards  *CardStore
	guard  *SentGuard
	link   *Link
	exec   *Executor

	runOnce sync.Once
	done    chan struct{}
}

// NewHub builds the link. When it is enabled this migrates the link's own
// tables and drops outbox events older than outbox_max_age_days. When it is
// disabled it touches nothing.
func NewHub(cfg Config, d Deps) (*Hub, error) {
	if d.Clock == nil {
		d.Clock = realClock{}
	}
	if d.Log == nil {
		d.Log = zap.NewNop()
	}
	h := &Hub{cfg: cfg, deps: d, clock: d.Clock, log: d.Log, guard: NewSentGuard(d.Clock), done: make(chan struct{})}
	if !cfg.Enabled {
		return h, nil
	}
	if cfg.URL == "" || cfg.Token == "" || cfg.HubID == "" {
		return nil, fmt.Errorf("agent.enabled is true but agent.url, agent.token and agent.hub_id are not all set")
	}

	if err := Migrate(d.DB); err != nil {
		return nil, fmt.Errorf("agent link: migrate: %w", err)
	}
	h.outbox = NewOutbox(d.DB, d.Clock)
	h.mem = NewCommandMemory(d.DB, d.Clock)
	h.cards = NewCardStore(d.DB, d.Clock)

	if cfg.OutboxMaxAgeDays > 0 {
		cutoff := d.Clock.Now().Add(-time.Duration(cfg.OutboxMaxAgeDays) * 24 * time.Hour)
		n, err := h.outbox.DropOlderThan(cutoff)
		if err != nil {
			return nil, fmt.Errorf("agent link: outbox age cleanup: %w", err)
		}
		if n > 0 {
			h.log.Warn("agent link: dropped undelivered events older than outbox_max_age_days",
				zap.Int64("dropped", n), zap.Int("outbox_max_age_days", cfg.OutboxMaxAgeDays))
		}
	}
	h.prune()

	h.exec = &Executor{WA: d.WA, Topics: d.Topics, Bridge: d.Bridge, Memory: h.mem, Cards: h.cards, Clock: d.Clock, Guard: h.guard, Log: d.Log}
	opt := d.Link
	opt.URL, opt.Token, opt.HubID, opt.HubVersion = cfg.URL, cfg.Token, cfg.HubID, d.HubVersion
	h.link = NewLink(opt, h.outbox, h.exec, d.Notifier, d.Clock, d.Log)
	return h, nil
}

// Enabled reports whether the link is on. It is safe on a nil Hub.
func (h *Hub) Enabled() bool { return h != nil && h.link != nil }

// Link exposes the connection (tests).
func (h *Hub) Link() *Link { return h.link }

// prune forgets old command results and old cards.
func (h *Hub) prune() {
	if n, err := h.mem.Prune(); err != nil {
		h.log.Warn("agent link: could not prune command results", zap.Error(err))
	} else if n > 0 {
		h.log.Debug("agent link: pruned old command results", zap.Int64("pruned", n))
	}
	if n, err := h.cards.Prune(); err != nil {
		h.log.Warn("agent link: could not prune old cards", zap.Error(err))
	} else if n > 0 {
		h.log.Debug("agent link: pruned old cards", zap.Int64("pruned", n))
	}
}

// Start runs the link in the background until ctx ends.
func (h *Hub) Start(ctx context.Context) {
	h.runOnce.Do(func() {
		if !h.Enabled() {
			close(h.done)
			return
		}
		go func() {
			defer close(h.done)
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						h.prune()
					}
				}
			}()
			if err := h.link.Run(ctx); err != nil && ctx.Err() == nil {
				h.log.Error("agent link: not running any more", zap.Error(err))
			}
		}()
	})
}

// Wait blocks until the link has stopped or the timeout passes.
func (h *Hub) Wait(timeout time.Duration) {
	select {
	case <-h.done:
	case <-time.After(timeout):
	}
}

/* ---------------------------------------------------------------- events -- */

// CustomerInput is a customer message, already reduced to plain values.
type CustomerInput struct {
	ChatKey     string
	TopicID     int64
	HubMsgID    string
	ContactName string
	Text        string
	Media       []Media
	ReplyTo     string
	EditOf      string
}

func topicString(id int64) string { return strconv.FormatInt(id, 10) }

func nonNilMedia(m []Media) []Media {
	if m == nil {
		return []Media{}
	}
	return m
}

// EmitCustomerMessage queues a customer.message. A chat that is not a
// one-to-one chat, or a message with neither text nor media, produces nothing.
func (h *Hub) EmitCustomerMessage(in CustomerInput) error {
	if !h.Enabled() {
		return nil
	}
	conv, err := ConversationFor(in.ChatKey, h.deps.Bridge.IsSelf)
	if err != nil {
		return err
	}
	if in.Text == "" && len(in.Media) == 0 {
		return nil
	}
	p := CustomerMessage{
		Conversation: conv,
		TopicID:      topicString(in.TopicID),
		HubMsgID:     in.HubMsgID,
		Contact:      Contact{Name: in.ContactName, Phone: PhoneFor(in.ChatKey)},
		Text:         in.Text,
		Media:        nonNilMedia(in.Media),
		ReplyTo:      in.ReplyTo,
		EditOf:       in.EditOf,
	}
	return h.link.Emit(EventID(TypeCustomerMessage, conv, in.HubMsgID), TypeCustomerMessage, p)
}

// StaffInput is a message a staff member sent, from the WhatsApp phone app
// ("phone") or from the topic ("topic").
type StaffInput struct {
	ChatKey  string
	TopicID  int64
	HubMsgID string
	Source   string
	TgUserID int64
	Name     string
	Text     string
	Media    []Media
	ReplyTo  string
}

// EmitStaffMessage queues a staff.message. It never reports a message the Hub
// itself sent for the Agent.
func (h *Hub) EmitStaffMessage(in StaffInput) error {
	if !h.Enabled() {
		return nil
	}
	conv, err := ConversationFor(in.ChatKey, h.deps.Bridge.IsSelf)
	if err != nil {
		return err
	}
	if h.guard.Has(in.HubMsgID) {
		return nil
	}
	// From the phone, an empty message is a reaction or a protocol stub, not
	// a reply. From the topic or /send it already reached the customer (the
	// caller saw its WhatsApp id), and topic media is not downloaded, so a
	// photo reply arrives here with no text and no media. It must still be
	// reported: it is the signal that a human answered, which cancels an
	// /ai_after draft.
	if in.Source == "phone" && in.Text == "" && len(in.Media) == 0 {
		return nil
	}
	author := StaffAuthor{Source: in.Source, Name: in.Name}
	if in.TgUserID != 0 {
		author.TgUserID = strconv.FormatInt(in.TgUserID, 10)
	}
	p := StaffMessage{
		Conversation: conv,
		TopicID:      topicString(in.TopicID),
		HubMsgID:     in.HubMsgID,
		Author:       author,
		Text:         in.Text,
		Media:        nonNilMedia(in.Media),
		ReplyTo:      in.ReplyTo,
	}
	return h.link.Emit(EventID(TypeStaffMessage, conv, in.HubMsgID), TypeStaffMessage, p)
}

/* ------------------------------------------------------------ /ai_ ------- */

const (
	// UnavailableLine is what staff see when /ai_* cannot reach the Agent.
	UnavailableLine = "The agent isn't available here."
	unknownAILine   = "That /ai_ command isn't recognised."
)

// ParseAICommand recognises a topic message that starts with /ai_ (optionally
// /ai_xxx@botname) and splits it into the command (no slash, no bot suffix,
// lowercase) and the raw remaining text.
func ParseAICommand(text string) (command, args string, ok bool) {
	if len(text) < 4 || !strings.EqualFold(text[:4], "/ai_") {
		return "", "", false
	}
	end := strings.IndexAny(text, " \t\r\n")
	token := text[1:]
	rest := ""
	if end >= 0 {
		token = text[1:end]
		rest = text[end:]
	}
	if at := strings.IndexByte(token, '@'); at >= 0 {
		token = token[:at]
	}
	return strings.ToLower(token), strings.TrimSpace(rest), true
}

func validControlName(c string) bool {
	if !strings.HasPrefix(c, "ai_") || len(c) == 3 {
		return false
	}
	for _, r := range c {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

// ControlInput is a topic message as the /ai_ interceptor sees it.
type ControlInput struct {
	Text          string
	ChatKey       string // the chat the topic maps to; "" when the topic is not mapped
	TopicID       int64
	TargetWAMsgID string // WhatsApp id of the message the command replied to, if bridged
	AuthorID      int64
	AuthorName    string
}

// InterceptAI decides what happens to a topic message. When intercepted is
// true the message must not be forwarded to WhatsApp, whether or not the link
// is on. reply, when not empty, is one line to post back in the topic.
func (h *Hub) InterceptAI(in ControlInput) (intercepted bool, reply string) {
	command, args, ok := ParseAICommand(in.Text)
	if !ok {
		return false, ""
	}
	if !validControlName(command) {
		return true, unknownAILine
	}
	if !h.Enabled() {
		return true, UnavailableLine
	}
	conv, err := ConversationFor(in.ChatKey, h.deps.Bridge.IsSelf)
	if err != nil {
		return true, UnavailableLine
	}
	p := Control{
		Conversation:   conv,
		TopicID:        topicString(in.TopicID),
		Command:        command,
		Args:           args,
		TargetHubMsgID: in.TargetWAMsgID,
		Author:         TgAuthor{TgUserID: strconv.FormatInt(in.AuthorID, 10), Name: in.AuthorName},
	}
	if err := h.link.Emit(NewULID(h.clock.Now()), TypeControl, p); err != nil {
		h.log.Error("agent link: could not queue control event", zap.Error(err))
		return true, "Could not hand that to the agent, try again."
	}
	return true, ""
}
