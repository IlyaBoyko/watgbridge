package lzcustomer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"watgbridge/agentlink"
	"watgbridge/database"
	"watgbridge/state"
	"watgbridge/tgcustomer"
	"watgbridge/utils"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"go.uber.org/zap"
)

// Glue between the channel and the bridge's own state, the Hub bot and
// Lazada's CDN. No decisions live here.

/* ---------------------------------------------------------------- images -- */

const (
	imageTimeout  = 30 * time.Second
	imageMaxBytes = 10 << 20
	maxRedirects  = 3
)

// HTTPImages downloads pictures over HTTP(S): 30 s at most, 10 MiB at most,
// and only if the answer says it is an image.
type HTTPImages struct {
	hc *http.Client
	// insecure allows plain http (tests); Lazada's CDN is https.
	insecure bool
}

// NewHTTPImages builds the downloader. hc may be nil.
func NewHTTPImages(hc *http.Client, insecure bool) *HTTPImages {
	h := &HTTPImages{insecure: insecure}
	if hc == nil {
		hc = &http.Client{}
	}
	// A copy, so the caller's client is not changed.
	c := *hc
	c.Timeout = imageTimeout
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if !h.insecure && req.URL.Scheme != "https" {
			return errors.New("redirect to a non-https address")
		}
		return nil
	}
	h.hc = &c
	return h
}

func (h *HTTPImages) Fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", errors.New("not a usable address")
	}
	if req.URL.Scheme != "https" && !(h.insecure && req.URL.Scheme == "http") {
		return nil, "", errors.New("not an https address")
	}
	res, err := h.hc.Do(req)
	if err != nil {
		// The address can carry a token; keep it out of the error.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, "", fmt.Errorf("download failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download answered %s", res.Status)
	}
	mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if !strings.HasPrefix(mt, "image/") {
		return nil, "", fmt.Errorf("download is not an image (%q)", mt)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, imageMaxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("download failed: %w", err)
	}
	if len(data) > imageMaxBytes {
		return nil, "", errors.New("the picture is over 10 MiB")
	}
	return data, mt, nil
}

/* ----------------------------------------------------------- the Hub bot -- */

type hubTopics struct{}

func hubBot() (*gotgbot.Bot, error) {
	if state.State.TelegramBot == nil {
		return nil, fmt.Errorf("Telegram bot is not running")
	}
	return state.State.TelegramBot, nil
}

func (hubTopics) CreateTopic(ctx context.Context, name string) (int64, error) {
	b, err := hubBot()
	if err != nil {
		return 0, err
	}
	topic, err := b.CreateForumTopicWithContext(ctx, state.State.Config.Telegram.TargetChatID, name, &gotgbot.CreateForumTopicOpts{})
	if err != nil {
		return 0, err
	}
	return topic.MessageThreadId, nil
}

// topicText is the HTML for a post: Prefix and Text escaped, Text in bold when
// it is the buyer's own and the post still fits limit. The Hub bot's client
// sends everything as HTML (see telegram/client.go).
func topicText(p TopicPost, limit int) string {
	prefix, body := html.EscapeString(p.Prefix), html.EscapeString(p.Text)
	if p.Bold {
		body = utils.BoldCustomerBody(prefix, body, limit)
	}
	return prefix + body
}

func (hubTopics) Post(ctx context.Context, thread int64, p TopicPost) (int64, error) {
	b, err := hubBot()
	if err != nil {
		return 0, err
	}
	chat := state.State.Config.Telegram.TargetChatID
	file := func() *gotgbot.FileReader {
		name := p.Filename
		if name == "" {
			name = "file"
		}
		return &gotgbot.FileReader{Name: name, Data: bytes.NewReader(p.Data)}
	}

	var msg *gotgbot.Message
	switch p.Kind {
	case "text":
		msg, err = b.SendMessageWithContext(ctx, chat, topicText(p, utils.TgTextLimit), &gotgbot.SendMessageOpts{MessageThreadId: thread})
	case "photo":
		msg, err = b.SendPhotoWithContext(ctx, chat, file(), &gotgbot.SendPhotoOpts{Caption: topicText(p, utils.TgCaptionLimit), MessageThreadId: thread})
	case "document":
		msg, err = b.SendDocumentWithContext(ctx, chat, file(), &gotgbot.SendDocumentOpts{Caption: topicText(p, utils.TgCaptionLimit), MessageThreadId: thread})
	default:
		return 0, fmt.Errorf("unknown topic post kind %q", p.Kind)
	}
	if err != nil {
		return 0, err
	}
	return msg.MessageId, nil
}

