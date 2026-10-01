package agentlink

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testConv = "wa:60123456789@s.whatsapp.net"

func cardEnv(t *testing.T, w *world, id string, p Send) Envelope {
	t.Helper()
	p.Kind = "card"
	return sendEnv(t, w, id, p)
}

func editEnv(t *testing.T, w *world, id string, p EditCard) Envelope {
	t.Helper()
	if p.Conversation == "" {
		p.Conversation = testConv
	}
	if p.ExpiresAt == "" {
		p.ExpiresAt = FormatTS(w.clock.Now().Add(10 * time.Minute))
	}
	env, err := NewEnvelope(id, TypeEditCard, w.clock.Now(), p)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

var draftButtons = Buttons{
	{{ID: "send_now", Label: "Send now"}, {ID: "edit", Label: "Edit"}},
	{{ID: "cancel", Label: "Cancel"}},
}

func cardRowByID(t *testing.T, w *world, cardID string) CardRow {
	t.Helper()
	row, ok, err := NewCardStore(w.db, w.clock).ByCardID(cardID)
	if err != nil || !ok {
		t.Fatalf("card %q is not recorded (%v)", cardID, err)
	}
	return row
}

// A1: a card goes into the conversation's topic as-is, keeps the row layout,
// and is recorded. Nothing reaches the customer.
func TestSendCardPostsInTopicWithKeyboardAndRecordsRow(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	res := e.Handle(context.Background(), cardEnv(t, w, "CARD1", Send{
		Text: "Draft, sends at 17:42:\nBoleh! <b>2-3</b> hari", CardID: "draft_01JB00000000000000000000AA", Buttons: &draftButtons,
	}))
	if !res.OK || res.Error != "" || res.CommandID != "CARD1" || res.HubMsgID != "" {
		t.Fatalf("result = %+v", res)
	}

	cards := w.topics.Cards()
	if len(cards) != 1 || cards[0].Thread != 1234 || cards[0].Text != "Draft, sends at 17:42:\nBoleh! <b>2-3</b> hari" {
		t.Fatalf("cards = %+v", cards)
	}
	row := cardRowByID(t, w, "draft_01JB00000000000000000000AA")
	want := Keyboard{
		{{Label: "Send now", CallbackData: EncodeCallbackData(row.ID, "send_now")}, {Label: "Edit", CallbackData: EncodeCallbackData(row.ID, "edit")}},
		{{Label: "Cancel", CallbackData: EncodeCallbackData(row.ID, "cancel")}},
	}
	if len(cards[0].KB) != 2 || len(cards[0].KB[0]) != 2 || len(cards[0].KB[1]) != 1 {
		t.Fatalf("keyboard layout = %+v", cards[0].KB)
	}
	for i := range want {
		for j := range want[i] {
			if cards[0].KB[i][j] != want[i][j] {
				t.Errorf("button [%d][%d] = %+v, want %+v", i, j, cards[0].KB[i][j], want[i][j])
			}
		}
	}

	if row.Conversation != testConv || row.TgChatID != -1001 || row.TgThreadID != 1234 || row.TgMsgID != 9001 || row.CreatedAt.IsZero() {
		t.Errorf("row = %+v", row)
	}
	if len(w.wa.Calls()) != 0 || len(w.topics.Posts()) != 0 || len(w.bridge.Pairs()) != 0 {
		t.Error("a card must not reach WhatsApp or be mirrored")
	}
}

func TestSendCardWithoutButtonsHasNoKeyboard(t *testing.T) {
	w := newWorld(t)
	res := w.executor().Handle(context.Background(), cardEnv(t, w, "CARD1", Send{Text: "FYI", CardID: "note_1"}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	if cards := w.topics.Cards(); len(cards) != 1 || len(cards[0].KB) != 0 {
		t.Errorf("cards = %+v", cards)
	}
}

// A1: the ways a card send is refused.
func TestSendCardRefusals(t *testing.T) {
	w := newWorld(t)
	w.bridge.threads["120363025246125486@g.us"] = 77
	e := w.executor()
	ctx := context.Background()
	img := []Media{{Kind: "image", Mime: "image/png", Size: 70, Data: pngB64}}

	for name, c := range map[string]struct {
		p    Send
		want string
	}{
		"media present":      {Send{Text: "x", CardID: "c1", Media: img}, ErrInvalid},
		"missing card id":    {Send{Text: "x"}, ErrInvalid},
		"missing text":       {Send{CardID: "c2"}, ErrInvalid},
		"blank text":         {Send{Text: "  \n", CardID: "c3"}, ErrInvalid},
		"no topic":           {Send{Text: "x", CardID: "c4", Conversation: "wa:60111111111@s.whatsapp.net"}, ErrUnknownConversation},
		"a group":            {Send{Text: "x", CardID: "c5", Conversation: "wa:120363025246125486@g.us"}, ErrUnknownConversation},
		"not a wa chat":      {Send{Text: "x", CardID: "c6", Conversation: "tg:5550001111"}, ErrUnknownConversation},
		"button id too long": {Send{Text: "x", CardID: "c7", Buttons: &Buttons{{{ID: strings.Repeat("b", 33), Label: "B"}}}}, ErrInvalid},
	} {
		if res := e.Handle(ctx, cardEnv(t, w, "CMD-"+name, c.p)); res.OK || res.Error != c.want {
			t.Errorf("%s: result = %+v, want %s", name, res, c.want)
		}
	}
	if len(w.topics.Cards()) != 0 {
		t.Error("a refused card was posted")
	}
	var n int64
	w.db.Model(&CardRow{}).Count(&n)
	if n != 0 {
		t.Errorf("%d card rows after refusals, want 0", n)
	}

	// A reused card id is an Agent bug, not a retry: a second command with the same card id is refused.
	if res := e.Handle(ctx, cardEnv(t, w, "FIRST", Send{Text: "one", CardID: "dup"})); !res.OK {
		t.Fatalf("first: %+v", res)
	}
	if res := e.Handle(ctx, cardEnv(t, w, "SECOND", Send{Text: "two", CardID: "dup"})); res.OK || res.Error != ErrInvalid {
		t.Errorf("reused card id: %+v", res)
	}
	if len(w.topics.Cards()) != 1 {
		t.Errorf("%d cards posted, want 1", len(w.topics.Cards()))
	}
	// ... while a retry of the same command id still returns the first result.
	if res := e.Handle(ctx, cardEnv(t, w, "FIRST", Send{Text: "one", CardID: "dup"})); !res.OK {
		t.Errorf("retry of the first command: %+v", res)
	}
}

func TestSendCardFailedPostFreesTheCardID(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	w.topics.fail = errors.New("telegram down")
	if res := e.Handle(context.Background(), cardEnv(t, w, "C1", Send{Text: "x", CardID: "draft_9"})); res.OK || res.Error != ErrInternal {
		t.Fatalf("result = %+v", res)
	}
	w.topics.fail = nil
	if res := e.Handle(context.Background(), cardEnv(t, w, "C2", Send{Text: "x", CardID: "draft_9"})); !res.OK {
		t.Errorf("the card id stayed burned after a failed post: %+v", res)
	}
}

// A2: the callback data round-trips and the longest legal button id fits.
func TestCallbackDataRoundTripAndFitsTelegramLimit(t *testing.T) {
	for _, c := range []struct {
		row    uint
		button string
	}{
		{1, "send_now"}, {42, "a"}, {1<<32 - 1, "cancel"}, {7, "with:colons:inside"},
	} {
		data := EncodeCallbackData(c.row, c.button)
		row, button, ok := DecodeCallbackData(data)
		if !ok || row != c.row || button != c.button {
			t.Errorf("%q decoded to (%d, %q, %v)", data, row, button, ok)
		}
		if !IsCardCallback(data) {
			t.Errorf("%q is not recognised as a card callback", data)
		}
	}

	longest := strings.Repeat("x", 32)
	// The biggest row id there can be: a card id could never have fit.
	data := EncodeCallbackData(^uint(0), longest)
	if len(data) > 64 {
		t.Errorf("%q is %d bytes, Telegram allows 64", data, len(data))
	}
	kb, err := BuildKeyboard(^uint(0), Buttons{{{ID: longest, Label: "L"}}})
	if err != nil || kb[0][0].CallbackData != data {
		t.Errorf("BuildKeyboard = %+v, %v", kb, err)
	}
	// The same 32 holds in bytes: the protocol limit and Telegram's are both bytes.
	row, button, ok := DecodeCallbackData(data)
	if !ok || row != ^uint(0) || button != longest {
		t.Errorf("the longest data decoded to (%d, %q, %v)", row, button, ok)
	}

	for _, bad := range []string{"", "revoke_1_x", "ag:", "ag:5", "ag:5:", "ag:x:id", "ag:0:id", "ag::id", "ag:-1:id"} {
		if _, _, ok := DecodeCallbackData(bad); ok {
			t.Errorf("%q decoded", bad)
		}
	}
	if IsCardCallback("revoke_3EB0_x") {
		t.Error("a revoke button was taken for a card button")
	}
}

/* ------------------------------------------------------------- callbacks -- */

// pressable posts a card through the hub's own executor and returns the
// callback data of its first button.
func pressable(t *testing.T, w *world, h *Hub, cardID string) string {
	t.Helper()
	res := h.exec.Handle(context.Background(), cardEnv(t, w, "POST-"+cardID, Send{Text: "Draft", CardID: cardID, Buttons: &draftButtons}))
	if !res.OK {
		t.Fatalf("could not post the card: %+v", res)
	}
	cards := w.topics.Cards()
	return cards[len(cards)-1].KB[0][0].CallbackData
}

var ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// A3: an authorized press is answered with no text and becomes a callback.
func TestCallbackFromAuthorizedUserEmitsEvent(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")
	data := pressable(t, w, h, "draft_01JB00000000000000000000AA")

	answer := h.HandleCallback(CallbackInput{Data: data, Authorized: true, UserID: 704338780, UserName: "Ilya"})
	if answer != "" {
		t.Fatalf("answer = %q, want no text", answer)
	}
	evs := queuedEvents(t, h)
	if len(evs) != 1 || evs[0].Type != TypeCallback {
		t.Fatalf("queued: %+v", evs)
	}
	if !ulidRe.MatchString(evs[0].ID) {
		t.Errorf("callback id %q is not a ULID", evs[0].ID)
	}
	var got Callback
	if err := json.Unmarshal(evs[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	want := Callback{
		Conversation: testConv, CardID: "draft_01JB00000000000000000000AA", ButtonID: "send_now",
		Author: TgAuthor{TgUserID: "704338780", Name: "Ilya"},
	}
	if got != want {
		t.Errorf("callback = %+v\nwant       %+v", got, want)
	}

	// Pressing again is a new press, with a new id.
	h.HandleCallback(CallbackInput{Data: data, Authorized: true, UserID: 704338780, UserName: "Ilya"})
	if evs := queuedEvents(t, h); len(evs) != 2 || evs[0].ID == evs[1].ID {
		t.Errorf("two presses gave %+v", evs)
	}
}

func TestCallbackFromUnauthorizedUserIsRefused(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")
	data := pressable(t, w, h, "draft_1")
	if answer := h.HandleCallback(CallbackInput{Data: data, Authorized: false, UserID: 5, UserName: "Stranger"}); answer != CallbackNotAllowed {
		t.Errorf("answer = %q, want %q", answer, CallbackNotAllowed)
	}
	if n := len(queuedEvents(t, h)); n != 0 {
		t.Errorf("%d events queued for an unauthorized press", n)
	}
}

func TestCallbackOnUnknownOrPrunedCardIsExpired(t *testing.T) {
	w := newWorld(t)
	h := w.hub("ws://unused")
	data := pressable(t, w, h, "draft_1")

	for name, d := range map[string]string{
		"unknown row": EncodeCallbackData(9999, "send_now"),
		"garbage":     "ag:what",
	} {
		if answer := h.HandleCallback(CallbackInput{Data: d, Authorized: true, UserID: 1, UserName: "I"}); answer != CallbackExpired {
			t.Errorf("%s: answer = %q, want %q", name, answer, CallbackExpired)
		}
	}

	w.clock.Advance(31 * 24 * time.Hour)
	h.prune()
	if answer := h.HandleCallback(CallbackInput{Data: data, Authorized: true, UserID: 1, UserName: "I"}); answer != CallbackExpired {
		t.Errorf("pruned card: answer = %q, want %q", answer, CallbackExpired)
	}
	if n := len(queuedEvents(t, h)); n != 0 {
		t.Errorf("%d events queued for expired cards", n)
	}
}

func TestCallbackWithLinkDisabledEmitsNothing(t *testing.T) {
	w := newWorld(t)
	h, err := NewHub(Config{Enabled: false}, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	in := CallbackInput{Data: EncodeCallbackData(1, "send_now"), Authorized: true, UserID: 1, UserName: "I"}
	if answer := h.HandleCallback(in); answer != CallbackNotConnected {
		t.Errorf("answer = %q, want %q", answer, CallbackNotConnected)
	}
	var none *Hub
	if answer := none.HandleCallback(in); answer != CallbackNotConnected {
		t.Errorf("nil hub: answer = %q, want %q", answer, CallbackNotConnected)
	}
	// Even a stranger learns nothing about the link.
	in.Authorized = false
	if answer := h.HandleCallback(in); answer != CallbackNotAllowed {
		t.Errorf("unauthorized with link off: answer = %q", answer)
	}
	if w.db.Migrator().HasTable(&OutboxRow{}) {
		t.Error("a disabled link wrote a table")
	}
}

/* ------------------------------------------------------------- edit_card -- */

// A4: edit_card replaces text and keyboard, and an empty list removes it.
func TestEditCardReplacesTextAndKeyboard(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	ctx := context.Background()
	e.Handle(ctx, cardEnv(t, w, "C1", Send{Text: "Draft", CardID: "draft_1", Buttons: &draftButtons}))
	row := cardRowByID(t, w, "draft_1")

	// New buttons replace the old ones.
	res := e.Handle(ctx, editEnv(t, w, "E1", EditCard{CardID: "draft_1", Text: "Draft v2", Buttons: &Buttons{{{ID: "ok", Label: "OK"}}}}))
	if !res.OK || res.Error != "" || res.CommandID != "E1" {
		t.Fatalf("result = %+v", res)
	}
	edits := w.topics.Edits()
	if len(edits) != 1 || edits[0].ChatID != -1001 || edits[0].MsgID != 9001 || edits[0].Text != "Draft v2" {
		t.Fatalf("edits = %+v", edits)
	}
	if len(edits[0].KB) != 1 || len(edits[0].KB[0]) != 1 || edits[0].KB[0][0] != (KeyboardButton{Label: "OK", CallbackData: EncodeCallbackData(row.ID, "ok")}) {
		t.Errorf("keyboard = %+v", edits[0].KB)
	}

	// No buttons given: the keyboard stays as it was last set.
	e.Handle(ctx, editEnv(t, w, "E2", EditCard{CardID: "draft_1", Text: "Draft v3"}))
	edits = w.topics.Edits()
	if len(edits) != 2 || edits[1].Text != "Draft v3" || len(edits[1].KB) != 1 || edits[1].KB[0][0].Label != "OK" {
		t.Errorf("an edit without buttons must keep them: %+v", edits)
	}

	// An empty list removes the keyboard, and it stays removed.
	e.Handle(ctx, editEnv(t, w, "E3", EditCard{CardID: "draft_1", Text: "Sent by Ilya.", Buttons: &Buttons{}}))
	e.Handle(ctx, editEnv(t, w, "E4", EditCard{CardID: "draft_1", Text: "Sent by Ilya (late edit)."}))
	edits = w.topics.Edits()
	if len(edits) != 4 || len(edits[2].KB) != 0 || len(edits[3].KB) != 0 {
		t.Errorf("buttons: [] must remove the keyboard for good: %+v", edits)
	}
}

func TestEditCardMessageNotModifiedCountsAsOK(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	e.Handle(context.Background(), cardEnv(t, w, "C1", Send{Text: "Draft", CardID: "draft_1", Buttons: &draftButtons}))
	w.topics.editFail = errors.New("telegram: Bad Request: message is not modified: specified new message content and reply markup are exactly the same")
	res := e.Handle(context.Background(), editEnv(t, w, "E1", EditCard{CardID: "draft_1", Text: "Draft", Buttons: &Buttons{}}))
	if !res.OK || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}

	// Any other Telegram failure is an internal error.
	w.topics.editFail = errors.New("Bad Request: message to edit not found")
	if res := e.Handle(context.Background(), editEnv(t, w, "E2", EditCard{CardID: "draft_1", Text: "x"})); res.OK || res.Error != ErrInternal {
		t.Errorf("result = %+v", res)
	}
}

func TestEditCardUnknownCardAndExpired(t *testing.T) {
	w := newWorld(t)
	e := w.executor()
	ctx := context.Background()
	if res := e.Handle(ctx, editEnv(t, w, "E1", EditCard{CardID: "nope", Text: "x"})); res.OK || res.Error != ErrUnknownCard {
		t.Errorf("unknown card: %+v", res)
	}

	e.Handle(ctx, cardEnv(t, w, "C1", Send{Text: "Draft", CardID: "draft_1"}))
	// The same card id under another conversation is not that card.
	if res := e.Handle(ctx, editEnv(t, w, "E2", EditCard{CardID: "draft_1", Text: "x", Conversation: "wa:60198765432@s.whatsapp.net"})); res.OK || res.Error != ErrUnknownCard {
		t.Errorf("other conversation: %+v", res)
	}
	if res := e.Handle(ctx, editEnv(t, w, "E3", EditCard{CardID: "draft_1", Text: " "})); res.OK || res.Error != ErrInvalid {
		t.Errorf("blank text: %+v", res)
	}

	// An expired command does nothing.
	res := e.Handle(ctx, editEnv(t, w, "E4", EditCard{CardID: "draft_1", Text: "late", ExpiresAt: FormatTS(w.clock.Now().Add(-time.Second))}))
	if res.OK || res.Error != ErrExpired {
		t.Errorf("expired: %+v", res)
	}
	if n := len(w.topics.Edits()); n != 0 {
		t.Errorf("%d edits happened, want none", n)
	}

	// A card that was pruned is unknown too.
	w.clock.Advance(31 * 24 * time.Hour)
	if _, err := e.Cards.Prune(); err != nil {
		t.Fatal(err)
	}
	if res := e.Handle(ctx, editEnv(t, w, "E5", EditCard{CardID: "draft_1", Text: "x"})); res.Error != ErrUnknownCard {
		t.Errorf("pruned card: %+v", res)
	}
}

/* ----------------------------------------------------------------- prune -- */

// A5: cards older than 30 days are pruned, newer ones stay.
func TestCardsOlderThan30DaysArePruned(t *testing.T) {
	w := newWorld(t)
	if err := Migrate(w.db); err != nil {
		t.Fatal(err)
	}
	cards := NewCardStore(w.db, w.clock)
	if _, err := cards.Create(CardRow{CardID: "old", Conversation: testConv}); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(29 * 24 * time.Hour)
	if _, err := cards.Create(CardRow{CardID: "newer", Conversation: testConv}); err != nil {
		t.Fatal(err)
	}
	if n, err := cards.Prune(); err != nil || n != 0 {
		t.Fatalf("pruned %d (%v) at 29 days, want 0", n, err)
	}
	w.clock.Advance(2 * 24 * time.Hour) // old: 31 days, newer: 2 days
	if n, err := cards.Prune(); err != nil || n != 1 {
		t.Fatalf("pruned %d (%v) at 31 days, want 1", n, err)
	}
	if _, ok, _ := cards.ByCardID("old"); ok {
		t.Error("the old card survived the prune")
	}
	if _, ok, _ := cards.ByCardID("newer"); !ok {
		t.Error("the newer card was pruned")
	}
}

// A5: the hub's hourly prune covers cards too, and runs at startup.
func TestHubPrunesCardsAtStartupAndOnDemand(t *testing.T) {
	w := newWorld(t)
	if err := Migrate(w.db); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCardStore(w.db, w.clock).Create(CardRow{CardID: "old", Conversation: testConv}); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(31 * 24 * time.Hour)
	h := w.hub("ws://unused") // startup prune
	if _, ok, _ := h.cards.ByCardID("old"); ok {
		t.Error("startup did not prune the old card")
	}

	if _, err := h.cards.Create(CardRow{CardID: "second", Conversation: testConv}); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(31 * 24 * time.Hour)
	h.prune()
	if _, ok, _ := h.cards.ByCardID("second"); ok {
		t.Error("the periodic prune did not remove the old card")
	}
}
