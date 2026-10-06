package agentlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// CommandHandler executes one Agent command and returns its result.
type CommandHandler interface {
	Handle(ctx context.Context, env Envelope) Result
}

// LinkOptions configures the connection. Zero timing values get the protocol's
// defaults; tests shrink them.
type LinkOptions struct {
	URL        string
	Token      string
	HubID      string
	HubVersion string
	Channels   []string
	// Features is what hello declares (protocol section 5d). Empty declares none.
	Features []string
	// Presence receives the validated `presence` frames. Nil drops them.
	Presence PresenceSink

	PingInterval     time.Duration // protocol: 30 s
	DeadAfter        time.Duration // protocol: 90 s of silence
	HandshakeTimeout time.Duration
	BackoffMin       time.Duration // protocol: 1 s
	BackoffMax       time.Duration // protocol: 60 s
}

func (o *LinkOptions) fill() {
	if o.PingInterval <= 0 {
		o.PingInterval = 30 * time.Second
	}
	if o.DeadAfter <= 0 {
		o.DeadAfter = 90 * time.Second
	}
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = 20 * time.Second
	}
	if o.BackoffMin <= 0 {
		o.BackoffMin = time.Second
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 60 * time.Second
	}
	if len(o.Channels) == 0 {
		o.Channels = []string{"wa", "tg"}
	}
}

// StopError ends the reconnect loop: the Agent told us not to come back.
type StopError struct {
	Code   int
	Reason string
	Notify bool // tell the owner
}

func (e *StopError) Error() string {
	return fmt.Sprintf("agent link stopped by close code %d: %s", e.Code, e.Reason)
}

// maxFrameBytes bounds one inbound frame. Media is at most 5 MiB before
// base64, so 16 MiB is generous; the library default (32 KiB) is far too small.
const maxFrameBytes = 16 << 20

// Link is the Hub's side of the Hub <-> Agent connection. It dials out,
// handshakes, replays the outbox, streams live events and executes commands.
type Link struct {
	opt      LinkOptions
	outbox   *Outbox
	cmds     CommandHandler
	notifier OwnerNotifier
	clock    Clock
	log      *zap.Logger
	rnd      func() float64

	wake  chan struct{} // an event was queued
	outCh chan []byte   // frames that are not outbox events: results, errors
	cmdCh chan Envelope
	// presenceCh feeds the presence worker, apart from cmdCh: "typing..." must
	// never wait behind a send, and a send never behind it.
	presenceCh chan Presence

	notifyOnce sync.Once
	connected  atomic.Bool
}

func NewLink(opt LinkOptions, outbox *Outbox, cmds CommandHandler, notifier OwnerNotifier, clock Clock, log *zap.Logger) *Link {
	opt.fill()
	if clock == nil {
		clock = realClock{}
	}
	return &Link{
		opt: opt, outbox: outbox, cmds: cmds, notifier: notifier, clock: clock, log: log,
		rnd:        rand.Float64,
		wake:       make(chan struct{}, 1),
		outCh:      make(chan []byte, 256),
		cmdCh:      make(chan Envelope, 1024),
		presenceCh: make(chan Presence, 256),
	}
}

// Connected reports whether a session is past the handshake.
func (l *Link) Connected() bool { return l.connected.Load() }

