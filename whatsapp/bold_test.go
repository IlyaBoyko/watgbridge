package whatsapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"watgbridge/state"
	"watgbridge/telegram/middlewares"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
)

// telegram.bold_customer_messages: the customer's own words are bold in the
// topic, the bridge's own lines and the owner's messages are not.

type sentCall struct{ Method, Text, Caption, ParseMode string }

// fakeBotAPI answers every Bot API method with a message that has no id, so
// the bridge does not try to save a pair into a database.
func fakeBotAPI(t *testing.T) (*gotgbot.Bot, func() []sentCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []sentCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		_ = r.ParseForm()
		parts := strings.Split(r.URL.Path, "/")
		mu.Lock()
		calls = append(calls, sentCall{
			Method: parts[len(parts)-1], Text: r.FormValue("text"), Caption: r.FormValue("caption"), ParseMode: r.FormValue("parse_mode"),
		})
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{
			"message_id": 0, "date": 1, "chat": map[string]any{"id": 1, "type": "supergroup"},
		}})
	}))
	t.Cleanup(srv.Close)
	bot, err := gotgbot.NewBot("1:T", &gotgbot.BotOpts{
		DisableTokenCheck: true,
		BotClient: &gotgbot.BaseBotClient{
			Client:             http.Client{},
			DefaultRequestOpts: &gotgbot.RequestOpts{APIURL: srv.URL, Timeout: 5 * time.Second},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bot.UseMiddleware(middlewares.ParseAsHTML)
	return bot, func() []sentCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]sentCall(nil), calls...)
	}
}

// newBC builds the context MessageFromOthersEventHandler would for a customer's
// message, with the given header already in the text.
func newBC(t *testing.T, bold bool, header string) (*bridgeContext, func() []sentCall) {
	t.Helper()
	bot, got := fakeBotAPI(t)
	cfg := &state.Config{}
	cfg.Telegram.BoldCustomerMessages = bold
	return &bridgeContext{cfg: cfg, logger: zap.NewNop(), tgBot: bot, bridgedText: header, boldBody: boldCustomer(cfg, types.MessageInfo{})}, got
}

func textEvent(text string) *events.Message {
	return &events.Message{Message: &waE2E.Message{Conversation: &text}}
}

const header = "🧑: <b>Aiman</b>\n👥: <b>(PVT)</b>\n"

func TestBoldCustomerOnlyForOthersAndOnlyWhenConfigured(t *testing.T) {
	on, off := &state.Config{}, &state.Config{}
	on.Telegram.BoldCustomerMessages = true
	customer := types.MessageInfo{}
	owner := types.MessageInfo{MessageSource: types.MessageSource{IsFromMe: true}}
	if !boldCustomer(on, customer) {
		t.Error("a customer's message must be bold when configured")
	}
	if boldCustomer(on, owner) {
		t.Error("the owner's own message (other-device mirror) must never be bold")
	}
	if boldCustomer(off, customer) {
		t.Error("nothing is bold unless configured")
	}
}

func TestWhatsAppTextIsBoldBelowTheHeader(t *testing.T) {
	bc, calls := newBC(t, true, header)
	bc.handleTextOrReaction("hello", textEvent("hello"), false, false)
	c := calls()
	if len(c) != 1 || c[0].Method != "sendMessage" || c[0].Text != header+"<b>hello</b>" || c[0].ParseMode != "html" {
		t.Errorf("calls = %+v", c)
	}
}

func TestWhatsAppTextEscapesAndKeepsWhatsAppFormattingLiteral(t *testing.T) {
	// The bridge does not convert WhatsApp's *bold* / _italic_ / ~strike~ / `mono`:
	// they arrive as written, and everything that is HTML is escaped inside <b>.
	in := "a < b & c > d *bold* _it_ ~s~ `m` <i>x</i>"
	bc, calls := newBC(t, true, header)
	bc.handleTextOrReaction(in, textEvent(in), false, false)
	want := header + "<b>a &lt; b &amp; c &gt; d *bold* _it_ ~s~ `m` &lt;i&gt;x&lt;/i&gt;</b>"
	if c := calls(); len(c) != 1 || c[0].Text != want {
		t.Errorf("calls = %+v, want text %q", c, want)
	}
}

func TestWhatsAppMentionLinkNestsInsideTheBold(t *testing.T) {
	// Mentions are linked in the body before it is wrapped (the contact lookup
	// needs a live client, so the linked body is given here).
	bc, _ := newBC(t, true, header)
	bc.appendBody(`hi <a href="https://wa.me/601">@Aiman</a> &amp; co`, 4096)
	if want := header + `<b>hi <a href="https://wa.me/601">@Aiman</a> &amp; co</b>`; bc.bridgedText != want {
		t.Errorf("got %q, want %q", bc.bridgedText, want)
	}
}

