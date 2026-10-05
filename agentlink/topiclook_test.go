package agentlink

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"watgbridge/state"
)

// Protocol section 5b: how the Agent's posts look in the staff topic.

/* ------------------------------------------------------------- rendering -- */

func TestRenderMirrorEscapesAndHasNoQuoteBox(t *testing.T) {
	got, ok := renderMirror(mirrorParts{
		Text:  "a < b & \"c\" > d",
		Lines: []string{"Bank <A>: 12 & 3", "View: https://x.test/?a=1&b=2"},
	}, tgTextLimit)
	want := "🤖 a &lt; b &amp; &#34;c&#34; &gt; d\nBank &lt;A&gt;: 12 &amp; 3\nView: https://x.test/?a=1&amp;b=2"
	if !ok || !got.HTML || got.Text != want {
		t.Errorf("got %+v (ok=%v), want HTML %q", got, ok, want)
	}
}

func TestRenderMirrorSignatureOnlyWhenSet(t *testing.T) {
	without, _ := renderMirror(mirrorParts{Text: "Boleh!"}, tgTextLimit)
	if strings.Contains(without.Text, "✓") {
		t.Errorf("no signature set, yet the mirror says %q", without.Text)
	}
	with, _ := renderMirror(mirrorParts{Text: "Boleh!", Lines: []string{"Alias: x"}, Signature: "Sent by Loki <16:01>"}, tgTextLimit)
	want := "🤖 Boleh!\nAlias: x\n<i>✓ Sent by Loki &lt;16:01&gt;</i>"
	if with.Text != want || !with.HTML {
		t.Errorf("got %q, want %q", with.Text, want)
	}
}

func TestRenderMirrorWithoutTextIsJustTheRobot(t *testing.T) {
	got, _ := renderMirror(mirrorParts{}, tgCaptionLimit)
	if got.Text != "🤖" {
		t.Errorf("text-less mirror = %q", got.Text)
	}
	got, _ = renderMirror(mirrorParts{Signature: "Loki"}, tgCaptionLimit)
	if got.Text != "🤖\n<i>✓ Loki</i>" {
		t.Errorf("signed text-less mirror = %q", got.Text)
	}
}

func TestRenderMirrorFallsBackToPlainWhenFormattingWouldExceedTheLimit(t *testing.T) {
	// 600 ampersands are 600 characters plain but 3000 once escaped.
	text := strings.Repeat("&", 600)
	got, ok := renderMirror(mirrorParts{Text: text, Signature: "Loki"}, tgCaptionLimit)
	if !ok || got.HTML || got.Text != "🤖 "+text+"\n✓ Loki" {
		t.Errorf("got HTML=%v ok=%v len=%d, want the plain fallback", got.HTML, ok, len(got.Text))
	}
	if tgLen(got.Text) > tgCaptionLimit {
		t.Errorf("fallback is %d units, over the limit", tgLen(got.Text))
	}

	// Plain text too long as well: the caller is told, and still gets the text.
	long := strings.Repeat("x", tgCaptionLimit)
	got, ok = renderMirror(mirrorParts{Text: long}, tgCaptionLimit)
	if ok || got.HTML {
		t.Errorf("over-long plain text: ok=%v HTML=%v", ok, got.HTML)
	}
}

func TestRenderCard(t *testing.T) {
	if got := renderCard("", "Draft <1>"); got != (TopicText{Text: "Draft <1>"}) {
		t.Errorf("no title must leave the text alone and plain: %+v", got)
	}
	if got := renderCard("  ", "x"); got.HTML {
		t.Errorf("a blank title counts as none: %+v", got)
	}
	got := renderCard("Draft & ready", "Boleh <b>")
	if want := "📝 <b>Draft &amp; ready</b>\n<blockquote>Boleh &lt;b&gt;</blockquote>"; got.Text != want || !got.HTML {
		t.Errorf("got %+v, want %q", got, want)
	}
	long := renderCard("T", strings.Repeat("&", 1000))
	if long.HTML || long.Text != "📝 T\n"+strings.Repeat("&", 1000) {
		t.Errorf("long card must fall back to plain: HTML=%v", long.HTML)
	}
}

