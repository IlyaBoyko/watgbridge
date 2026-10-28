package lzcustomer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"watgbridge/agentlink"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Notes the channel posts. They are plain: the glue escapes them for Telegram.
const (
	notePrefix = "ℹ️ "
	// topicNameMax is Telegram's limit for a topic name.
	topicNameMax = 128
)

// TopicPost is one post made by the Hub bot into the staff group. Text is
// plain; the implementation escapes it.
type TopicPost struct {
	Kind     string // text, photo or document
	Prefix   string // the bridge's own words before Text, never bold
	Text     string // the text, or the caption of a file
	Bold     bool   // Text is the buyer's own message: show it in bold
	Data     []byte
	Filename string
	Mime     string
}

// Topics is the Hub bot's side: it owns the staff group.
type Topics interface {
	CreateTopic(ctx context.Context, name string) (threadID int64, err error)
	// Post makes a post. A threadID of 0 is the group's General topic.
	Post(ctx context.Context, threadID int64, p TopicPost) (msgID int64, err error)
	// Delete removes a message of the staff group (the /lazada_auth command,
	// which holds the code).
	Delete(ctx context.Context, msgID int64) error
}

// Threads finds and stores the topic of a Lazada session, keyed `lz:<session_id>`.
type Threads interface {
	Find(key string) (threadID int64, found bool, err error)
	Add(key string, threadID int64) error
}

// Events carries what happened to the support Agent (a no-op when the link is
// off).
type Events interface {
	Customer(agentlink.CustomerInput) error
	Staff(agentlink.StaffInput) error
}

// Authorizer is the token side the channel needs. *TokenSource implements it.
type Authorizer interface {
	Authorize(ctx context.Context, code string) (Authorized, error)
	Status() (Authorized, bool)
}

// ImageFetcher downloads a buyer's picture from the URL Lazada gave.
type ImageFetcher interface {
	Fetch(ctx context.Context, url string) (data []byte, mime string, err error)
}

// Clock is the channel's only source of time, so tickers, backoff and expiry
// can be tested without waiting.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	// NewTicker returns a channel that ticks every d, and a stop function.
	NewTicker(d time.Duration) (<-chan time.Time, func())
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Options is everything a Service needs from the outside world.
type Options struct {
	API    API
	Auth   Authorizer
	DB     *gorm.DB
	Topics Topics
	// Threads and Events are the bridge's state and the agent link.
	Threads Threads
	Events  Events
	Images  ImageFetcher
	Clock   Clock
	Log     *zap.Logger

	AppKey      string
	CallbackURL string
	OwnerID     int64
	Location    *time.Location // the bridge's time_zone; UTC when nil

	// SafetyEvery fetches every session this often (lazada.safety_poll_seconds).
	// ActiveEvery does too, but only while a chat has had a message in the last
	// 10 minutes; 0 turns it off (lazada.active_poll_seconds).
	SafetyEvery time.Duration
	ActiveEvery time.Duration

	BoldCustomer bool // telegram.bold_customer_messages

	// TextLimit is the most runes one Lazada message gets (lazada.text_limit);
	// 0 means 1000.
	TextLimit int
	// PollAfterSend is how long after a send the session is fetched once, to
	// catch Lazada's content filter (LAZADA.md A6). 0 means 5 s; negative turns
	// it off.
	PollAfterSend time.Duration
}

// Service is the running Lazada channel: the fetcher, the mapping into topics
// and events, and /lazada_auth. It is the agentlink.LazadaChannel.
type Service struct {
	o     Options
	store *Store
	log   *zap.Logger
	clock Clock
	loc   *time.Location

	// pending is what triggers asked for since the worker last looked.
	mu         sync.Mutex
	pendingAll bool
	pending    map[string]bool
	wake       chan struct{}

	// backoff state is the worker's alone.
	backoff      time.Duration
	backoffUntil time.Time

	// blocked: the authorization is gone, so fetching stops until /lazada_auth.
	blocked atomic.Bool
	// rounds counts finished fetch rounds, whatever their outcome (tests wait on it).
	rounds atomic.Int64

	// opMu makes a fetch round and a send exclude each other (see SendText): a
	// message the Hub sent is in lazada_sent before any fetch can see it.
	opMu sync.Mutex

	topicMu sync.Mutex
	// keysLogged remembers which template ids had their content keys logged.
	keysLogged sync.Map

	// life ends with Stop; sends and their delayed polls refuse to start after it.
	life     context.Context
	stopLife context.CancelFunc

	cancel context.CancelFunc
	done   chan struct{}
}