// Emit queues a Hub event. It is written to the outbox before anything is
// sent. An id that is already queued is not queued twice.
func (l *Link) Emit(id, typ string, payload any) error {
	env, err := NewEnvelope(id, typ, l.clock.Now(), payload)
	if err != nil {
		return err
	}
	frame, err := json.Marshal(env)
	if err != nil {
		return err
	}
	added, err := l.outbox.Add(id, frame)
	if err != nil {
		return err
	}
	if added {
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// Backoff is the reconnect delay for the given attempt (0-based): exponential
// from min up to max, with jitter between half and all of that delay.
func Backoff(attempt int, min, max time.Duration, rnd func() float64) time.Duration {
	d := min
	for i := 0; i < attempt && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d/2 + time.Duration(rnd()*float64(d/2))
}

// Run keeps the link up until ctx ends or the Agent closes it with a code
// that means "do not reconnect".
func (l *Link) Run(ctx context.Context) error {
	go l.worker(ctx)
	go l.presenceWorker(ctx)

	attempt := 0
	for {
		welcomed, err := l.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var stop *StopError
		if errors.As(err, &stop) {
			l.log.Error("agent link: stopped, not reconnecting",
				zap.Int("close_code", stop.Code), zap.String("reason", stop.Reason))
			if stop.Notify {
				l.notifyOnce.Do(func() {
					l.notifier.NotifyOwner(fmt.Sprintf(
						"The agent link has stopped and will not reconnect. The agent closed it with code %d: %s",
						stop.Code, stop.Reason))
				})
			}
			return stop
		}

		if welcomed {
			attempt = 0
		}
		delay := Backoff(attempt, l.opt.BackoffMin, l.opt.BackoffMax, l.rnd)
		attempt++
		l.log.Warn("agent link: disconnected, will retry",
			zap.Error(err), zap.Duration("retry_in", delay))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// classify turns a connection error into a StopError when the Agent closed it
// with a code that forbids reconnecting.
func classify(err error) error {
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		return err
	}
	switch int(ce.Code) {
	case CloseVersionMismatch, CloseTokenRevoked:
		return &StopError{Code: int(ce.Code), Reason: ce.Reason, Notify: true}
	case CloseReplaced:
		return &StopError{Code: int(ce.Code), Reason: ce.Reason}
	}
	return err
}

// session is one connection from dial to teardown. It reports whether the
// handshake completed, so the caller knows to reset its backoff.
func (l *Link) session(ctx context.Context) (welcomed bool, err error) {
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	conn, resp, err := websocket.Dial(dctx, l.opt.URL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + l.opt.Token}},
	})
	dcancel()
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return false, fmt.Errorf("dial (http status %d): %w", status, err)
	}
	conn.SetReadLimit(maxFrameBytes)
	defer conn.CloseNow()

	// Handshake: hello, then nothing else until welcome.
	hello, err := NewEnvelope(NewULID(l.clock.Now()), TypeHello, l.clock.Now(), Hello{
		HubID: l.opt.HubID, Protocol: ProtocolVersion, HubVersion: l.opt.HubVersion, Channels: l.opt.Channels, Features: l.opt.Features,
	})
	if err != nil {
		return false, err
	}
	frame, _ := json.Marshal(hello)
	hctx, hcancel := context.WithTimeout(ctx, l.opt.HandshakeTimeout)
	defer hcancel()
	if err := conn.Write(hctx, websocket.MessageText, frame); err != nil {
		return false, classify(err)
	}
	for {
		_, data, err := conn.Read(hctx)
		if err != nil {
			return false, classify(err)
		}
		env, derr := DecodeEnvelope(data)
		if derr != nil {
			l.log.Warn("agent link: bad frame during handshake", zap.Error(derr))
			continue
		}
		if env.Type == TypeWelcome {
			if w, derr := DecodePayload(env); derr == nil {
				l.log.Info("agent link: connected", zap.String("agent_version", w.(*Welcome).AgentVersion))
			}
			break
		}
		if env.Type == TypeError {
			l.log.Warn("agent link: error from agent during handshake", zap.ByteString("payload", env.Payload))
		}
	}
	hcancel()

	// Stale results belong to the previous connection; the Agent retries those
	// commands and gets the remembered result.
	for drained := false; !drained; {
		select {
		case <-l.outCh:
		default:
			drained = true
		}
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l.connected.Store(true)
	defer l.connected.Store(false)

	errs := make(chan error, 3)
	var wg sync.WaitGroup
	run := func(f func(context.Context, *websocket.Conn) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f(sctx, conn)
		}()
	}
	run(l.readLoop)
	run(l.writeLoop)
	run(l.pingLoop)

	first := <-errs
	cancel()
	conn.CloseNow()
	wg.Wait()
	close(errs)

	// The cause is the most telling error: a close code beats the secondary
	// "connection closed" the other goroutines report.
	all := []error{first}
	for e := range errs {
		all = append(all, e)
	}
	for _, e := range all {
		c := classify(e)
		var stop *StopError
		if errors.As(c, &stop) {
			return true, c
		}
	}
	return true, first
}

func (l *Link) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			l.queueError("", "schema", "only text frames are allowed")
			continue
		}
		l.handleFrame(data)
	}
}

func (l *Link) pingLoop(ctx context.Context, conn *websocket.Conn) error {
	t := time.NewTicker(l.opt.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		// A pong is a frame from the peer. No answer within DeadAfter means
		// the connection is dead.
		pctx, cancel := context.WithTimeout(ctx, l.opt.DeadAfter)
		err := conn.Ping(pctx)
		cancel()
		if err != nil {
			return fmt.Errorf("agent silent: %w", err)
		}
	}
}