func TestWhatsAppTextNotBoldWhenOffOrFromTheOwner(t *testing.T) {
	bc, calls := newBC(t, false, header)
	bc.handleTextOrReaction("hello", textEvent("hello"), false, false)
	if c := calls(); len(c) != 1 || c[0].Text != header+"hello" {
		t.Errorf("config off: calls = %+v", c)
	}

	bc, calls = newBC(t, true, header)
	bc.boldBody = false // what MessageFromOthersEventHandler sets for IsFromMe
	bc.handleTextOrReaction("hello", textEvent("hello"), false, false)
	if c := calls(); len(c) != 1 || c[0].Text != header+"hello" {
		t.Errorf("owner's message: calls = %+v", c)
	}
}

func TestWhatsAppTextLengthGuard(t *testing.T) {
	// 4000 characters and a short header fit a message in bold ...
	long := strings.Repeat("a", 4000)
	bc, calls := newBC(t, true, header)
	bc.handleTextOrReaction(long, textEvent(long), false, false)
	if c := calls(); len(c) != 1 || c[0].Text != header+"<b>"+long+"</b>" {
		t.Error("a long text that fits must still be bold")
	}

	// ... but a header this long leaves no room for the tags: sent as before.
	bigHeader := strings.Repeat("h", 4096-len(long)-3)
	bc, calls = newBC(t, true, bigHeader)
	bc.handleTextOrReaction(long, textEvent(long), false, false)
	if c := calls(); len(c) != 1 || c[0].Text != bigHeader+long {
		t.Error("a message the bold would push over 4096 must not be bold")
	}
}

func TestEditedWhatsAppMessageIsBoldAsTextAndAsCaption(t *testing.T) {
	// Edited in place: a plain edit is a text (4096) ...
	bc, calls := newBC(t, true, header)
	bc.cfg.WhatsApp.SendEditedMessageUpdates = false
	bc.replyToMsgId = 5
	bc.handleTextOrReaction("fixed", textEvent("fixed"), true, false)
	if c := calls(); len(c) != 1 || c[0].Method != "editMessageText" || c[0].Text != header+"<b>fixed</b>" {
		t.Errorf("edit of a text: %+v", c)
	}
	// ... an edit of a file's caption is a caption (1024).
	bc, calls = newBC(t, true, header)
	bc.replyToMsgId = 5
	bc.handleTextOrReaction("fixed", textEvent("fixed"), true, true)
	if c := calls(); len(c) != 1 || c[0].Method != "editMessageCaption" || c[0].Caption != header+"<b>fixed</b>" {
		t.Errorf("edit of a caption: %+v", c)
	}
	caption := strings.Repeat("a", 1020)
	bc, calls = newBC(t, true, header)
	bc.replyToMsgId = 5
	bc.handleTextOrReaction(caption, textEvent(caption), true, true)
	if c := calls(); len(c) != 1 || c[0].Caption != header+caption {
		t.Error("a caption edit the bold would push over 1024 must not be bold")
	}
	// Edits sent as new messages are texts again.
	bc, calls = newBC(t, true, header)
	bc.cfg.WhatsApp.SendEditedMessageUpdates = true
	bc.handleTextOrReaction(caption, textEvent(caption), true, true)
	if c := calls(); len(c) != 1 || c[0].Method != "sendMessage" || c[0].Text != header+"<b>"+caption+"</b>" {
		t.Errorf("edit as a new message: %+v", c)
	}
}

func TestWhatsAppCaptionBoldAndLengthGuard(t *testing.T) {
	text := header
	addCaption(&text, "my <slip> & more", true)
	if want := header + "<b>my &lt;slip&gt; &amp; more</b>"; text != want {
		t.Errorf("got %q, want %q", text, want)
	}

	text = header
	addCaption(&text, "plain", false)
	if text != header+"plain" {
		t.Errorf("not configured: %q", text)
	}

	text = header
	addCaption(&text, "", true)
	if text != header {
		t.Errorf("no caption must add nothing, not an empty <b></b>: %q", text)
	}

	// Captions are cut to 1020 characters; with the header the bold tags no
	// longer fit under 1024, so this one stays plain.
	long := strings.Repeat("a", 1100)
	text = header
	addCaption(&text, long, true)
	if want := header + strings.Repeat("a", 1020) + "..."; text != want {
		t.Errorf("an over-long caption must not be bold, got %d bytes", len(text))
	}
}

func TestServiceLinesAreNeverBold(t *testing.T) {
	// Fallback notices, reactions and the like are the bridge's own words: they
	// go out through paths that never call appendBody.
	notice := "\n<i>Skipping image because 'skip_images' set in config file</i>"
	bc, calls := newBC(t, true, header)
	bc.sendFallbackText(notice)
	if c := calls(); len(c) != 1 || c[0].Text != header+notice {
		t.Errorf("calls = %+v", c)
	}
	// Even with an empty body, nothing like <b></b> appears.
	bc, _ = newBC(t, true, header)
	bc.appendBody("", 4096)
	if bc.bridgedText != header {
		t.Errorf("empty body changed the text: %q", bc.bridgedText)
	}
}
