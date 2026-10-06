package tgcustomer

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"watgbridge/agentlink"
	"watgbridge/database"
	"watgbridge/retry"
	"watgbridge/state"
	"watgbridge/utils"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Options is everything a Service needs from the outside world.
type Options struct {
	Token      string
	APIURL     string // empty for Telegram's own
	SelfHosted bool
	HTTPClient *http.Client // nil for http.DefaultClient
	DB         *gorm.DB
	Topics     Topics
	Threads    Threads
	Events     Events
	Log        *zap.Logger

	BoldCustomer bool // telegram.bold_customer_messages
}

// Service is the running customer bot. It is also the agentlink.CustomerChannel
// the Agent's commands send through.
type Service struct {
	bot     *gotgbot.Bot
	cb      *customerBot
	bridge  *Bridge
	pairs   *Pairs
	updater *ext.Updater
	log     *zap.Logger
}

var (
	_ agentlink.CustomerChannel = (*Service)(nil)
	_ agentlink.TGPresence      = (*Service)(nil)
)

// perMessageTimeout bounds one update: a download, a topic post and an event.
const perMessageTimeout = 3 * time.Minute

// NewService builds the bot and migrates the package's table. It does not
// poll: Start does, so that the agent link can be built in between and no
// customer message is handled before it can be reported.
func NewService(o Options) (*Service, error) {
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	if err := Migrate(o.DB); err != nil {
		return nil, fmt.Errorf("customer bot: migrate: %w", err)
	}
	apiURL := o.APIURL
	if apiURL == "" {
		apiURL = gotgbot.DefaultAPIURL
	}
	// A plain client: the staff bot's rate-limit middleware sleeps and retries
	// forever on a 429, but the Agent has to be told about one.
	bot, err := gotgbot.NewBot(o.Token, &gotgbot.BotOpts{
		BotClient: &gotgbot.BaseBotClient{
			Client:             *hc,
			DefaultRequestOpts: &gotgbot.RequestOpts{APIURL: apiURL, Timeout: 30 * time.Second},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("customer bot: could not log in (is customer_bot.bot_token right?): %w", err)
	}

	s := &Service{bot: bot, pairs: NewPairs(o.DB), log: o.Log}
	s.cb = &customerBot{bot: bot, hc: hc, selfHosted: o.SelfHosted, log: o.Log}
	s.bridge = &Bridge{
		CustomerFiles: s.cb, Topics: o.Topics, Sender: s.cb, Threads: o.Threads,
		Pairs: s.pairs, Events: o.Events, Log: o.Log, BoldCustomer: o.BoldCustomer,
	}

	// One update at a time, in the order Telegram sent them: a customer's
	// messages must reach the topic and the Agent in order.
	dispatcher := ext.NewDispatcher(&ext.DispatcherOpts{
		MaxRoutines:      1,
		UnhandledErrFunc: func(err error) { o.Log.Error("customer bot dispatcher", zap.Error(err)) },
	})
	dispatcher.AddHandler(handlers.NewMessage(
		func(*gotgbot.Message) bool { return true }, s.onUpdate,
	).SetAllowEdited(true))
	s.updater = ext.NewUpdater(dispatcher, &ext.UpdaterOpts{
		UnhandledErrFunc: func(err error) { o.Log.Error("customer bot updater", zap.Error(err)) },
	})
	return s, nil
}

func (s *Service) onUpdate(_ *gotgbot.Bot, c *ext.Context) error {
	if c.EffectiveMessage == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), perMessageTimeout)
	defer cancel()
	s.bridge.HandleMessage(ctx, c.EffectiveMessage, c.Update.EditedMessage != nil)
	return nil
}

// Start takes the bot over: it deletes the bot's webhook (only one consumer
// may receive a bot's updates) and long-polls. Updates the website's webhook
// had not yet received are kept, not dropped.
func (s *Service) Start() error {
	err := s.updater.StartPolling(s.bot, &ext.PollingOpts{
		EnableWebhookDeletion: true,
		GetUpdatesOpts: &gotgbot.GetUpdatesOpts{
			Timeout:        25,
			AllowedUpdates: []string{"message", "edited_message"},
			RequestOpts:    &gotgbot.RequestOpts{Timeout: 35 * time.Second},
		},
	})
	if err != nil {
		return fmt.Errorf("customer bot: could not start polling: %w", err)
	}
	s.log.Info("customer bot is polling", zap.Int64("id", s.bot.Id), zap.String("username", "@"+s.bot.Username))
	return nil
}