func (hubTopics) Delete(ctx context.Context, msgID int64) error {
	b, err := hubBot()
	if err != nil {
		return err
	}
	_, err = b.DeleteMessageWithContext(ctx, state.State.Config.Telegram.TargetChatID, msgID, nil)
	return err
}

/* ----------------------------------------------------- the bridge's state -- */

// dbThreads keeps Lazada sessions' topics in ChatThreadPair, under
// `lz:<session_id>`, the way `tg:` rows are kept.
type dbThreads struct{}

func (dbThreads) Find(key string) (int64, bool, error) {
	return database.ChatThreadGetTgFromWa(key, state.State.Config.Telegram.TargetChatID)
}

func (dbThreads) Add(key string, thread int64) error {
	return database.ChatThreadAddNewPair(key, state.State.Config.Telegram.TargetChatID, thread)
}

// agentEvents hands events to the agent link (a no-op while it is off).
type agentEvents struct{}

func (agentEvents) Customer(in agentlink.CustomerInput) error {
	return agentlink.EmitCustomerMessage(in)
}
func (agentEvents) Staff(in agentlink.StaffInput) error { return agentlink.EmitStaffMessage(in) }

/* ----------------------------------------------------------------- hooks -- */

var current atomic.Pointer[Service]

// Init builds the Lazada channel from the bridge's config. It returns nil, nil
// when `lazada.enabled` is false, and then nothing anywhere changes. Nothing
// here touches the network, so unlike the customer bot there is no login to
// retry: a failure is a broken config or database, and the caller carries on
// without the channel.
func Init() (*Service, error) {
	cfg := state.State.Config
	lz := cfg.Lazada
	if !lz.Enabled {
		return nil, nil
	}
	db := state.State.Database
	tokens := NewTokenSource(db, NewAuth(nil, lz.AuthURL, lz.AppKey, lz.AppSecret, nil), nil)
	s, err := NewService(Options{
		API:  NewClient(nil, lz.APIURL, lz.AppKey, lz.AppSecret, nil, tokens),
		Auth: tokens, DB: db,
		Topics: hubTopics{}, Threads: dbThreads{}, Events: agentEvents{},
		Log:    state.State.Logger.Named("lazada"),
		AppKey: lz.AppKey, CallbackURL: lz.CallbackURL, OwnerID: cfg.Telegram.OwnerID,
		Location:     state.State.LocalLocation,
		SafetyEvery:  time.Duration(lz.SafetyPollSeconds) * time.Second,
		ActiveEvery:  time.Duration(lz.ActivePollSeconds) * time.Second,
		BoldCustomer: cfg.Telegram.BoldCustomerMessages,
	})
	if err != nil {
		return nil, err
	}
	current.Store(s)
	return s, nil
}

// HandleAuthCommand is the hook for /lazada_auth in the bridge's message
// handler. It returns true when the message was that command: it is then never
// sent on, whether or not the channel is running, because it must not reach a
// customer. Who may use it is decided by the channel (the owner only).
func HandleAuthCommand(b *gotgbot.Bot, c *ext.Context) bool {
	msg := c.EffectiveMessage
	if msg == nil {
		return false
	}
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	args, ok := ParseAuthCommand(text)
	if !ok {
		return false
	}
	s := current.Load()
	if s == nil {
		_, _ = utils.TgReplyTextByContext(b, c, "Lazada is turned off (lazada.enabled), so there is nothing to authorize.", nil, false)
		return true
	}
	var from int64
	if msg.From != nil {
		from = msg.From.Id
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s.HandleAuth(ctx, AuthRequest{FromID: from, Thread: msg.MessageThreadId, MsgID: msg.MessageId, Args: args})
	return true
}

// HandleTopicMessage is the hook in the bridge's topic-to-WhatsApp handler. It
// returns true when the topic belongs to a Lazada chat: the message then goes
// nowhere, and the sender is told why. It does so whether or not the channel
// is running, because a Lazada buyer's topic must never reach WhatsApp.
func HandleTopicMessage(b *gotgbot.Bot, c *ext.Context) bool {
	msg := c.EffectiveMessage
	if msg == nil || msg.MessageThreadId == 0 {
		return false
	}
	key, err := database.ChatThreadGetWaFromTg(msg.Chat.Id, msg.MessageThreadId)
	if err != nil {
		return false
	}
	if _, ok := agentlink.ParseLzChatKey(key); !ok {
		return false
	}
	if tgcustomer.IsServiceMessage(msg) {
		return true
	}
	if _, err := utils.TgReplyTextByContext(b, c, SendingNotYet, nil, false); err != nil {
		state.State.Logger.Warn("lazada: could not answer a topic message", zap.Error(err))
	}
	return true
}
