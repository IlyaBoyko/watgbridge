package telegram

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"watgbridge/retry"
	"watgbridge/state"
	"watgbridge/telegram/middlewares"
	"watgbridge/utils"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"go.uber.org/zap"
)

// withRetry runs one Telegram start-up call again with backoff. Through the
// Pi's proxy the first request after a container start can fail, and giving up
// on it would crash-loop the whole bridge.
func withRetry(logger *zap.Logger, what string, op func() error) error {
	return retry.Do(context.Background(), retry.Default, nil, op,
		func(attempt int, wait time.Duration, err error) {
			logger.Warn("telegram "+what+" failed, retrying",
				zap.Int("attempt", attempt), zap.Duration("retry_in", wait), zap.Error(err))
		})
}

func NewTelegramClient() error {
	var (
		cfg    = state.State.Config
		logger = state.State.Logger
	)
	defer logger.Sync()

	var bot *gotgbot.Bot
	err := withRetry(logger, "login", func() (err error) {
		bot, err = gotgbot.NewBot(cfg.Telegram.BotToken, &gotgbot.BotOpts{
			BotClient: &gotgbot.BaseBotClient{
				Client: http.Client{},
				DefaultRequestOpts: &gotgbot.RequestOpts{
					APIURL:  cfg.Telegram.APIURL,
					Timeout: time.Duration(math.MaxInt64),
				},
			},
		})
		if utils.TgIsBadToken(err) {
			err = retry.Permanent(err)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("could not initialize telegram bot : %s", err)
	}
	state.State.TelegramBot = bot

	bot.UseMiddleware(middlewares.AutoHandleRateLimit)
	bot.UseMiddleware(middlewares.ParseAsHTML)
	bot.UseMiddleware(middlewares.DisableWebPagePreview)
	bot.UseMiddleware(middlewares.SendWithoutReply)

	dispatcher := ext.NewDispatcher(&ext.DispatcherOpts{
		UnhandledErrFunc: func(err error) {
			logger.Error("telegram dispatcher received error",
				zap.Error(err),
			)
		},
		MaxRoutines: ext.DefaultMaxRoutines,
	})

	updater := ext.NewUpdater(dispatcher, &ext.UpdaterOpts{
		UnhandledErrFunc: func(err error) {
			logger.Error("telegram updater received error",
				zap.Error(err),
			)
		},
	})

	state.State.TelegramUpdater = updater
	state.State.TelegramDispatcher = dispatcher

	// StartPolling's first call is deleteWebhook; when that fails nothing has
	// started, so trying again is safe.
	err = withRetry(logger, "start polling", func() error {
		return updater.StartPolling(bot, &ext.PollingOpts{
			DropPendingUpdates: true,
			GetUpdatesOpts: &gotgbot.GetUpdatesOpts{
				Timeout: 25,
				AllowedUpdates: []string{
					"message",
					"edited_message",
					"channel_post",
					"edited_channel_post",
					"callback_query",
					"my_chat_member",
					"chat_member",
				},
				RequestOpts: &gotgbot.RequestOpts{
					Timeout: 35 * time.Second,
				},
			},
		})
	})
	if err != nil {
		return fmt.Errorf("telegram failed to start polling : %s", err)
	}

	logger.Info("successfully logged into telegram",
		zap.Int64("id", bot.Id),
		zap.String("name", bot.FirstName),
		zap.String("username", "@"+bot.Username),
		zap.String("api_url", cfg.Telegram.APIURL),
	)

	return nil
}