var _ agentlink.LazadaChannel = (*Service)(nil)

// NewService builds the channel and migrates its tables. It fetches nothing:
// Run does, once the agent link exists, so that no message is handled before it
// can be reported.
func NewService(o Options) (*Service, error) {
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.Images == nil {
		o.Images = NewHTTPImages(nil, false)
	}
	if err := Migrate(o.DB); err != nil {
		return nil, fmt.Errorf("lazada: migrate: %w", err)
	}
	life, stopLife := context.WithCancel(context.Background())
	return &Service{
		life: life, stopLife: stopLife,
		o: o, store: NewStore(o.DB, o.Clock.Now), log: o.Log, clock: o.Clock, loc: o.Location,
		pending: map[string]bool{}, wake: make(chan struct{}, 1),
	}, nil
}

// Poll is agentlink.LazadaChannel: ask for a fetch of one session, or of every
// session with "". It returns at once. While authorization is missing nothing
// is fetched, and /lazada_auth starts a full fetch itself.
func (s *Service) Poll(sessionID string) { s.trigger(sessionID) }

func (s *Service) trigger(sessionID string) {
	if s.blocked.Load() {
		s.log.Debug("lazada: poll ignored, not authorized")
		return
	}
	s.queue(sessionID)
}

func (s *Service) queue(sessionID string) {
	s.mu.Lock()
	if sessionID == "" {
		s.pendingAll = true
	} else {
		s.pending[sessionID] = true
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// recentWindow is how recent the newest message must be for the active ticker
// to fetch.
const recentWindow = 10 * time.Minute

// Run starts the worker and the tickers and returns at once. It also fetches
// every session once, so the channel works with the agent link off.
func (s *Service) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel, s.done = cancel, make(chan struct{})

	if _, created, err := s.store.meta(s.clock.Now().UnixMilli()); err != nil {
		s.log.Error("lazada: could not read its bookkeeping row", zap.Error(err))
	} else if created {
		s.log.Info("lazada: first run, messages from before now are not fetched")
	}

	var wg sync.WaitGroup
	spawn := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	spawn(func() { s.work(ctx) })
	spawn(func() { s.tickers(ctx) })
	go func() { wg.Wait(); close(s.done) }()

	s.housekeeping(ctx)
	s.trigger("")
}

// Stop ends the worker and waits for the fetch in flight.
func (s *Service) Stop() {
	s.stopLife()
	if s.cancel == nil {
		return
	}
	s.cancel()
	<-s.done
}

// tickers feeds the worker's triggers from the clock.
func (s *Service) tickers(ctx context.Context) {
	var safety, active <-chan time.Time
	if d := s.o.SafetyEvery; d > 0 {
		c, stop := s.clock.NewTicker(d)
		defer stop()
		safety = c
	}
	if d := s.o.ActiveEvery; d > 0 {
		c, stop := s.clock.NewTicker(d)
		defer stop()
		active = c
	}
	house, stopHouse := s.clock.NewTicker(time.Hour)
	defer stopHouse()
	for {
		select {
		case <-ctx.Done():
			return
		case <-safety:
			s.trigger("")
		case <-active:
			if s.recentActivity() {
				s.trigger("")
			}
		case <-house:
			s.housekeeping(ctx)
		}
	}
}

func (s *Service) recentActivity() bool {
	newest, err := s.store.newestActivity()
	if err != nil {
		s.log.Warn("lazada: could not read the newest activity", zap.Error(err))
		return false
	}
	return newest > 0 && s.clock.Now().Sub(time.UnixMilli(newest)) < recentWindow
}

// housekeeping is what runs hourly: forgetting old sent ids, and the warning
// that the authorization is about to lapse.
func (s *Service) housekeeping(ctx context.Context) {
	if n, err := s.store.PruneSent(); err != nil {
		s.log.Warn("lazada: could not prune lazada_sent", zap.Error(err))
	} else if n > 0 {
		s.log.Debug("lazada: pruned old sent ids", zap.Int64("pruned", n))
	}
	s.checkExpiry(ctx)
}

func isNotAuthorized(err error) bool {
	return errors.Is(err, ErrAuth) || errors.Is(err, ErrNotAuthorized)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
