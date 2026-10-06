package agentlink

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// "typing..." for customers (protocol section 5d). The Agent asks for it with
// `presence`; the Hub keeps it alive one conversation at a time until the
// deadline, a paused, or the reply that ends it. It is cosmetic: nothing here
// may delay or fail a send.

const (
	// WhatsApp clears a composing state after about 25 s, Telegram a chat
	// action after 5 s.
	defaultWATick = 10 * time.Second
	defaultTGTick = 4 * time.Second
	// maxPresenceHold caps what the Agent can ask for, whatever expires_at says.
	maxPresenceHold = 3 * time.Minute
	// presenceCallTimeout bounds one call to a channel.
	presenceCallTimeout = 8 * time.Second
)

// PresenceSink takes the validated `presence` frames from the link. It is
// called from one goroutine, in arrival order.
type PresenceSink interface {
	HandlePresence(ctx context.Context, p Presence)
}

// ReplyPresence is what the Executor tells when a reply is about to go out and
// when it is over.
type ReplyPresence interface {
	// Stop ends the "typing..." of a conversation, if any.
	Stop(conversation string)
}

// WAPresence is WhatsApp's chat state. `to` is the chat JID string, resolved
// exactly as a send resolves it (a phone JID, or a LID when the chat has no
// known phone number). Only glue_wa.go implements it against whatsmeow.
type WAPresence interface {
	SetComposing(ctx context.Context, to string, composing bool) error
}

// TGPresence is the Telegram customer bot's chat action. Only the tgcustomer
// package implements it against Telegram.
type TGPresence interface {
	SendTyping(ctx context.Context, chatID int64) error
}

// PresenceOptions configures a PresenceKeeper. A nil channel turns that side
// off: its conversations are ignored. Zero timings get the protocol's.
type PresenceOptions struct {
	WA     WAPresence
	TG     TGPresence
	WATick time.Duration
	TGTick time.Duration
	// MaxHold caps how long one typing may last (3 minutes).
	MaxHold time.Duration

	// Filled in by the Hub.
	Bridge Bridge
	Clock  Clock
	Log    *zap.Logger
}

// PresenceKeeper runs one keepalive per conversation.
type PresenceKeeper struct {
	opt PresenceOptions

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	loops  map[string]*presenceLoop
	closed bool
}

type presenceLoop struct {
	cancel   context.CancelFunc
	done     chan struct{}
	deadline time.Time // guarded by PresenceKeeper.mu
}

var (
	_ PresenceSink  = (*PresenceKeeper)(nil)
	_ ReplyPresence = (*PresenceKeeper)(nil)
)

