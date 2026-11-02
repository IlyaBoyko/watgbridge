package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"strings"
	"sync"
	"time"

	"watgbridge/state"
	"watgbridge/utils"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
	"github.com/mdp/qrterminal/v3"
	"github.com/skip2/go-qrcode"
	"go.uber.org/zap"
)

// ErrNotPaired is NewWhatsAppClient's answer when the login store holds no
// WhatsApp session (never linked, or WhatsApp logged the device out).
var ErrNotPaired = errors.New("WhatsApp is not linked")

const (
	pairCommand   = "wa_pair"
	statusCommand = "wa_status"

	controlCallbackPrefix = "wa_ctl:"
	statusCallback        = controlCallbackPrefix + "status"
	pairCallback          = controlCallbackPrefix + "pair"

	// pairMinGap is the least time between the starts of two pairing
	// attempts, however often the owner asks.
	pairMinGap = 3 * time.Minute
	// pairHandlerGroup runs before the bridge's own handlers, so a /wa_*
	// command typed in a topic is never forwarded to a customer.
	pairHandlerGroup = -50
)

// pairingState is what the owner's controls and the pairing loop share.
type pairingState struct {
	requests chan struct{}

	mu         sync.Mutex
	holding    bool
	attempting bool
	lastStart  time.Time
	lastErr    string
}

func newPairingState() *pairingState {
	return &pairingState{requests: make(chan struct{}, 1)}
}

var pairing = newPairingState()

// beginHold reports false when a hold already runs, so a logout during one
// does not start a second. An ask left over from earlier is no request now.
func (p *pairingState) beginHold() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.holding {
		return false
	}
	p.holding = true
	select {
	case <-p.requests:
	default:
	}
	return true
}

func (p *pairingState) endHold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holding = false
}

func (p *pairingState) attemptStarted(at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempting = true
	p.lastStart = at
}

func (p *pairingState) attemptEnded(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempting = false
	p.lastErr = ""
	if err != nil {
		p.lastErr = err.Error()
	}
}

// sinceLastStart is how long ago the last attempt started; ok is false when
// none has run.
func (p *pairingState) sinceLastStart(now time.Time) (d time.Duration, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastStart.IsZero() {
		return 0, false
	}
	return now.Sub(p.lastStart), true
}

// ask is the owner's request for an attempt. It returns the answer to show;
// the gap between attempts is the pairing loop's to enforce.
func (p *pairingState) ask(linked bool) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case linked:
		return "WhatsApp is already linked."
	case !p.holding:
		return "The bridge is still starting. Try again in a moment."
	case p.attempting:
		return "A pairing attempt is already running. Scan one of its QR codes."
	}
	select {
	case p.requests <- struct{}{}:
	default:
	}
	return "Pairing requested."
}

// waLink is the WhatsApp side of a status report.
type waLink struct {
	linked    bool
	connected bool
	loggedIn  bool
	name      string
	jid       string
}

// statusText is the report behind the Status button.
func (p *pairingState) statusText(l waLink, now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var b strings.Builder
	switch {
	case l.linked && l.loggedIn:
		b.WriteString("<b>WhatsApp:</b> linked and connected")
	case l.linked && l.connected:
		b.WriteString("<b>WhatsApp:</b> linked, logging in")
	case l.linked:
		b.WriteString("<b>WhatsApp:</b> linked, not connected. The bridge reconnects by itself.")
	case p.attempting:
		b.WriteString("<b>WhatsApp:</b> not linked. A pairing attempt is running: scan one of its QR codes.")
	case p.holding:
		b.WriteString("<b>WhatsApp:</b> not linked. The bridge is not contacting WhatsApp. Press Pair when the phone is ready.")
	default:
		b.WriteString("<b>WhatsApp:</b> not linked. The bridge is still starting.")
	}
	if l.linked {
		fmt.Fprintf(&b, "\n<b>Account:</b> %s (%s)", html.EscapeString(l.name), html.EscapeString(l.jid))
		return b.String()
	}
	if p.lastStart.IsZero() {
		return b.String()
	}
	since := now.Sub(p.lastStart)
	fmt.Fprintf(&b, "\n<b>Last attempt:</b> %s ago", since.Round(time.Second))
	if p.lastErr != "" {
		fmt.Fprintf(&b, " (%s)", html.EscapeString(p.lastErr))
	}
	if wait := pairMinGap - since; wait > 0 && !p.attempting {
		fmt.Fprintf(&b, "\n<b>Pair available in:</b> %s", wait.Round(time.Second))
	}
	return b.String()
}