// writeLoop is the only writer. It replays the outbox in sequence order, then
// follows it live. Results and error frames are interleaved between batches.
func (l *Link) writeLoop(ctx context.Context, conn *websocket.Conn) error {
	const batch = 100
	var last uint
	write := func(frame []byte) error {
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, frame)
	}
	for {
		for drained := false; !drained; {
			select {
			case f := <-l.outCh:
				if err := write(f); err != nil {
					return err
				}
			default:
				drained = true
			}
		}

		rows, err := l.outbox.Pending(last, batch)
		if err != nil {
			return fmt.Errorf("outbox read: %w", err)
		}
		for _, r := range rows {
			if err := write([]byte(r.JSON)); err != nil {
				return err
			}
			last = r.Seq
		}
		if len(rows) == batch {
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.wake:
		case f := <-l.outCh:
			if err := write(f); err != nil {
				return err
			}
		}
	}
}

func (l *Link) handleFrame(data []byte) {
	env, err := DecodeEnvelope(data)
	if err != nil {
		l.queueError(env.ID, "schema", err.Error())
		return
	}
	if !KnownType(env.Type) {
		l.queueError(env.ID, "unknown_type", fmt.Sprintf("type %q is not part of protocol v1", env.Type))
		return
	}

	switch env.Type {
	case TypeAck:
		p, err := DecodePayload(env)
		if err != nil {
			l.queueError(env.ID, "schema", err.Error())
			return
		}
		if err := l.outbox.Delete(p.(*Ack).ID); err != nil {
			l.log.Error("agent link: could not delete acked event", zap.Error(err))
		}

	case TypeSend, TypeEditCard:
		if _, err := DecodePayload(env); err != nil {
			// The executor also answers this command with result "invalid".
			l.queueError(env.ID, "schema", err.Error())
		}
		select {
		case l.cmdCh <- env:
		default:
			l.queueFrame(TypeResult, Result{CommandID: env.ID, OK: false, Error: ErrRateLimited})
		}

	case TypePresence:
		// Not a command: no result, no id memory. A bad one is answered like any
		// schema violation; a good one is handed on without waiting.
		p, err := DecodePayload(env)
		if err != nil {
			l.queueError(env.ID, "schema", err.Error())
			return
		}
		select {
		case l.presenceCh <- *p.(*Presence):
		default:
			l.log.Debug("agent link: presence queue full, dropped a frame")
		}

	case TypeError:
		var pe ProtocolError
		_ = json.Unmarshal(env.Payload, &pe)
		l.log.Warn("agent link: error from agent",
			zap.String("ref_id", pe.RefID), zap.String("code", pe.Code), zap.String("message", pe.Message))

	case TypeWelcome:
		// Already handshaken; a repeat is harmless.

	default:
		l.queueError(env.ID, "unexpected_type", fmt.Sprintf("type %q is not sent to a Hub", env.Type))
	}
}

// worker executes commands one at a time, in arrival order, for the life of
// the process: a command that arrived just before a reconnect still runs once,
// and its result is remembered for the Agent's retry.
func (l *Link) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-l.cmdCh:
			res := l.cmds.Handle(ctx, env)
			l.queueFrame(TypeResult, res)
		}
	}
}

func (l *Link) queueFrame(typ string, payload any) {
	env, err := NewEnvelope(NewULID(l.clock.Now()), typ, l.clock.Now(), payload)
	if err != nil {
		l.log.Error("agent link: could not encode frame", zap.Error(err))
		return
	}
	frame, _ := json.Marshal(env)
	select {
	case l.outCh <- frame:
	default:
		l.log.Warn("agent link: outgoing queue full, dropped a frame", zap.String("type", typ))
	}
}

func (l *Link) queueError(refID, code, message string) {
	l.log.Warn("agent link: invalid frame from agent",
		zap.String("ref_id", refID), zap.String("code", code), zap.String("message", message))
	l.queueFrame(TypeError, ProtocolError{RefID: refID, Code: code, Message: message})
}

// presenceWorker applies `presence` frames one at a time, in arrival order, so
// a paused can never overtake the typing before it. A slow channel only delays
// later presence frames, never a send.
func (l *Link) presenceWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-l.presenceCh:
			if l.opt.Presence != nil {
				l.opt.Presence.HandlePresence(ctx, p)
			}
		}
	}
}
