package agentlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Protocol section 5c: `format: "html"` on a send.

const priceHTML = "<b>Price list</b>\n<s>RM 100</s> RM 80 &amp; <a href=\"https://clarusmolecular.com/p?a=1&amp;b=2\">shop</a>"

// format is optional, only "html" is valid, and it survives a round trip.
func TestSendFormatDecodesAndValidates(t *testing.T) {
	env := func(extra string) Envelope {
		return Envelope{V: 1, ID: "x", Type: "send", TS: "2026-10-01T09:30:12.345Z",
			Payload: json.RawMessage(`{"conversation":"tg:5550001111","kind":"reply","text":"<b>x</b>","media":[]` + extra + `,"expires_at":"2026-10-01T09:40:12.345Z"}`)}
	}
	p, err := DecodePayload(env(`,"format":"html"`))
	if err != nil || p.(*Send).Format != FormatHTML {
		t.Fatalf("html: %+v err=%v", p, err)
	}
	if out := mustJSON(t, p); !bytes.Contains(out, []byte(`"format":"html"`)) {
		t.Errorf("format lost on re-encode: %s", out)
	}
	p, err = DecodePayload(env(``))
	if err != nil || p.(*Send).Format != "" {
		t.Fatalf("absent: %+v err=%v", p, err)
	}
	if out := mustJSON(t, p); bytes.Contains(out, []byte(`"format"`)) {
		t.Errorf("an absent format was written: %s", out)
	}
	for _, bad := range []string{`"markdown"`, `"HTML"`, `"plain"`} {
		if _, err := DecodePayload(env(`,"format":` + bad)); err == nil {
			t.Errorf("format %s accepted", bad)
		}
	}
}

// The Agent's HTML goes to the customer exactly as given, the topic mirror
// keeps it under the robot and above the signature, and the pair is recorded.
func TestTelegramFormattedReplyIsSentAsGivenAndMirrorKeepsIt(t *testing.T) {
	w := tgWorld(t)
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{
		Kind: "reply", Format: FormatHTML, Text: priceHTML, Signature: "Sent by Loki",
	}))
	if !res.OK || res.HubMsgID != "5550001111:7001" {
		t.Fatalf("result = %+v", res)
	}
	sends := w.customer.Sends()
	if len(sends) != 1 || sends[0].Text.HTML != priceHTML || !sends[0].Text.Formatted {
		t.Fatalf("sends = %+v", sends)
	}
	want := "🤖 " + priceHTML + "\n<i>✓ Sent by Loki</i>"
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].Text != want || !posts[0].HTML {
		t.Errorf("mirror = %+v\nwant %q", posts, want)
	}
	if len(w.customer.Pairs()) != 1 {
		t.Errorf("pairs = %+v", w.customer.Pairs())
	}
}

// Without the field nothing changes: escaped, and not marked as formatted, so
// the channel never retries it as plain text.
func TestTelegramReplyWithoutFormatIsEscapedAndNotFormatted(t *testing.T) {
	w := tgWorld(t)
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{Kind: "reply", Text: "<b>x</b>"}))
	if !res.OK {
		t.Fatal(res)
	}
	got := w.customer.Sends()[0].Text
	if got.HTML != "&lt;b&gt;x&lt;/b&gt;" || got.Formatted {
		t.Errorf("message = %+v", got)
	}
	if p := w.topics.Posts(); p[0].Text != "🤖 &lt;b&gt;x&lt;/b&gt;" {
		t.Errorf("mirror = %+v", p)
	}
}

// Copyables and the link behave as for plain replies; only the main text is
// the Agent's HTML.
func TestTelegramFormattedReplyKeepsCopyablesAndLink(t *testing.T) {
	w := tgWorld(t)
	copyables := Copyables{{Label: "Alias", Value: "a<b"}}
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{
		Kind: "reply", Format: FormatHTML, Text: "<b>Pay</b>", Copyables: &copyables, Link: orderLink,
	}))
	if !res.OK {
		t.Fatal(res)
	}
	s := w.customer.Sends()[0].Text
	if s.HTML != "<b>Pay</b>\nAlias: <code>a&lt;b</code>" || len(s.Buttons) != 2 || s.Buttons[0][0].CopyText != "a<b" {
		t.Errorf("message = %+v", s)
	}
	want := "🤖 <b>Pay</b>\nAlias: a&lt;b\nView your order: " + orderLink.URL
	if p := w.topics.Posts(); p[0].Text != want {
		t.Errorf("mirror = %q\nwant %q", p[0].Text, want)
	}
}

// The caption limit counts what Telegram shows, not the tags; the mirror keeps
// the HTML only while it fits, like every mirror.
func TestTelegramFormattedCaptionIsMeasuredWithoutTags(t *testing.T) {
	w := tgWorld(t)
	text := "<b>" + strings.Repeat("a", 1020) + "</b>"
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{
		Kind: "reply", Format: FormatHTML, Text: text,
		Media: []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}},
	}))
	if !res.OK {
		t.Fatal(res)
	}
	sends := w.customer.Sends()
	if len(sends) != 1 || !sends[0].File || sends[0].Media.HTMLCaption != text || !sends[0].Media.FormattedCaption {
		t.Fatalf("sends = %+v", sends)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].HTML || posts[0].Text != "🤖 "+strings.Repeat("a", 1020) {
		t.Errorf("mirror = %+v", posts)
	}
}

// If the topic refuses the formatted mirror, it gets the plain form rather
// than nothing.
func TestTelegramFormattedMirrorFallsBackToPlainWhenTheTopicRefuses(t *testing.T) {
	w := tgWorld(t)
	w.topics.failText = func(text string) error {
		if strings.Contains(text, "<b>") {
			return errors.New("Bad Request: can't parse entities")
		}
		return nil
	}
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{Kind: "reply", Format: FormatHTML, Text: "<b>Hi</b> a &lt; b"}))
	if !res.OK {
		t.Fatal(res)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].Text != "🤖 Hi a &lt; b" || len(w.customer.Pairs()) != 1 {
		t.Errorf("mirror = %+v pairs = %+v", posts, w.customer.Pairs())
	}
}

// WhatsApp text is sent as is: the field is ignored there.
func TestWhatsAppReplyIgnoresFormat(t *testing.T) {
	w := newWorld(t)
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{Kind: "reply", Format: FormatHTML, Text: "*Price* ~RM 100~ <b>x</b>"}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	calls := w.wa.Calls()
	if len(calls) != 1 || calls[0].Text != "*Price* ~RM 100~ <b>x</b>" {
		t.Fatalf("whatsapp calls = %+v", calls)
	}
	if p := w.topics.Posts(); len(p) != 1 || p[0].Text != "🤖 *Price* ~RM 100~ &lt;b&gt;x&lt;/b&gt;" {
		t.Errorf("mirror = %+v", p)
	}
}

func TestStripHTML(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                                         "plain",
		"<b>Bold</b> <i>i</i> <s>s</s> <u>u</u>":        "Bold i s u",
		"a &lt;b&gt; &amp; c":                           "a <b> & c",
		"<code>1234 &amp; 5</code>":                     "1234 & 5",
		"<b>&lt;b&gt;</b>":                              "<b>",
		`<a href="https://x.io/p?a=1&amp;b=2">shop</a>`: "shop (https://x.io/p?a=1&b=2)",
		`<a href="https://x.io">https://x.io</a>`:       "https://x.io",
		"<b>unclosed":                                   "unclosed",
	} {
		if got := StripHTML(in); got != want {
			t.Errorf("StripHTML(%q) = %q, want %q", in, got, want)
		}
	}
}
