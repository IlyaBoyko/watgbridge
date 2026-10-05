package agentlink

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

const tgConv = "tg:5550001111"

func tgWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.bridge.threads[tgConv] = 1250
	return w
}

func tgSend(t *testing.T, w *world, id string, p Send) Envelope {
	t.Helper()
	p.Conversation = tgConv
	return sendEnv(t, w, id, p)
}

var orderLink = &SendLink{Label: "View your order", URL: "https://clarusmolecular.com/order/cm-261002-qx7p-AbCd"}

// A6: plain text is escaped, the answer is mirrored with the robot prefix and
// both messages are paired, and reply_to quotes the customer's message.
func TestTelegramReplyEscapesMirrorsAndPairs(t *testing.T) {
	w := tgWorld(t)
	e := w.executor()

	res := e.Handle(context.Background(), tgSend(t, w, "CMD1", Send{Kind: "reply", Text: "a < b & c > d", ReplyTo: "5550001111:40"}))
	if !res.OK || res.Error != "" || res.HubMsgID != "5550001111:7001" || res.DeliveredAt == "" {
		t.Fatalf("result = %+v", res)
	}
	sends := w.customer.Sends()
	if len(sends) != 1 || sends[0].File || sends[0].ChatID != 5550001111 {
		t.Fatalf("sends = %+v", sends)
	}
	if got := sends[0].Text; got.HTML != "a &lt; b &amp; c &gt; d" || got.ReplyTo != 40 || len(got.Buttons) != 0 {
		t.Errorf("text message = %+v", got)
	}
	if n := len(w.wa.Calls()); n != 0 {
		t.Errorf("%d WhatsApp sends for a Telegram customer", n)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].Thread != 1250 || posts[0].Text != "🤖 <blockquote>a &lt; b &amp; c &gt; d</blockquote>" {
		t.Fatalf("mirror = %+v", posts)
	}
	if want := []custPair{{5550001111, 7001, 1250, 9001}}; !reflect.DeepEqual(w.customer.Pairs(), want) {
		t.Errorf("pairs = %+v", w.customer.Pairs())
	}
	if !e.Guard.Has("5550001111:7001") {
		t.Error("the sent message is not remembered as the Agent's own")
	}
}

func TestTelegramReplyQuoteOfAnotherChatIsDropped(t *testing.T) {
	w := tgWorld(t)
	e := w.executor()
	for i, replyTo := range []string{"999:40", "garbage", "5550001111:x", "5550001111:0"} {
		res := e.Handle(context.Background(), tgSend(t, w, fmt.Sprintf("CMD%d", i), Send{Kind: "reply", Text: "hi", ReplyTo: replyTo}))
		if !res.OK {
			t.Fatalf("%q: result = %+v", replyTo, res)
		}
	}
	for _, s := range w.customer.Sends() {
		if s.Text.ReplyTo != 0 {
			t.Errorf("a quote of %d was sent", s.Text.ReplyTo)
		}
	}
}

// A6: copyables become <code> lines plus copy_text buttons, everything else
// stays escaped, and the link is a URL button in a row of its own.
func TestTelegramReplyCopyablesAndLink(t *testing.T) {
	w := tgWorld(t)
	e := w.executor()
	copyables := Copyables{{Label: "Bank <A>", Value: "1234 & 5"}, {Label: "Alias", Value: "my.alias"}}

	res := e.Handle(context.Background(), tgSend(t, w, "CMD1", Send{
		Kind: "reply", Text: "Pay <now> & done", Copyables: &copyables, Link: orderLink,
	}))
	if !res.OK || res.HubMsgID != "5550001111:7001" {
		t.Fatalf("result = %+v", res)
	}
	sends := w.customer.Sends()
	if len(sends) != 1 {
		t.Fatalf("%d messages for copyables on Telegram, want one", len(sends))
	}
	wantHTML := "Pay &lt;now&gt; &amp; done\nBank &lt;A&gt;: <code>1234 &amp; 5</code>\nAlias: <code>my.alias</code>"
	if sends[0].Text.HTML != wantHTML {
		t.Errorf("html = %q\nwant   %q", sends[0].Text.HTML, wantHTML)
	}
	wantKB := [][]CustomerButton{
		{{Text: "Copy Bank <A>", CopyText: "1234 & 5"}},
		{{Text: "Copy Alias", CopyText: "my.alias"}},
		{{Text: "View your order", URL: orderLink.URL}},
	}
	if !reflect.DeepEqual(sends[0].Text.Buttons, wantKB) {
		t.Errorf("buttons = %+v", sends[0].Text.Buttons)
	}
	// The mirror shows copyables and link as plain label: value lines.
	posts := w.topics.Posts()
	want := "🤖 <blockquote>Pay &lt;now&gt; &amp; done</blockquote>\nBank &lt;A&gt;: 1234 &amp; 5\nAlias: my.alias\nView your order: " + orderLink.URL
	if len(posts) != 1 || posts[0].Text != want {
		t.Errorf("mirror = %+v\nwant %q", posts, want)
	}
}

