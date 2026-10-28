package lzcustomer_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	. "watgbridge/lzcustomer"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
)

const wantLink = "https://auth.lazada.com/oauth/authorize?response_type=code&force_auth=true&redirect_uri=https%3A%2F%2Fclarusmolecular.com%2F&client_id=" + testKey

func TestParseAuthCommand(t *testing.T) {
	cases := []struct {
		in   string
		args string
		ok   bool
	}{
		{"/lazada_auth", "", true},
		{"/lazada_auth ", "", true},
		{"/lazada_auth abc123", "abc123", true},
		{"/lazada_auth   abc123  ", "abc123", true},
		{"/lazada_auth\nabc123", "abc123", true},
		{"/lazada_auth@clarus_hub_bot", "", true},
		{"/lazada_auth@clarus_hub_bot abc", "abc", true},
		{"/lazada_authx abc", "", false},
		{"/lazada_auth_more", "", false},
		{"lazada_auth abc", "", false},
		{" /lazada_auth abc", "", false},
		{"hello /lazada_auth", "", false},
		{"/ai_status", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		args, ok := ParseAuthCommand(c.in)
		if args != c.args || ok != c.ok {
			t.Errorf("ParseAuthCommand(%q) = (%q, %v), want (%q, %v)", c.in, args, ok, c.args, c.ok)
		}
	}
}

func stubWorld(t *testing.T, mod ...func(*worldOpts)) (*world, *stubAuth) {
	t.Helper()
	var st *stubAuth
	mods := append([]func(*worldOpts){func(o *worldOpts) {
		st = &stubAuth{}
		o.auth = st
	}}, mod...)
	w := newWorld(t, mods...)
	st.j = w.j
	return w, st
}

func TestOnlyTheOwnerMayAuthorize(t *testing.T) {
	w, st := stubWorld(t)
	for _, args := range []string{"", "some-code"} {
		w.svc.HandleAuth(context.Background(), AuthRequest{FromID: 12345, Thread: 1300, MsgID: 77, Args: args})
	}
	want := []string{"post:1300:text:Only the owner can authorize Lazada.", "post:1300:text:Only the owner can authorize Lazada."}
	if got := w.j.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("journal = %q", got)
	}
	if len(st.codes) != 0 {
		t.Error("a code from a non-owner was exchanged")
	}
	if len(w.topics.deleted) != 0 {
		t.Error("a non-owner's message was deleted")
	}
}

func TestNoArgumentGivesTheAuthorizationLink(t *testing.T) {
	w, st := stubWorld(t)
	w.svc.HandleAuth(context.Background(), AuthRequest{FromID: testOwner, Thread: 0, MsgID: 77})
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].Thread != 0 {
		t.Fatalf("posts = %+v", posts)
	}
	text := posts[0].Text
	if !strings.Contains(text, wantLink) || !strings.Contains(text, "copy the code= value from the address bar and send /lazada_auth <code> within 30 minutes") {
		t.Errorf("reply = %q", text)
	}
	if len(st.codes) != 0 || len(w.topics.deleted) != 0 {
		t.Error("asking for the link exchanged or deleted something")
	}

	// A different callback URL is encoded into the link.
	w2, _ := stubWorld(t, func(o *worldOpts) { o.callbackURL = "https://example.com/cb?x=1&y=2" })
	w2.svc.HandleAuth(context.Background(), AuthRequest{FromID: testOwner})
	if got := w2.topics.Posts()[0].Text; !strings.Contains(got, "redirect_uri=https%3A%2F%2Fexample.com%2Fcb%3Fx%3D1%26y%3D2&client_id="+testKey) {
		t.Errorf("reply = %q", got)
	}
}

// The command message holds the code, so it is deleted before the exchange. The
// reply carries the seller id and dates in the bridge's time zone, and a full
// fetch is started.
func TestGoodCodeDeletesTheMessageThenAuthorizesRepliesAndFetches(t *testing.T) {
	kl, err := time.LoadLocation("Asia/Kuala_Lumpur")
	if err != nil {
		t.Skip("no tz database")
	}
	w, st := stubWorld(t, func(o *worldOpts) { o.loc = kl })
	st.result = Authorized{
		SellerID: "100094063", Country: "MY", Account: "shop@example.com",
		AccessExpiresAt:  time.Date(2026, 11, 4, 20, 0, 0, 0, time.UTC),  // 05 Nov, 04:00 in KL
		RefreshExpiresAt: time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC), // 28 Nov in KL
	}
	w.svc.HandleAuth(context.Background(), AuthRequest{FromID: testOwner, Thread: 1300, MsgID: 77, Args: "THE-CODE"})

	want := []string{
		"delete:77",
		"authorize",
		"post:1300:text:Lazada authorized: seller 100094063 (MY), account shop@example.com. Token valid until 2026-11-05; re-authorize by 2026-11-28. Put seller_id 100094063 in the agent config (markets.my.lazada.sellerId).",
	}
	if got := w.j.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal =\n%q\nwant\n%q", got, want)
	}
	if !reflect.DeepEqual(st.codes, []string{"THE-CODE"}) {
		t.Errorf("codes = %q", st.codes)
	}
	if !w.svc.PendingAll() {
		t.Error("no full fetch was started")
	}
}

