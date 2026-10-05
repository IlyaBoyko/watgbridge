package tgcustomer

import (
	"errors"
	"strings"
	"testing"

	"watgbridge/agentlink"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Protocol section 5c: the Agent's HTML is sent as given with HTML parse mode;
// when Telegram cannot parse it, it is sent once more as plain text.

const okMessage = `{"ok":true,"result":{"message_id":77,"date":1,"chat":{"id":5550001111,"type":"private"}}}`

func TestCustomerBotFormattedTextFallsBackToPlainOnAParseError(t *testing.T) {
	f := newFakeTG(t)
	cb := newCustomerBot(t, f)
	core, logs := observer.New(zap.WarnLevel)
	cb.log = zap.New(core)

	f.respond["sendMessage"] = func(c tgCall) (int, string) {
		if c.Form["parse_mode"] == "HTML" {
			return tgError(400, "Bad Request: can't parse entities: Unsupported start tag", "")(c)
		}
		return 200, okMessage
	}
	id, err := cb.SendText(t.Context(), customerID, agentlink.CustomerText{
		HTML: "<b>Hi</b> a &lt; b <x>", Formatted: true,
		Buttons: [][]agentlink.CustomerButton{{{Text: "Copy", CopyText: "v"}}}, ReplyTo: 5,
	})
	if err != nil || id != 77 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	got := f.CallsTo("sendMessage")
	if len(got) != 2 {
		t.Fatalf("%d sendMessage calls, want the HTML try and one plain retry", len(got))
	}
	if got[0].Form["text"] != "<b>Hi</b> a &lt; b <x>" || got[0].Form["parse_mode"] != "HTML" {
		t.Errorf("first try = %v", got[0].Form)
	}
	if _, has := got[1].Form["parse_mode"]; has || got[1].Form["text"] != "Hi a < b " {
		t.Errorf("retry = %v", got[1].Form)
	}
	if !strings.Contains(got[1].Form["reply_markup"], "copy_text") || !strings.Contains(got[1].Form["reply_parameters"], `"message_id":5`) {
		t.Errorf("the retry lost the keyboard or the quote: %v", got[1].Form)
	}
	if logs.Len() != 1 {
		t.Errorf("%d warnings, want 1", logs.Len())
	}
}

// A formatted text that parses is sent once, with HTML parse mode, untouched.
func TestCustomerBotFormattedTextThatParsesIsSentOnce(t *testing.T) {
	f := newFakeTG(t)
	cb := newCustomerBot(t, f)
	if _, err := cb.SendText(t.Context(), customerID, agentlink.CustomerText{HTML: "<b>x</b> &amp; y", Formatted: true}); err != nil {
		t.Fatal(err)
	}
	got := f.CallsTo("sendMessage")
	if len(got) != 1 || got[0].Form["text"] != "<b>x</b> &amp; y" || got[0].Form["parse_mode"] != "HTML" {
		t.Errorf("calls = %+v", got)
	}
}

// Text the Hub escaped itself is never retried, and a failure that is not a
// parse error is not retried either.
func TestCustomerBotDoesNotRetryOtherFailures(t *testing.T) {
	f := newFakeTG(t)
	f.respond["sendMessage"] = tgError(400, "Bad Request: can't parse entities", "")
	cb := newCustomerBot(t, f)
	if _, err := cb.SendText(t.Context(), customerID, agentlink.CustomerText{HTML: "<b>", Formatted: false}); err == nil {
		t.Error("an unformatted parse error was swallowed")
	}
	if n := len(f.CallsTo("sendMessage")); n != 1 {
		t.Errorf("%d attempts for an escaped text, want 1", n)
	}

	f = newFakeTG(t)
	f.respond["sendMessage"] = tgError(403, "Forbidden: bot was blocked by the user", "")
	cb = newCustomerBot(t, f)
	if _, err := cb.SendText(t.Context(), customerID, agentlink.CustomerText{HTML: "<b>x</b>", Formatted: true}); !errors.Is(err, agentlink.ErrCustomerBlocked) {
		t.Errorf("err = %v", err)
	}
	if n := len(f.CallsTo("sendMessage")); n != 1 {
		t.Errorf("%d attempts after a 403, want 1", n)
	}

	// A formatted text that keeps failing is tried twice, no more.
	f = newFakeTG(t)
	f.respond["sendMessage"] = tgError(400, "Bad Request: can't parse entities", "")
	cb = newCustomerBot(t, f)
	if _, err := cb.SendText(t.Context(), customerID, agentlink.CustomerText{HTML: "<b>x</b>", Formatted: true}); err == nil {
		t.Error("two failures reported as success")
	}
	if n := len(f.CallsTo("sendMessage")); n != 2 {
		t.Errorf("%d attempts, want 2", n)
	}
}

func TestCustomerBotFormattedCaptionFallsBackToPlain(t *testing.T) {
	f := newFakeTG(t)
	f.respond["sendPhoto"] = func(c tgCall) (int, string) {
		if c.Form["parse_mode"] == "HTML" {
			return tgError(400, "Bad Request: can't parse entities", "")(c)
		}
		return 200, okMessage
	}
	cb := newCustomerBot(t, f)
	id, err := cb.SendFile(t.Context(), customerID, agentlink.CustomerFile{
		Kind: "image", Data: []byte("D"), HTMLCaption: "<b>Cap</b>", FormattedCaption: true,
	})
	if err != nil || id != 77 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	got := f.CallsTo("sendPhoto")
	if len(got) != 2 || got[1].Form["caption"] != "Cap" || string(got[1].Files["photo"]) != "D" {
		t.Fatalf("calls = %+v", got)
	}
	if _, has := got[1].Form["parse_mode"]; has {
		t.Error("the retry still asked for HTML")
	}
}