func TestTelegramReplyLinkAloneIsOneURLButton(t *testing.T) {
	w := tgWorld(t)
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{Kind: "reply", Text: "Got your order.", Link: orderLink}))
	if !res.OK {
		t.Fatal(res)
	}
	got := w.customer.Sends()[0].Text
	if got.HTML != "Got your order." || !reflect.DeepEqual(got.Buttons, [][]CustomerButton{{{Text: "View your order", URL: orderLink.URL}}}) {
		t.Errorf("message = %+v", got)
	}
}

func TestTelegramReplyWithMediaCarriesTheCaption(t *testing.T) {
	w := tgWorld(t)
	copyables := Copyables{{Label: "Alias", Value: "my.alias"}}
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{
		Kind: "reply", Text: "DuitNow <QR>", Copyables: &copyables,
		Media: []Media{{Kind: "image", Mime: "image/png", Filename: "qr.png", Size: 70, Data: pngB64}},
	}))
	if !res.OK || res.HubMsgID != "5550001111:7001" {
		t.Fatalf("result = %+v", res)
	}
	sends := w.customer.Sends()
	raw, _ := base64.StdEncoding.DecodeString(pngB64)
	if len(sends) != 1 || !sends[0].File || sends[0].Media.Kind != "image" || string(sends[0].Media.Data) != string(raw) ||
		sends[0].Media.Filename != "qr.png" || sends[0].Media.HTMLCaption != "DuitNow &lt;QR&gt;\nAlias: <code>my.alias</code>" ||
		len(sends[0].Media.Buttons) != 1 {
		t.Fatalf("sends = %+v", sends)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].Media == nil || posts[0].Text != "🤖 <blockquote>DuitNow &lt;QR&gt;</blockquote>\nAlias: my.alias" {
		t.Errorf("mirror = %+v", posts)
	}
}

func TestTelegramReplyMediaWithOnlyALinkKeepsTheButtonOnTheMedia(t *testing.T) {
	w := tgWorld(t)
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{
		Kind: "reply", Link: orderLink,
		Media: []Media{{Kind: "document", Mime: "application/pdf", Filename: "invoice.pdf", Size: 70, Data: pngB64, Caption: "Invoice"}},
	}))
	if !res.OK {
		t.Fatal(res)
	}
	s := w.customer.Sends()
	if len(s) != 1 || s[0].Media.HTMLCaption != "Invoice" || len(s[0].Media.Buttons) != 1 || s[0].Media.Kind != "document" {
		t.Fatalf("sends = %+v", s)
	}
	if p := w.topics.Posts(); len(p) != 1 || p[0].Text != "🤖 <blockquote>Invoice</blockquote>\nView your order: "+orderLink.URL {
		t.Errorf("mirror = %+v", p)
	}
}