func NewPresenceKeeper(o PresenceOptions) *PresenceKeeper {
	if o.WATick <= 0 {
		o.WATick = defaultWATick
	}
	if o.TGTick <= 0 {
		o.TGTick = defaultTGTick
	}
	if o.MaxHold <= 0 {
		o.MaxHold = maxPresenceHold
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PresenceKeeper{opt: o, ctx: ctx, cancel: cancel, loops: map[string]*presenceLoop{}}
}

// presenceTarget is where a conversation's indicator is shown.
type presenceTarget struct {
	conversation string
	wa           string // the chat JID; set for WhatsApp
	tg           int64  // the customer's chat id; set for Telegram
	tick         time.Duration
}

// resolve finds where a conversation's indicator goes. It is false for a
// conversation this Hub does not know, an lz: one, or a channel that is off.
func (k *PresenceKeeper) resolve(conversation string) (presenceTarget, bool) {
	t := presenceTarget{conversation: conversation}
	var threadKey string
	if userID, ok := ParseTgChatKey(conversation); ok {
		if k.opt.TG == nil {
			return t, false
		}
		// The customer's private chat id equals the user id, as for sends.
		t.tg, t.tick, threadKey = userID, k.opt.TGTick, conversation
	} else {
		if k.opt.WA == nil {
			return t, false
		}
		// The same resolution a send uses, so a @lid chat gets the JID its
		// messages go to.
		key, err := ChatKeyFor(conversation, k.opt.Bridge.IsSelf)
		if err != nil {
			return t, false
		}
		t.wa, t.tick, threadKey = key, k.opt.WATick, key
	}
	// A conversation without a topic is not one the Hub knows (a send fails
	// the same way).
	if _, found, err := k.opt.Bridge.ThreadFor(threadKey); err != nil || !found {
		return t, false
	}
	return t, true
}

// HandlePresence applies one `presence` frame. It never fails: a frame it
// cannot act on is logged at debug level and dropped.
func (k *PresenceKeeper) HandlePresence(ctx context.Context, p Presence) {
	expires, err := time.Parse(time.RFC3339Nano, p.ExpiresAt)
	if err != nil {
		return
	}
	if p.State == PresenceTyping && k.refresh(p.Conversation, expires) {
		return
	}
	t, ok := k.resolve(p.Conversation)
	if !ok {
		k.opt.Log.Debug("agent link: ignored presence for a conversation the Hub cannot show it in",
			zap.String("conversation", p.Conversation), zap.String("state", p.State))
		return
	}
	switch p.State {
	case PresenceTyping:
		k.start(t, expires)
	case PresencePaused:
		k.Stop(p.Conversation)
		if t.wa != "" {
			k.setComposing(ctx, t, false)
		}
	}
}

// deadlineFor is expires, capped at MaxHold from now.
func (k *PresenceKeeper) deadlineFor(expires time.Time) time.Time {
	if limit := k.opt.Clock.Now().Add(k.opt.MaxHold); expires.After(limit) {
		return limit
	}
	return expires
}

// refresh moves the deadline of a loop that is already running and reports
// whether there was one.
func (k *PresenceKeeper) refresh(conversation string, expires time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	l, ok := k.loops[conversation]
	if ok {
		l.deadline = k.deadlineFor(expires)
	}
	return ok
}

// start shows the indicator now and keeps it alive.
func (k *PresenceKeeper) start(t presenceTarget, expires time.Time) {
	deadline := k.deadlineFor(expires)
	if !deadline.After(k.opt.Clock.Now()) {
		return // already over
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	if l, ok := k.loops[t.conversation]; ok {
		l.deadline = deadline
		return
	}
	ctx, cancel := context.WithCancel(k.ctx)
	l := &presenceLoop{cancel: cancel, done: make(chan struct{}), deadline: deadline}
	k.loops[t.conversation] = l
	go k.run(ctx, t, l)
}

func (k *PresenceKeeper) run(ctx context.Context, t presenceTarget, l *presenceLoop) {
	defer close(l.done)
	k.show(ctx, t)
	tick := time.NewTicker(t.tick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		k.mu.Lock()
		if !k.opt.Clock.Now().Before(l.deadline) {
			if k.loops[t.conversation] == l {
				delete(k.loops, t.conversation)
			}
			k.mu.Unlock()
			// WhatsApp is told at the end, so it does not wait for its own timeout.
			if t.wa != "" {
				k.setComposing(ctx, t, false)
			}
			return
		}
		k.mu.Unlock()
		k.show(ctx, t)
	}
}

// show makes one call to the channel. A failure is only logged: the next tick
// is the retry.
func (k *PresenceKeeper) show(ctx context.Context, t presenceTarget) {
	if t.wa != "" {
		k.setComposing(ctx, t, true)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, presenceCallTimeout)
	defer cancel()
	if err := k.opt.TG.SendTyping(cctx, t.tg); err != nil {
		k.opt.Log.Debug("agent link: could not show typing to a Telegram customer",
			zap.String("conversation", t.conversation), zap.Error(err))
	}
}

func (k *PresenceKeeper) setComposing(ctx context.Context, t presenceTarget, composing bool) {
	cctx, cancel := context.WithTimeout(ctx, presenceCallTimeout)
	defer cancel()
	if err := k.opt.WA.SetComposing(cctx, t.wa, composing); err != nil {
		k.opt.Log.Debug("agent link: could not set the WhatsApp chat state",
			zap.String("conversation", t.conversation), zap.Bool("composing", composing), zap.Error(err))
	}
}

// Stop ends one conversation's loop and returns once its last call to the
// channel is over, so the refresh cannot land after whatever the caller sends
// next. The call is cancelled, not awaited to completion, so this is quick.
func (k *PresenceKeeper) Stop(conversation string) {
	k.mu.Lock()
	l := k.loops[conversation]
	delete(k.loops, conversation)
	k.mu.Unlock()
	if l != nil {
		l.cancel()
		<-l.done
	}
}

// StopAll ends every loop and refuses new ones (shutdown).
func (k *PresenceKeeper) StopAll() {
	k.mu.Lock()
	k.closed = true
	loops := k.loops
	k.loops = map[string]*presenceLoop{}
	k.mu.Unlock()
	k.cancel()
	for _, l := range loops {
		<-l.done
	}
}

// Deadline reports when a conversation's indicator ends (tests).
func (k *PresenceKeeper) Deadline(conversation string) (time.Time, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	l, ok := k.loops[conversation]
	if !ok {
		return time.Time{}, false
	}
	return l.deadline, true
}