func TestRenderNote(t *testing.T) {
	if got := renderNote("Slip <ok> & done"); got != (TopicText{Text: "ℹ️ <i>Slip &lt;ok&gt; &amp; done</i>", HTML: true}) {
		t.Errorf("got %+v", got)
	}
	if got := renderNote(strings.Repeat("&", 1000)); got.HTML || !strings.HasPrefix(got.Text, "ℹ️ &&") {
		t.Errorf("long note must fall back to plain: HTML=%v", got.HTML)
	}
}

func TestRenderSigned(t *testing.T) {
	if got := renderSigned("Sent by Loki"); got.Text != "<i>✓ Sent by Loki</i>" || !got.HTML {
		t.Errorf("got %+v", got)
	}
	if got := renderSigned(""); got.Text != "<i>✓ Sent</i>" {
		t.Errorf("got %+v", got)
	}
}

/* -------------------------------------------------------------- decoding -- */

func TestDecodeSendWithNewOptionalFields(t *testing.T) {
	env := func(payload string) Envelope {
		return Envelope{V: 1, ID: "x", Type: "send", TS: "2026-10-01T09:30:12.345Z", Payload: json.RawMessage(payload)}
	}
	p, err := DecodePayload(env(`{"conversation":"wa:1@s.whatsapp.net","kind":"reply","text":"hi","media":[],"signature":"Sent by Loki · 16:01","replaces_card":"draft_1","title":"Draft","expires_at":"2026-10-01T09:40:12.345Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	s := p.(*Send)
	if s.Signature != "Sent by Loki · 16:01" || s.ReplacesCard != "draft_1" || s.Title != "Draft" {
		t.Errorf("decoded %+v", s)
	}
	back := string(mustJSON(t, s))
	for _, want := range []string{`"signature":"Sent by Loki · 16:01"`, `"replaces_card":"draft_1"`, `"title":"Draft"`} {
		if !strings.Contains(back, want) {
			t.Errorf("re-encoded send lost %s: %s", want, back)
		}
	}

	// An older Agent sends none of them: they stay empty and are not encoded.
	p, err = DecodePayload(env(`{"conversation":"wa:1@s.whatsapp.net","kind":"reply","text":"hi","media":[],"expires_at":"2026-10-01T09:40:12.345Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := p.(*Send); s.Signature != "" || s.ReplacesCard != "" || s.Title != "" {
		t.Errorf("decoded %+v", s)
	}
	back = string(mustJSON(t, p))
	for _, field := range []string{"signature", "replaces_card", "title"} {
		if strings.Contains(back, field) {
			t.Errorf("%s must not appear when unset: %s", field, back)
		}
	}
}

func TestDecodeEditCardWithAndWithoutTitle(t *testing.T) {
	env := func(payload string) Envelope {
		return Envelope{V: 1, ID: "x", Type: "edit_card", TS: "2026-10-01T09:30:12.345Z", Payload: json.RawMessage(payload)}
	}
	p, err := DecodePayload(env(`{"conversation":"wa:1@s.whatsapp.net","card_id":"c","text":"t","title":"Sent","buttons":[],"expires_at":"2026-10-01T09:40:12.345Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if e := p.(*EditCard); e.Title != "Sent" || e.Buttons == nil {
		t.Errorf("decoded %+v", e)
	}
	p, err = DecodePayload(env(`{"conversation":"wa:1@s.whatsapp.net","card_id":"c","text":"t","expires_at":"2026-10-01T09:40:12.345Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if e := p.(*EditCard); e.Title != "" || strings.Contains(string(mustJSON(t, e)), "title") {
		t.Errorf("decoded %+v", e)
	}
}

func TestDecodeRejectsTooLongSignatureAndTitle(t *testing.T) {
	long := func(n int) string { return strings.Repeat("é", n) }
	frame := func(typ, extra string) Envelope {
		payload := `{"conversation":"wa:1@s.whatsapp.net","kind":"reply","text":"x","media":[],` + extra + `"expires_at":"2026-10-01T09:40:12.345Z"}`
		if typ == "edit_card" {
			payload = `{"conversation":"wa:1@s.whatsapp.net","card_id":"c","text":"x",` + extra + `"expires_at":"2026-10-01T09:40:12.345Z"}`
		}
		return Envelope{V: 1, ID: "x", Type: typ, TS: "2026-10-01T09:30:12.345Z", Payload: json.RawMessage(payload)}
	}
	// Limits count characters, not bytes: 80 and 120 two-byte characters pass.
	for name, e := range map[string]Envelope{
		"signature at the limit": frame("send", `"signature":"`+long(80)+`",`),
		"title at the limit":     frame("send", `"title":"`+long(120)+`",`),
		"edit title at limit":    frame("edit_card", `"title":"`+long(120)+`",`),
	} {
		if _, err := DecodePayload(e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, e := range map[string]Envelope{
		"signature over":  frame("send", `"signature":"`+long(81)+`",`),
		"title over":      frame("send", `"title":"`+long(121)+`",`),
		"edit title over": frame("edit_card", `"title":"`+long(121)+`",`),
	} {
		if _, err := DecodePayload(e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

/* ---------------------------------------------------------------- mirror -- */

func TestSignatureIsOnlyOnTheTopicMirror(t *testing.T) {
	w := newWorld(t)
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Text: "Boleh!", Signature: "Sent by Loki · 16:01",
		Copyables: &Copyables{{Label: "Alias", Value: "clarus.pagos"}},
	}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	posts := w.topics.Posts()
	want := "🤖 Boleh!\nAlias: clarus.pagos\n<i>✓ Sent by Loki · 16:01</i>"
	if len(posts) != 1 || posts[0].Text != want || !posts[0].HTML {
		t.Fatalf("mirror = %+v, want %q", posts, want)
	}
	for _, c := range w.wa.Calls() {
		if strings.Contains(c.Text, "Loki") || strings.Contains(c.Text, "✓") || strings.Contains(c.Text, "<") {
			t.Errorf("the customer got topic formatting: %+v", c)
		}
	}
	// Staff replying to the mirror still quotes the WhatsApp message.
	if pairs := w.bridge.Pairs(); len(pairs) != 1 || pairs[0].WaMsgID != "WA001" || pairs[0].TgMsgID != 9001 {
		t.Errorf("pairs = %+v", pairs)
	}
}

func TestWhatsAppSignatureGoesOnTheLastMessageOnly(t *testing.T) {
	w := newWorld(t)
	img := Media{Kind: "image", Mime: "image/png", Filename: "a.png", Size: 70, Data: pngB64}
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Text: "Two pictures", Signature: "Loki", Media: []Media{img, img},
	}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	posts := w.topics.Posts()
	if len(posts) != 2 {
		t.Fatalf("posts = %+v", posts)
	}
	if strings.Contains(posts[0].Text, "✓") || !strings.HasSuffix(posts[1].Text, "<i>✓ Loki</i>") {
		t.Errorf("signature placement: first %q, last %q", posts[0].Text, posts[1].Text)
	}
	if len(w.bridge.Pairs()) != 2 {
		t.Errorf("both mirror posts must stay paired: %+v", w.bridge.Pairs())
	}
}

func TestWhatsAppMirrorFallsBackToPlainOverTheLimit(t *testing.T) {
	w := newWorld(t)
	text := strings.Repeat("&", 600) // over 1024 once escaped, under it plain
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "CMD1", Send{
		Kind: "reply", Text: text, Signature: "Loki",
		Media: []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}},
	}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].HTML || posts[0].Text != "🤖 "+text+"\n✓ Loki" {
		t.Errorf("mirror = HTML %v, %d bytes", posts[0].HTML, len(posts[0].Text))
	}
}

func TestTelegramSignatureIsOnTheMirrorNotTheCustomer(t *testing.T) {
	w := tgWorld(t)
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{
		Kind: "reply", Text: "Pay <now>", Signature: "Sent automatically · 16:01",
		Copyables: &Copyables{{Label: "Bank", Value: "1234"}}, Link: orderLink,
	}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	want := "🤖 Pay &lt;now&gt;\nBank: 1234\nView your order: " + orderLink.URL + "\n<i>✓ Sent automatically · 16:01</i>"
	if posts := w.topics.Posts(); len(posts) != 1 || posts[0].Text != want || !posts[0].HTML {
		t.Fatalf("mirror = %+v, want %q", posts, want)
	}
	sends := w.customer.Sends()
	if len(sends) != 1 || strings.Contains(sends[0].Text.HTML, "✓") || strings.Contains(sends[0].Text.HTML, "blockquote") {
		t.Errorf("the customer got topic formatting: %+v", sends)
	}
	if len(w.customer.Pairs()) != 1 {
		t.Errorf("pairs = %+v", w.customer.Pairs())
	}
}

func TestTelegramLongCaptionSignatureFollowsTheText(t *testing.T) {
	w := tgWorld(t)
	text := strings.Repeat("a", 1500) // over the caption limit: file first, text second
	res := w.executor().Handle(context.Background(), tgSend(t, w, "CMD1", Send{
		Kind: "reply", Text: text, Signature: "Loki",
		Media: []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}},
	}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	posts := w.topics.Posts()
	if len(posts) != 2 || posts[0].Media == nil || posts[0].Text != "🤖" ||
		posts[1].Text != "🤖 "+text+"\n<i>✓ Loki</i>" {
		t.Fatalf("posts = %+v", posts)
	}
}

/* ------------------------------------------------------------ note, cards -- */

func TestNoteIsEscapedAndItalic(t *testing.T) {
	w := newWorld(t)
	res := w.executor().Handle(context.Background(), sendEnv(t, w, "N1", Send{Kind: "note", Text: "Slip <b>&</b>"}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	if posts := w.topics.Posts(); len(posts) != 1 || posts[0].Text != "ℹ️ <i>Slip &lt;b&gt;&amp;&lt;/b&gt;</i>" || !posts[0].HTML {
		t.Errorf("posts = %+v", posts)
	}
}

func TestCardTitleIsBoldAboveTheQuotedText(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	res := e.Handle(context.Background(), cardEnv(t, w, "C1", Send{
		CardID: "d1", Title: "Draft — sends at 16:04", Text: "Boleh <3>", Buttons: &draftButtons,
	}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	cards := w.topics.Cards()
	if len(cards) != 1 || !cards[0].HTML || len(cards[0].KB) != 2 ||
		cards[0].Text != "📝 <b>Draft — sends at 16:04</b>\n<blockquote>Boleh &lt;3&gt;</blockquote>" {
		t.Fatalf("cards = %+v", cards)
	}

	res = e.Handle(context.Background(), editEnv(t, w, "E1", EditCard{CardID: "d1", Title: "Sent", Text: "Boleh <3>"}))
	if !res.OK {
		t.Fatalf("edit result = %+v", res)
	}
	edits := w.topics.Edits()
	if len(edits) != 1 || !edits[0].HTML || edits[0].Text != "📝 <b>Sent</b>\n<blockquote>Boleh &lt;3&gt;</blockquote>" {
		t.Errorf("edits = %+v", edits)
	}

	// Without a title the text goes out as it always did.
	e.Handle(context.Background(), editEnv(t, w, "E2", EditCard{CardID: "d1", Text: "plain <text>"}))
	if edits := w.topics.Edits(); len(edits) != 2 || edits[1].HTML || edits[1].Text != "plain <text>" {
		t.Errorf("edits = %+v", edits)
	}
}

/* ---------------------------------------------------------- replaces_card -- */

const draftID = "draft_01JB00000000000000000000AA"

// withDraft posts a card for the conversation and returns its Telegram message.
func withDraft(t *testing.T, w *world, e *Executor, conv string) (chatID, msgID int64) {
	t.Helper()
	res := e.Handle(context.Background(), cardEnv(t, w, "CARD-"+conv, Send{
		Conversation: conv, CardID: draftID, Title: "Draft", Text: "Boleh!", Buttons: &draftButtons,
	}))
	if !res.OK {
		t.Fatalf("card result = %+v", res)
	}
	row := cardRowByID(t, w, draftID)
	return row.TgChatID, row.TgMsgID
}

func cardKnown(t *testing.T, w *world) bool {
	t.Helper()
	_, ok, err := NewCardStore(w.db, w.clock).ByCardID(draftID)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestReplacesCardDeletesTheCardAndForgetsIt(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	chatID, msgID := withDraft(t, w, e, testConv)

	res := e.Handle(context.Background(), sendEnv(t, w, "R1", Send{Kind: "reply", Text: "Boleh!", Signature: "Sent by Loki", ReplacesCard: draftID}))
	if !res.OK || res.HubMsgID != "WA001" || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	if got := w.topics.Deleted(); len(got) != 1 || got[0] != [2]int64{chatID, msgID} {
		t.Errorf("deleted = %v, want the card's message %d/%d", got, chatID, msgID)
	}
	if len(w.topics.Edits()) != 0 {
		t.Errorf("a deleted card must not be edited: %+v", w.topics.Edits())
	}
	if cardKnown(t, w) {
		t.Error("the card id must be forgotten")
	}
	if len(w.topics.Posts()) != 1 {
		t.Errorf("posts = %+v, want only the signed mirror", w.topics.Posts())
	}
	res = e.Handle(context.Background(), editEnv(t, w, "E1", EditCard{CardID: draftID, Text: "x"}))
	if res.OK || res.Error != ErrUnknownCard {
		t.Errorf("edit of a replaced card: %+v", res)
	}
}

func TestReplacesCardOnTelegramCustomerReply(t *testing.T) {
	w := tgWorld(t)
	e := w.executor()
	chatID, msgID := withDraft(t, w, e, tgConv)

	res := e.Handle(context.Background(), tgSend(t, w, "R1", Send{Kind: "reply", Text: "Boleh!", Signature: "Loki", ReplacesCard: draftID}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	if got := w.topics.Deleted(); len(got) != 1 || got[0] != [2]int64{chatID, msgID} || cardKnown(t, w) {
		t.Errorf("deleted = %v, card known = %v", got, cardKnown(t, w))
	}
}

func TestReplacesCardWaitsForTheLastMessageOfTheCommand(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	withDraft(t, w, e, testConv)
	img := Media{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}
	// The second picture fails: the customer has half the answer, so the draft stays.
	w.wa.failIf = func(c waCall) error {
		if len(w.wa.calls) == 1 {
			return errors.New("connection lost")
		}
		return nil
	}
	res := e.Handle(context.Background(), sendEnv(t, w, "R1", Send{Kind: "reply", Text: "x", Media: []Media{img, img}, ReplacesCard: draftID, Signature: "Loki"}))
	if res.OK {
		t.Fatalf("result = %+v", res)
	}
	if len(w.topics.Deleted()) != 0 || len(w.topics.Edits()) != 0 || !cardKnown(t, w) {
		t.Errorf("partial reply touched the card: deleted %v, edits %+v", w.topics.Deleted(), w.topics.Edits())
	}
}

func TestReplacesCardEditsTheCardWhenTheDeleteFails(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	chatID, msgID := withDraft(t, w, e, testConv)
	w.topics.deleteFail = errors.New("Forbidden: bot can't delete this message")

	res := e.Handle(context.Background(), sendEnv(t, w, "R1", Send{Kind: "reply", Text: "Boleh!", Signature: "Sent by Loki <16:01>", ReplacesCard: draftID}))
	if !res.OK || res.HubMsgID != "WA001" {
		t.Fatalf("result = %+v", res)
	}
	edits := w.topics.Edits()
	if len(edits) != 1 || edits[0].ChatID != chatID || edits[0].MsgID != msgID || !edits[0].HTML ||
		edits[0].Text != "<i>✓ Sent by Loki &lt;16:01&gt;</i>" || len(edits[0].KB) != 0 {
		t.Errorf("edits = %+v", edits)
	}
	if len(w.topics.Posts()) != 1 {
		t.Errorf("the reply's mirror must still be posted: %+v", w.topics.Posts())
	}
}

func TestReplacesCardWithoutSignatureSaysSent(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	withDraft(t, w, e, testConv)
	w.topics.deleteFail = errors.New("Forbidden")
	e.Handle(context.Background(), sendEnv(t, w, "R1", Send{Kind: "reply", Text: "Boleh!", ReplacesCard: draftID}))
	if edits := w.topics.Edits(); len(edits) != 1 || edits[0].Text != "<i>✓ Sent</i>" {
		t.Errorf("edits = %+v", edits)
	}
}

func TestReplacesCardLeavesTheCardWhenTheReplyFails(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	withDraft(t, w, e, testConv)
	w.wa.fail = errors.New("not connected")

	res := e.Handle(context.Background(), sendEnv(t, w, "R1", Send{Kind: "reply", Text: "Boleh!", Signature: "Loki", ReplacesCard: draftID}))
	if res.OK || res.Error != ErrCustomerUnreachable {
		t.Fatalf("result = %+v", res)
	}
	if len(w.topics.Deleted()) != 0 || len(w.topics.Edits()) != 0 || !cardKnown(t, w) {
		t.Errorf("card was touched: deleted %v, edits %+v, known %v", w.topics.Deleted(), w.topics.Edits(), cardKnown(t, w))
	}

	// Same for a Telegram customer.
	w2 := tgWorld(t)
	e2 := w2.executor()
	withDraft(t, w2, e2, tgConv)
	w2.customer.failIf = func(custSend) error { return errors.New("blocked") }
	res = e2.Handle(context.Background(), tgSend(t, w2, "R1", Send{Kind: "reply", Text: "Boleh!", Signature: "Loki", ReplacesCard: draftID}))
	if res.OK {
		t.Fatalf("result = %+v", res)
	}
	if len(w2.topics.Deleted()) != 0 || len(w2.topics.Edits()) != 0 || !cardKnown(t, w2) {
		t.Error("the Telegram customer's failed reply touched the card")
	}
}

func TestReplacesCardKeepsTheDraftWhenTheMirrorIsMissing(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	withDraft(t, w, e, testConv)
	w.topics.fail = errors.New("topic is closed") // the mirror cannot be posted

	res := e.Handle(context.Background(), sendEnv(t, w, "R1", Send{Kind: "reply", Text: "Boleh!", Signature: "Loki", ReplacesCard: draftID}))
	if !res.OK {
		t.Fatalf("the customer has the reply, so the command succeeds: %+v", res)
	}
	// Deleting would leave no trace of the reply in the topic: shrink the card instead.
	if len(w.topics.Deleted()) != 0 {
		t.Error("the card was deleted although its replacement is not in the topic")
	}
	if edits := w.topics.Edits(); len(edits) != 1 || edits[0].Text != "<i>✓ Loki</i>" || len(edits[0].KB) != 0 {
		t.Errorf("edits = %+v", edits)
	}
}

func TestReplacesCardAlreadyDeletedByStaffCountsAsDone(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	withDraft(t, w, e, testConv)
	w.topics.deleteFail = errors.New("Bad Request: message to delete not found")
	res := e.Handle(context.Background(), sendEnv(t, w, "R1", Send{Kind: "reply", Text: "Boleh!", ReplacesCard: draftID}))
	if !res.OK || len(w.topics.Edits()) != 0 || cardKnown(t, w) {
		t.Errorf("result %+v, edits %+v, known %v", res, w.topics.Edits(), cardKnown(t, w))
	}
}

func TestReplacesCardNamingNoOwnCardIsIgnored(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	withDraft(t, w, e, testConv)

	// An unknown card id and a card of another conversation: the reply still goes out.
	for name, p := range map[string]Send{
		"unknown card":         {Kind: "reply", Text: "a", ReplacesCard: "nope"},
		"another conversation": {Kind: "reply", Text: "b", ReplacesCard: draftID, Conversation: "wa:60111111111@s.whatsapp.net"},
	} {
		w.bridge.threads["60111111111@s.whatsapp.net"] = 77
		res := e.Handle(context.Background(), sendEnv(t, w, "R-"+name, p))
		if !res.OK {
			t.Errorf("%s: result = %+v", name, res)
		}
	}
	if len(w.topics.Deleted()) != 0 || len(w.topics.Edits()) != 0 || !cardKnown(t, w) {
		t.Error("a card that is not the reply's own must not be touched")
	}
}

func TestReplyWithoutNewFieldsBehavesAsBefore(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	withDraft(t, w, e, testConv)
	res := e.Handle(context.Background(), sendEnv(t, w, "R1", Send{Kind: "reply", Text: "Boleh!"}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	posts := w.topics.Posts()
	if len(posts) != 1 || strings.Contains(posts[0].Text, "✓") {
		t.Errorf("posts = %+v", posts)
	}
	if len(w.topics.Deleted()) != 0 || len(w.topics.Edits()) != 0 || !cardKnown(t, w) {
		t.Error("without replaces_card the card stays as it is")
	}
}

/* ------------------------------------------- no quote box, never customer-bold -- */

// The owner found the quote box hard to scan: the robot and the reply share the
// first line, plain. Cards keep their bold title and quote.
func TestAgentMirrorHasNoQuoteBoxOnEitherChannel(t *testing.T) {
	got, _ := renderMirror(mirrorParts{Text: "Boleh!\nSecond line", Lines: []string{"Alias: x"}, Signature: "Loki"}, tgTextLimit)
	if strings.Contains(got.Text, "blockquote") || !strings.HasPrefix(got.Text, "🤖 Boleh!\nSecond line\nAlias: x") {
		t.Errorf("mirror = %q", got.Text)
	}

	w := newWorld(t)
	w.executor().Handle(context.Background(), sendEnv(t, w, "A1", Send{Kind: "reply", Text: "Boleh!"}))
	tw := tgWorld(t)
	tw.executor().Handle(context.Background(), tgSend(t, tw, "A2", Send{Kind: "reply", Text: "Boleh!"}))
	for name, posts := range map[string][]topicPost{"whatsapp": w.topics.Posts(), "telegram": tw.topics.Posts()} {
		if len(posts) != 1 || posts[0].Text != "🤖 Boleh!" {
			t.Errorf("%s mirror = %+v", name, posts)
		}
	}
}

// telegram.bold_customer_messages is about the customer's words: an agent post
// is not one, whatever the setting.
func TestAgentPostsAreNeverBoldedByTheCustomerSetting(t *testing.T) {
	state.State.Config.Telegram.BoldCustomerMessages = true
	t.Cleanup(func() { state.State.Config.Telegram.BoldCustomerMessages = false })

	w := newWorld(t)
	e := w.executor()
	e.Handle(context.Background(), sendEnv(t, w, "A1", Send{Kind: "reply", Text: "Boleh!", Signature: "Loki"}))
	e.Handle(context.Background(), sendEnv(t, w, "A2", Send{Kind: "note", Text: "slip ok"}))
	e.Handle(context.Background(), cardEnv(t, w, "A3", Send{CardID: "d1", Title: "Draft", Text: "Boleh", Buttons: &draftButtons}))
	for _, p := range w.topics.Posts() {
		if strings.Contains(strings.ReplaceAll(p.Text, "<b>Draft</b>", ""), "<b>") {
			t.Errorf("agent post carries customer bold: %q", p.Text)
		}
	}
	for _, c := range w.topics.Cards() {
		if strings.Contains(strings.ReplaceAll(c.Text, "<b>Draft</b>", ""), "<b>") {
			t.Errorf("agent card carries customer bold: %q", c.Text)
		}
	}
}