// Stop ends polling and waits for the update in flight.
func (s *Service) Stop() {
	if err := s.updater.Stop(); err != nil {
		s.log.Warn("customer bot: stop", zap.Error(err))
	}
}

/* ----------------------------------------------- agentlink.CustomerChannel -- */

func (s *Service) SendText(ctx context.Context, chatID int64, m agentlink.CustomerText) (int64, error) {
	return s.cb.SendText(ctx, chatID, m)
}

func (s *Service) SendFile(ctx context.Context, chatID int64, m agentlink.CustomerFile) (int64, error) {
	return s.cb.SendFile(ctx, chatID, m)
}

func (s *Service) RecordPair(chatID, customerMsgID, threadID, topicMsgID int64) error {
	return s.pairs.Record(chatID, customerMsgID, threadID, topicMsgID)
}

func (s *Service) HubMsgIDOfTopicMsg(threadID, topicMsgID int64) string {
	chat, id, found, err := s.pairs.CustomerMsgFor(threadID, topicMsgID)
	if err != nil || !found {
		return ""
	}
	return agentlink.HubMsgIDForTelegram(chat, id)
}

/* ----------------------------------------------------------------- hooks -- */

var current atomic.Pointer[Service]

// Init builds the customer bot from the bridge's config. It returns nil, nil
// when `customer_bot.enabled` is false, and then nothing anywhere changes.
func Init() (*Service, error) {
	cfg := state.State.Config
	if !cfg.CustomerBot.Enabled {
		return nil, nil
	}
	token := cfg.CustomerBot.BotToken
	if token == "" {
		return nil, retry.Permanent(fmt.Errorf("customer_bot.enabled is true but customer_bot.bot_token is not set"))
	}
	if token == cfg.Telegram.BotToken {
		return nil, retry.Permanent(fmt.Errorf("customer_bot.bot_token is the staff bot's token: the customer bot must be a different bot"))
	}
	s, err := NewService(Options{
		Token: token, APIURL: cfg.Telegram.APIURL, SelfHosted: cfg.Telegram.SelfHostedAPI,
		DB: state.State.Database, Topics: hubTopics{}, Threads: dbThreads{}, Events: agentEvents{},
		Log:          state.State.Logger.Named("customerbot"),
		BoldCustomer: cfg.Telegram.BoldCustomerMessages,
	})
	if err != nil {
		if utils.TgIsBadToken(err) {
			err = retry.Permanent(err)
		}
		return nil, err
	}
	current.Store(s)
	return s, nil
}

// HandleTopicMessage is the hook in the bridge's topic-to-WhatsApp handler. It
// returns true when the topic belongs to a Telegram customer: the message then
// goes to that customer, and never to WhatsApp, whether or not the customer
// bot is running.
func HandleTopicMessage(b *gotgbot.Bot, c *ext.Context) bool {
	msg := c.EffectiveMessage
	if msg == nil || msg.MessageThreadId == 0 {
		return false
	}
	key, err := database.ChatThreadGetWaFromTg(msg.Chat.Id, msg.MessageThreadId)
	if err != nil {
		return false
	}
	userID, ok := agentlink.ParseTgChatKey(key)
	if !ok {
		return false
	}
	if IsServiceMessage(msg) {
		return true
	}

	s := current.Load()
	if s == nil {
		note := "The customer bot is turned off (customer_bot.enabled), so this was not sent to the customer."
		if state.State.Config.CustomerBot.Enabled {
			note = "The customer bot is not logged in yet (the Hub keeps retrying), so this was not sent to the customer."
		}
		_, _ = utils.TgReplyTextByContext(b, c, note, nil, false)
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), perMessageTimeout)
	defer cancel()
	if err := s.bridge.StaffToCustomer(ctx, msg, userID); err != nil {
		_ = utils.TgReplyWithErrorByContext(b, c, "Cannot send to the customer", err)
		return true
	}
	utils.SendMessageConfirmation(b, c, state.State.Config, msg, nil)
	return true
}

// SendTyping is agentlink.TGPresence.
func (s *Service) SendTyping(ctx context.Context, chatID int64) error {
	return s.cb.SendTyping(ctx, chatID)
}