func currentWaLink() waLink {
	client := state.State.WhatsAppClient
	if client == nil || client.Store == nil || client.Store.ID == nil {
		return waLink{}
	}
	return waLink{
		linked:    true,
		connected: client.IsConnected(),
		loggedIn:  client.IsLoggedIn(),
		name:      client.Store.PushName,
		jid:       client.Store.ID.String(),
	}
}

func controlButtons() gotgbot.InlineKeyboardMarkup {
	return gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
		{Text: "Status", CallbackData: statusCallback},
		{Text: "Pair", CallbackData: pairCallback},
	}}}
}

func sendToOwner(text string) {
	var (
		cfg    = state.State.Config
		logger = state.State.Logger
		bot    = state.State.TelegramBot
	)
	if bot == nil {
		return
	}
	if _, err := bot.SendMessage(cfg.Telegram.OwnerID, text, &gotgbot.SendMessageOpts{ReplyMarkup: controlButtons()}); err != nil {
		logger.Warn("could not message the owner about WhatsApp pairing", zap.Error(err))
	}
}

func ownerStatusCommand(b *gotgbot.Bot, c *ext.Context) error {
	if isOwnerPrivateChat(c) {
		sendToOwner(pairing.statusText(currentWaLink(), time.Now()))
	}
	return ext.EndGroups
}

func ownerPairCommand(b *gotgbot.Bot, c *ext.Context) error {
	if isOwnerPrivateChat(c) {
		sendToOwner(pairing.ask(currentWaLink().linked))
	}
	return ext.EndGroups
}

func isOwnerPrivateChat(c *ext.Context) bool {
	owner := state.State.Config.Telegram.OwnerID
	return c.EffectiveUser != nil && c.EffectiveUser.Id == owner &&
		c.EffectiveChat != nil && c.EffectiveChat.Id == owner
}

func controlCallback(b *gotgbot.Bot, c *ext.Context) error {
	cq := c.CallbackQuery
	if cq.From.Id != state.State.Config.Telegram.OwnerID {
		_, _ = cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: "Owner only."})
		return ext.EndGroups
	}
	if cq.Data == pairCallback {
		_, _ = cq.Answer(b, &gotgbot.AnswerCallbackQueryOpts{Text: pairing.ask(currentWaLink().linked), ShowAlert: true})
		return ext.EndGroups
	}
	_, _ = cq.Answer(b, nil)
	sendToOwner(pairing.statusText(currentWaLink(), time.Now()))
	return ext.EndGroups
}

// InstallPairingControls adds the owner's /wa_status, /wa_pair and their
// buttons. It runs before the WhatsApp login, because they are needed exactly
// when that login is waiting for a phone.
func InstallPairingControls() {
	dispatcher := state.State.TelegramDispatcher
	if dispatcher == nil {
		return
	}
	dispatcher.AddHandlerToGroup(handlers.NewCommand(statusCommand, ownerStatusCommand), pairHandlerGroup)
	dispatcher.AddHandlerToGroup(handlers.NewCommand(pairCommand, ownerPairCommand), pairHandlerGroup)
	dispatcher.AddHandlerToGroup(handlers.NewCallback(
		func(cq *gotgbot.CallbackQuery) bool {
			return strings.HasPrefix(cq.Data, controlCallbackPrefix)
		}, controlCallback), pairHandlerGroup)
}

// SetOwnerMenu shows the WhatsApp commands in the owner's private chat only,
// on top of the bot's regular menu, which a chat's own list would otherwise
// replace. It runs before the WhatsApp login, because the regular menu is only
// set after it, and again once that menu is set.
func SetOwnerMenu() {
	var (
		cfg    = state.State.Config
		logger = state.State.Logger
		bot    = state.State.TelegramBot
	)
	if cfg.Telegram.SkipSettingCommands {
		return
	}
	menu, err := bot.GetMyCommands(&gotgbot.GetMyCommandsOpts{Scope: gotgbot.BotCommandScopeDefault{}})
	if err != nil {
		logger.Warn("could not read the bot menu", zap.Error(err))
		return
	}
	menu = append(menu,
		gotgbot.BotCommand{Command: statusCommand, Description: "Owner: WhatsApp status"},
		gotgbot.BotCommand{Command: pairCommand, Description: "Owner: link WhatsApp (QR codes)"},
	)
	if _, err = bot.SetMyCommands(menu, &gotgbot.SetMyCommandsOpts{
		Scope: gotgbot.BotCommandScopeChat{ChatId: cfg.Telegram.OwnerID},
	}); err != nil {
		logger.Warn("could not set the owner's menu", zap.Error(err))
	}
}