// A message that cannot be deleted (no rights) does not stop the exchange.
func TestUndeletableCommandMessageStillAuthorizes(t *testing.T) {
	w, st := stubWorld(t)
	w.topics.deleteErr = fmt.Errorf("message can't be deleted")
	st.result = Authorized{SellerID: "1", Country: "MY"}
	w.svc.HandleAuth(context.Background(), AuthRequest{FromID: testOwner, MsgID: 5, Args: "c"})
	got := w.j.all()
	if len(got) != 3 || got[0] != "delete:5" || got[1] != "authorize" || !strings.Contains(got[2], "Lazada authorized: seller 1 (MY)") {
		t.Errorf("journal = %q", got)
	}
}

// Failures are worded plainly, give the link again, and never repeat the code,
// in the topic or in the logs.
func TestFailedCodesAreAnsweredPlainlyAndNeverEchoed(t *testing.T) {
	const code = "SECRET-CODE-123"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"auth", fmt.Errorf("x: %w", ErrAuth), "That code was not accepted (expired or already used). Get a new one: " + wantLink},
		{"rate limit", ErrRateLimited, "Lazada is limiting requests right now. Wait a minute and send the code again."},
		{"api error", &APIError{Code: "IllegalCode", Message: "boom " + code}, "Lazada refused that code (IllegalCode). Get a new one: " + wantLink},
		{"network", fmt.Errorf("dial tcp: connection refused for %s", code), "Could not reach Lazada to check that code. Try again; if it keeps failing, get a new code: " + wantLink},
	}
	for _, c := range cases {
		w, st := stubWorld(t)
		st.err = c.err
		w.svc.HandleAuth(context.Background(), AuthRequest{FromID: testOwner, Thread: 1300, MsgID: 9, Args: code})
		posts := w.topics.Posts()
		if len(posts) != 1 || posts[0].Text != c.want {
			t.Errorf("%s: reply = %+v, want %q", c.name, posts, c.want)
		}
		if !reflect.DeepEqual(w.topics.deleted, []int64{9}) {
			t.Errorf("%s: the command message was not deleted first", c.name)
		}
		if w.svc.PendingAll() {
			t.Errorf("%s: a failed code started a fetch", c.name)
		}
	}
}

// With the real token source and the fake Lazada: a code Lazada does not know.
func TestUnknownCodeAgainstLazadaIsRefusedWithoutEchoingIt(t *testing.T) {
	const code = "NOT-A-REAL-CODE"
	w := newWorld(t, func(o *worldOpts) { o.unauth = true })
	w.svc.HandleAuth(context.Background(), AuthRequest{FromID: testOwner, Thread: 1300, MsgID: 9, Args: code})
	posts := w.topics.Posts()
	if len(posts) != 1 || !strings.HasPrefix(posts[0].Text, "That code was not accepted") {
		t.Fatalf("posts = %+v", posts)
	}
	for _, p := range posts {
		if strings.Contains(p.Text, code) {
			t.Error("the code was echoed")
		}
	}
	for _, e := range w.logs.All() {
		if strings.Contains(fmt.Sprint(e.Message, e.Context), code) {
			t.Errorf("the code is in a log line: %v", e)
		}
	}

	// And a good one works end to end.
	w.lz.AllowCode("GOOD")
	w.svc.HandleAuth(context.Background(), AuthRequest{FromID: testOwner, Thread: 1300, MsgID: 10, Args: "GOOD"})
	posts = w.topics.Posts()
	if last := posts[len(posts)-1].Text; !strings.HasPrefix(last, "Lazada authorized: seller 100094063 (") {
		t.Errorf("reply = %q", last)
	}
	if st, ok := w.tokens.Status(); !ok || st.SellerID != "100094063" {
		t.Errorf("token status = %+v", st)
	}
}