// A6: a caption over Telegram's 1024 limit sends the media first and the text
// as a second message. The limit counts UTF-16 code units, like Telegram.
func TestTelegramCaptionLimit(t *testing.T) {
	media := []Media{{Kind: "image", Mime: "image/png", Filename: "qr.png", Size: 70, Data: pngB64}}
	for name, tc := range map[string]struct {
		text  string
		split bool
	}{
		"exactly 1024":               {strings.Repeat("a", 1024), false},
		"1025":                       {strings.Repeat("a", 1025), true},
		"emoji count as two units":   {strings.Repeat("😀", 513), true},
		"emoji at the limit":         {strings.Repeat("😀", 512), false},
		"copyable lines are counted": {strings.Repeat("a", 1020), true},
	} {
		t.Run(name, func(t *testing.T) {
			w := tgWorld(t)
			var copyables *Copyables
			if name == "copyable lines are counted" {
				copyables = &Copyables{{Label: "Alias", Value: "my.alias"}}
			}
			res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{
				Kind: "reply", Text: tc.text, Media: media, Copyables: copyables, Link: orderLink,
			}))
			if !res.OK || res.HubMsgID != "5550001111:7001" {
				t.Fatalf("result = %+v (hub_msg_id must be the media's)", res)
			}
			sends := w.customer.Sends()
			if !tc.split {
				if len(sends) != 1 || !sends[0].File || sends[0].Media.HTMLCaption == "" {
					t.Fatalf("sends = %d, want the media with its caption", len(sends))
				}
				return
			}
			if len(sends) != 2 {
				t.Fatalf("%d sends, want media then text", len(sends))
			}
			if !sends[0].File || sends[0].Media.HTMLCaption != "" || len(sends[0].Media.Buttons) != 0 {
				t.Errorf("media = %+v", sends[0].Media)
			}
			if sends[1].File || !strings.HasPrefix(sends[1].Text.HTML, tc.text[:4]) || len(sends[1].Text.Buttons) == 0 {
				t.Errorf("text message = %+v", sends[1].Text)
			}
			if strings.Contains(sends[1].Text.HTML, "<code>") != (copyables != nil) {
				t.Errorf("copyable lines belong in the text message: %q", sends[1].Text.HTML)
			}
			// Both messages are mirrored and paired.
			if len(w.topics.Posts()) != 2 || len(w.customer.Pairs()) != 2 {
				t.Errorf("mirror posts = %d, pairs = %d, want 2 and 2", len(w.topics.Posts()), len(w.customer.Pairs()))
			}
			if p := w.customer.Pairs(); p[0].CustMsg != 7001 || p[1].CustMsg != 7002 {
				t.Errorf("pairs = %+v", p)
			}
		})
	}
}

func TestTelegramTextTooLongIsInvalid(t *testing.T) {
	w := tgWorld(t)
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{Kind: "reply", Text: strings.Repeat("a", 4097)}))
	if res.OK || res.Error != ErrInvalid || len(w.customer.Sends()) != 0 {
		t.Errorf("result = %+v", res)
	}
}

// A6: a blocked bot or a deleted chat is customer_unreachable and leaves no
// mirror; Telegram's 429 is rate_limited.
func TestTelegramSendFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"blocked (403)":      {fmt.Errorf("%w: Forbidden: bot was blocked by the user", ErrCustomerBlocked), ErrCustomerUnreachable},
		"rate limited (429)": {fmt.Errorf("%w: Too Many Requests: retry after 5", ErrCustomerRateLimited), ErrRateLimited},
		"anything else":      {errors.New("network is down"), ErrInternal},
	} {
		t.Run(name, func(t *testing.T) {
			w := tgWorld(t)
			w.customer.failIf = func(custSend) error { return tc.err }
			res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{Kind: "reply", Text: "hi"}))
			if res.OK || res.Error != tc.want || res.HubMsgID != "" || res.CommandID != "CMD1" {
				t.Fatalf("result = %+v", res)
			}
			if n := len(w.topics.Posts()); n != 0 {
				t.Errorf("%d mirror posts after a failed send, want none", n)
			}
			if n := len(w.customer.Pairs()); n != 0 {
				t.Errorf("%d pairs recorded, want none", n)
			}
		})
	}
}