// WaitForPairing blocks until the owner has linked WhatsApp. A pairing
// attempt is one connection showing about 2.5 minutes of QR codes. With
// tryFirst (a fresh start) one attempt runs at once; after that, and always
// without tryFirst (a logout), attempts start only on the owner's Pair button
// or /wa_pair, at most one per pairMinGap. The process never exits over a
// missed scan, so nothing turns into a stream of pairing connections to
// WhatsApp.
func WaitForPairing(ctx context.Context, tryFirst bool) error {
	if !pairing.beginHold() {
		return nil
	}
	defer pairing.endHold()

	logger := state.State.Logger
	logger.Warn("WhatsApp is not linked", zap.Bool("pairing_now", tryFirst))
	err := pairLoop(ctx, pairing, tryFirst, func() error { return pairOnce(logger) }, sendToOwner, time.Now)
	if err == nil {
		logger.Info("successfully logged into WhatsApp",
			zap.String("push_name", state.State.WhatsAppClient.Store.PushName),
			zap.String("jid", state.State.WhatsAppClient.Store.ID.String()),
		)
	}
	return err
}

// pairLoop is WaitForPairing without Telegram or WhatsApp. With tryFirst it
// runs one attempt straight away; every other attempt waits for a request,
// never sooner than pairMinGap after the last start, and requests that
// arrive while an attempt runs are dropped.
func pairLoop(ctx context.Context, p *pairingState, tryFirst bool, attempt func() error, notify func(string), now func() time.Time) error {
	if tryFirst {
		notify("WhatsApp is not linked. QR codes follow for about 2.5 minutes: scan one in WhatsApp → Linked devices.")
	} else {
		notify("WhatsApp is not linked, so the bridge is not contacting WhatsApp at all.\n\n" +
			"Press Pair when you have the phone ready. QR codes follow for about 2.5 minutes; " +
			"scan one in WhatsApp → Linked devices.")
	}
	for first := tryFirst; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-p.requests:
			}
			if since, ok := p.sinceLastStart(now()); ok {
				if wait := pairMinGap - since; wait > 0 {
					notify(fmt.Sprintf("The last pairing started less than %d minutes ago. Press Pair again in %d seconds.",
						int(pairMinGap.Minutes()), int(wait.Seconds())+1))
					continue
				}
			}
		}
		p.attemptStarted(now())
		err := attempt()
		p.attemptEnded(err)
		// Asks made while the attempt ran are not a request for another one.
		select {
		case <-p.requests:
		default:
		}
		if err == nil {
			notify("WhatsApp is linked.")
			return nil
		}
		notify(fmt.Sprintf("WhatsApp was not linked (%s), so I'm stopping: the bridge will not contact WhatsApp again until you press Pair.",
			html.EscapeString(err.Error())))
	}
}

// pairOnce is one pairing attempt: one connection that shows QR codes until
// a phone links it or WhatsApp ends the session.
func pairOnce(logger *zap.Logger) error {
	var (
		cfg    = state.State.Config
		client = state.State.WhatsAppClient
		bot    = state.State.TelegramBot
	)

	qrChan, err := client.GetQRChannel(context.Background())
	if err != nil {
		return fmt.Errorf("could not start pairing: %w", err)
	}
	if err = client.Connect(); err != nil {
		return fmt.Errorf("could not connect to WhatsApp for pairing: %w", err)
	}

	last := ""
	for evt := range qrChan {
		last = evt.Event
		if evt.Event != "code" {
			logger.Info("received WhatsApp login event", zap.Any("event", evt.Event))
			continue
		}
		if bot != nil {
			qrCodePNG, err := qrcode.Encode(evt.Code, qrcode.Highest, 512)
			if err != nil {
				_ = utils.TgSendTextById(bot, cfg.Telegram.OwnerID, 0, fmt.Sprintf(
					"Please check your terminal and scan the QR code to login to WhatsApp. Failed to encode to PNG and send here:\n<code>%s</code>",
					html.EscapeString(err.Error()),
				))
			} else {
				_, _ = bot.SendPhoto(
					cfg.Telegram.OwnerID,
					gotgbot.InputFileByReader("qrcode.png", bytes.NewReader(qrCodePNG)),
					&gotgbot.SendPhotoOpts{Caption: "Scan the above QR code to login to WhatsApp."},
				)
			}
		}
		qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
	}

	if client.Store.ID == nil {
		client.Disconnect()
		if last == "" {
			last = "no answer"
		}
		return fmt.Errorf("pairing ended: %s", last)
	}
	return nil
}
