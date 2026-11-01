package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"sync/atomic"
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
	pairCommand = "wa_pair"
	// pairMinGap is the least time between the starts of two pairing
	// attempts, however often the owner asks.
	pairMinGap = 3 * time.Minute
	// pairHandlerGroup keeps /wa_pair apart from the bridge's own handlers.
	pairHandlerGroup = -50
)

// pairingHeld is set while a WaitForPairing runs, so a logout during a
// pairing hold does not start a second one.
var pairingHeld atomic.Bool

// WaitForPairing blocks until the owner has linked WhatsApp. A pairing
// attempt is one connection showing about 2.5 minutes of QR codes. With
// tryFirst (a fresh start) one attempt runs at once; after that, and always
// without tryFirst (a logout), attempts start only on the owner's /wa_pair,
// at most one per pairMinGap. The process never exits over a missed scan,
// so nothing turns into a stream of pairing connections to WhatsApp.
func WaitForPairing(ctx context.Context, tryFirst bool) error {
	if !pairingHeld.CompareAndSwap(false, true) {
		return nil
	}
	defer pairingHeld.Store(false)

	var (
		cfg        = state.State.Config
		logger     = state.State.Logger
		bot        = state.State.TelegramBot
		dispatcher = state.State.TelegramDispatcher
	)

	notify := func(text string) {
		if bot == nil {
			return
		}
		if err := utils.TgSendTextById(bot, cfg.Telegram.OwnerID, 0, text); err != nil {
			logger.Warn("could not message the owner about WhatsApp pairing", zap.Error(err))
		}
	}

	requests := make(chan struct{}, 1)
	if dispatcher != nil {
		h := handlers.NewCommand(pairCommand, func(b *gotgbot.Bot, c *ext.Context) error {
			if c.EffectiveUser == nil || c.EffectiveUser.Id != cfg.Telegram.OwnerID {
				return nil
			}
			select {
			case requests <- struct{}{}:
			default:
			}
			return ext.EndGroups
		})
		dispatcher.AddHandlerToGroup(h, pairHandlerGroup)
		defer dispatcher.RemoveHandlerFromGroup(h.Name(), pairHandlerGroup)
	}

	logger.Warn("WhatsApp is not linked", zap.Bool("pairing_now", tryFirst))
	err := pairLoop(ctx, tryFirst, requests, func() error { return pairOnce(logger) }, notify, time.Now)
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
func pairLoop(ctx context.Context, tryFirst bool, requests <-chan struct{}, attempt func() error, notify func(string), now func() time.Time) error {
	if tryFirst {
		notify("WhatsApp is not linked. QR codes follow for about 2.5 minutes: scan one in WhatsApp → Linked devices.")
	} else {
		notify("WhatsApp is not linked, so the bridge is not contacting WhatsApp at all.\n\n" +
			"Send /" + pairCommand + " here when you have the phone ready. QR codes follow for about 2.5 minutes; " +
			"scan one in WhatsApp → Linked devices.")
	}
	var lastStart time.Time
	for first := tryFirst; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-requests:
			}
			if !lastStart.IsZero() {
				if wait := pairMinGap - now().Sub(lastStart); wait > 0 {
					notify(fmt.Sprintf("The last pairing started less than %d minutes ago. Send /%s again in %d seconds.",
						int(pairMinGap.Minutes()), pairCommand, int(wait.Seconds())+1))
					continue
				}
			}
		}
		lastStart = now()
		err := attempt()
		// Asks made while the attempt ran are not a request for another one.
		select {
		case <-requests:
		default:
		}
		if err == nil {
			notify("WhatsApp is linked.")
			return nil
		}
		notify(fmt.Sprintf("WhatsApp was not linked (%s), so I'm stopping: the bridge will not contact WhatsApp again until you send /%s.",
			html.EscapeString(err.Error()), pairCommand))
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