func TestTelegramPartialAnswerSaysSo(t *testing.T) {
	w := tgWorld(t)
	w.customer.failIf = func(s custSend) error {
		if !s.File {
			return errors.New("telegram hiccup")
		}
		return nil
	}
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{
		Kind: "reply", Text: strings.Repeat("a", 1100),
		Media: []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}},
	}))
	if res.OK || res.Error != ErrInternal || res.HubMsgID != "5550001111:7001" {
		t.Fatalf("result = %+v", res)
	}
	var notes int
	for _, p := range w.topics.Posts() {
		if strings.HasPrefix(p.Text, "ℹ️ Only part of the last answer") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("%d notes about the partial answer, want 1; posts = %+v", notes, w.topics.Posts())
	}
}

func TestTelegramReplySucceedsEvenIfMirrorFails(t *testing.T) {
	w := tgWorld(t)
	w.topics.fail = errors.New("telegram down")
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{Kind: "reply", Text: "hi"}))
	if !res.OK || res.HubMsgID != "5550001111:7001" {
		t.Fatalf("result = %+v", res)
	}
	if n := len(w.customer.Pairs()); n != 0 {
		t.Error("a pair was recorded for a mirror that was never posted")
	}
}

func TestTelegramUnknownConversations(t *testing.T) {
	w := tgWorld(t)
	e := w.executor()
	// No topic: the Hub has never heard from this customer.
	res := e.Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "hi", Conversation: "tg:5550009999"}))
	if res.OK || res.Error != ErrUnknownConversation {
		t.Errorf("no topic: %+v", res)
	}
	// The customer bot is off.
	e.Customer = nil
	res = e.Handle(context.Background(), tgSend(t, w, "CMD2", Send{Kind: "reply", Text: "hi"}))
	if res.OK || res.Error != ErrUnknownConversation {
		t.Errorf("customer bot off: %+v", res)
	}
	// Ids that are not users.
	e.Customer = w.customer
	for i, conv := range []string{"tg:-1001234", "tg:0", "tg:abc", "tg:012"} {
		res = e.Handle(context.Background(), sendEnv(t, w, fmt.Sprintf("CMD-%d", i), Send{Kind: "reply", Text: "hi", Conversation: conv}))
		if res.OK || res.Error != ErrUnknownConversation {
			t.Errorf("%s: %+v", conv, res)
		}
	}
	if len(w.customer.Sends()) != 0 {
		t.Error("something was sent")
	}
}

// Notes and cards are topic-only and work for a Telegram customer as they do
// for a WhatsApp one.
func TestTelegramNoteAndCardStayInTheTopic(t *testing.T) {
	w := tgWorld(t)
	e := w.executor()
	if res := e.Handle(context.Background(), tgSend(t, w, "CMD1", Send{Kind: "note", Text: "Slip received."})); !res.OK {
		t.Fatalf("note: %+v", res)
	}
	if res := e.Handle(context.Background(), tgSend(t, w, "CMD2", Send{
		Kind: "card", Text: "Draft", CardID: "c1", Buttons: &Buttons{{{ID: "send", Label: "Send"}}},
	})); !res.OK {
		t.Fatalf("card: %+v", res)
	}
	if p := w.topics.Posts(); len(p) != 1 || p[0].Thread != 1250 || p[0].Text != "ℹ️ <i>Slip received.</i>" {
		t.Errorf("posts = %+v", p)
	}
	if c := w.topics.Cards(); len(c) != 1 || c[0].Thread != 1250 {
		t.Errorf("cards = %+v", c)
	}
	if len(w.customer.Sends()) != 0 || len(w.wa.Calls()) != 0 {
		t.Error("a note or card reached a customer")
	}
}

/* ------------------------------------------------------------- A7: WhatsApp -- */

// A7: link on WhatsApp is a `label: url` line of the main message.
func TestWhatsAppReplyLinkIsALineOfTheText(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	res := e.Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Text: "Got your order.", Link: orderLink}))
	if !res.OK || res.HubMsgID != "WA001" {
		t.Fatalf("result = %+v", res)
	}
	line := "View your order: " + orderLink.URL
	calls := w.wa.Calls()
	if len(calls) != 1 || calls[0].Text != "Got your order.\n"+line {
		t.Fatalf("calls = %+v", calls)
	}
	if p := w.topics.Posts(); len(p) != 1 || p[0].Text != "🤖 <blockquote>Got your order.</blockquote>\n"+line {
		t.Errorf("mirror = %+v", p)
	}
}