// The refresh token's expiry is announced in General from 14 days out, once a
// day.
func TestExpiryWarningFiresOncePerDayWithinFourteenDays(t *testing.T) {
	w, st := stubWorld(t)
	st.hasState = true
	st.status = Authorized{RefreshExpiresAt: w.clock.Now().Add(20 * 24 * time.Hour)}
	ctx := context.Background()

	w.svc.Housekeeping(ctx)
	if len(w.topics.Posts()) != 0 {
		t.Fatal("warned 20 days out")
	}

	st.status.RefreshExpiresAt = w.clock.Now().Add(10 * 24 * time.Hour) // 2026-11-07 10:00 UTC
	w.svc.Housekeeping(ctx)
	w.svc.Housekeeping(ctx)
	w.clock.Advance(3 * time.Hour) // still the same day
	w.svc.Housekeeping(ctx)
	if n := len(w.topics.Posts()); n != 1 {
		t.Fatalf("%d warnings on one day, want 1", n)
	}
	p := w.topics.Posts()[0]
	want := "Lazada authorization expires on 2026-11-07. Re-authorize: " + wantLink + ", then /lazada_auth <code>."
	if p.Thread != 0 || p.Text != want {
		t.Errorf("warning = thread %d %q, want %q", p.Thread, p.Text, want)
	}

	w.clock.Advance(24 * time.Hour)
	w.svc.Housekeeping(ctx)
	if n := len(w.topics.Posts()); n != 2 {
		t.Errorf("%d warnings after the next day, want 2", n)
	}
}

// Nothing stored, nothing to warn about.
func TestNoExpiryWarningWithoutAnAuthorization(t *testing.T) {
	w, _ := stubWorld(t)
	w.svc.Housekeeping(context.Background())
	if len(w.topics.Posts()) != 0 {
		t.Errorf("posts = %+v", w.topics.Posts())
	}
}

// The day is the bridge's own, not UTC: a warning at 23:00 UTC and one at
// 01:00 UTC the next day are on the same Kuala Lumpur day... and different UTC days.
func TestExpiryWarningUsesTheBridgesTimeZone(t *testing.T) {
	kl, err := time.LoadLocation("Asia/Kuala_Lumpur")
	if err != nil {
		t.Skip("no tz database")
	}
	w, st := stubWorld(t, func(o *worldOpts) { o.loc = kl })
	st.hasState = true
	st.status = Authorized{RefreshExpiresAt: w.clock.Now().Add(5 * 24 * time.Hour)}
	w.clock.Advance(13 * time.Hour) // 23:00 UTC = 07:00 next day in KL
	w.svc.Housekeeping(context.Background())
	w.clock.Advance(2 * time.Hour) // 01:00 UTC next day = 09:00 in KL, same local day
	w.svc.Housekeeping(context.Background())
	if n := len(w.topics.Posts()); n != 1 {
		t.Errorf("%d warnings on one local day, want 1", n)
	}
}

func authMsg(t *testing.T, text string, from int64, thread int64) *ext.Context {
	t.Helper()
	bot, err := gotgbot.NewBot("1:TEST", &gotgbot.BotOpts{DisableTokenCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	m := &gotgbot.Message{
		MessageId: 77, MessageThreadId: thread, IsTopicMessage: thread != 0, Text: text,
		From: &gotgbot.User{Id: from, FirstName: "Ilya"},
		Chat: gotgbot.Chat{Id: -1001, Type: "supergroup"},
	}
	return ext.NewContext(bot, &gotgbot.Update{UpdateId: 1, Message: m}, nil)
}

// The hook in the bridge's message handler: it takes /lazada_auth from any
// topic or General and hands it to the channel; anything else is not its.
func TestMessageHookTakesOnlyTheCommandAndPassesTheSender(t *testing.T) {
	w, st := stubWorld(t)
	st.result = Authorized{SellerID: "100094063", Country: "MY"}
	SetCurrent(w.svc)
	t.Cleanup(func() { SetCurrent(nil) })

	if HandleAuthCommand(nil, authMsg(t, "hello there", testOwner, 1300)) {
		t.Error("an ordinary message was taken")
	}
	if HandleAuthCommand(nil, authMsg(t, "/ai_status", testOwner, 1300)) {
		t.Error("an /ai_ command was taken")
	}
	if len(w.j.all()) != 0 {
		t.Fatalf("journal = %q", w.j.all())
	}

	// The owner, in a customer's topic.
	if !HandleAuthCommand(nil, authMsg(t, "/lazada_auth THE-CODE", testOwner, 1300)) {
		t.Fatal("the command was not taken")
	}
	got := w.j.all()
	if len(got) != 3 || got[0] != "delete:77" || got[1] != "authorize" || !strings.HasPrefix(got[2], "post:1300:text:Lazada authorized") {
		t.Errorf("journal = %q", got)
	}

	// Somebody else, in General.
	w.j.lines = nil
	if !HandleAuthCommand(nil, authMsg(t, "/lazada_auth THE-CODE", 999, 0)) {
		t.Fatal("the command was not taken")
	}
	if got := w.j.all(); !reflect.DeepEqual(got, []string{"post:0:text:Only the owner can authorize Lazada."}) {
		t.Errorf("journal = %q", got)
	}
	if len(st.codes) != 1 {
		t.Errorf("codes exchanged = %q, want only the owner's", st.codes)
	}
}