func TestWhatsAppReplyLinkWithMediaAndCopyables(t *testing.T) {
	w := newWorld(t)
	copyables := Copyables{{Label: "Alias", Value: "my.alias"}}
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Text: "DuitNow QR", Link: orderLink, Copyables: &copyables,
		Media: []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}},
	}))
	if !res.OK {
		t.Fatal(res)
	}
	line := "View your order: " + orderLink.URL
	calls := w.wa.Calls()
	// The captioned image, then the copyable's value alone.
	if len(calls) != 2 || calls[0].Kind != "image" || calls[0].Text != "DuitNow QR\n"+line || calls[1].Text != "my.alias" {
		t.Fatalf("calls = %+v", calls)
	}
	if p := w.topics.Posts(); len(p) != 1 || p[0].Text != "🤖 <blockquote>DuitNow QR</blockquote>\nAlias: my.alias\n"+line {
		t.Errorf("mirror = %+v", p)
	}
}

func TestWhatsAppReplyWithOnlyALinkAndMedia(t *testing.T) {
	w := newWorld(t)
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Link: orderLink,
		Media: []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}},
	}))
	if !res.OK {
		t.Fatal(res)
	}
	if c := w.wa.Calls(); len(c) != 1 || c[0].Text != "View your order: "+orderLink.URL {
		t.Errorf("calls = %+v", c)
	}
}

/* ----------------------------------------------------------- protocol -- */

// A8: the new fixture decodes into the link field and the copy in testdata is
// the clarus-agent one (TestFixtureCopiesMatchTheAgentRepo checks the bytes).
func TestSendReplyLinkFixtureDecodes(t *testing.T) {
	raw, err := os.ReadFile("testdata/agent.send-reply-link.json")
	if err != nil {
		t.Fatal(err)
	}
	env, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := DecodePayload(env)
	if err != nil {
		t.Fatal(err)
	}
	s := p.(*Send)
	if s.Conversation != "tg:5550001111" || s.Link == nil || s.Link.Label != "View your order" ||
		!strings.HasPrefix(s.Link.URL, "https://clarusmolecular.com/order/") {
		t.Errorf("send = %+v link = %+v", s, s.Link)
	}
	if !json.Valid(env.Payload) {
		t.Error("payload is not JSON")
	}
}

func TestSendLinkValidation(t *testing.T) {
	base := `{"conversation":"tg:5550001111","kind":"reply","text":"x","media":[],"expires_at":"2026-10-01T09:40:12.345Z","link":%s}`
	for name, link := range map[string]string{
		"ok":             `{"label":"Order","url":"https://example.com/o/1"}`,
		"http is fine":   `{"label":"Order","url":"http://example.com/o/1"}`,
		"empty label":    `{"label":"","url":"https://example.com"}`,
		"label too long": `{"label":"` + strings.Repeat("a", 65) + `","url":"https://example.com"}`,
		"javascript url": `{"label":"Order","url":"javascript:alert(1)"}`,
		"no scheme":      `{"label":"Order","url":"example.com/o/1"}`,
		"no host":        `{"label":"Order","url":"https:///x"}`,
		"tg deep link":   `{"label":"Order","url":"tg://resolve?domain=x"}`,
	} {
		var s Send
		if err := json.Unmarshal([]byte(fmt.Sprintf(base, link)), &s); err != nil {
			t.Fatal(err)
		}
		err := s.Validate()
		if want := name == "ok" || name == "http is fine"; (err == nil) != want {
			t.Errorf("%s: Validate() = %v", name, err)
		}
	}
	// An invalid link makes the command invalid, and nothing is sent.
	w := tgWorld(t)
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{Kind: "reply", Text: "x", Link: &SendLink{Label: "x", URL: "javascript:1"}}))
	if res.OK || res.Error != ErrInvalid || len(w.customer.Sends()) != 0 {
		t.Errorf("result = %+v", res)
	}
}
