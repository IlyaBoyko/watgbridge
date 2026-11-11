package lzcustomer_test

import (
	"fmt"
	"context"
	"strings"
	"testing"
	"time"

	. "watgbridge/lzcustomer"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const agentCode = "0_100000_SECRETcodeFromTheAgent"

func (w *world) postsText() []string { return w.topics.texts(0) }

func (w *world) waitGeneral(n int) []string {
	w.t.Helper()
	waitFor(w.t, "the post in General", func() bool { return len(w.postsText()) >= n })
	return w.postsText()
}

func (w *world) storedSeller() string {
	w.t.Helper()
	st, ok := w.tokens.Status()
	if !ok {
		w.t.Fatal("nothing stored")
	}
	return st.SellerID
}

func TestAgentCodeWithNoStoredTokensIsStoredReportedAndFetches(t *testing.T) {
	w := newWorld(t, func(o *worldOpts) { o.unauth = true })
	w.lz.AllowCode(agentCode)
	w.svc.AuthCode(agentCode)

	got := w.waitGeneral(1)
	if !strings.HasPrefix(got[0], "Lazada authorized: seller 100094063 (my)") {
		t.Errorf("post = %q", got[0])
	}
	waitFor(t, "the full fetch", w.svc.PendingAll)
	if w.storedSeller() != "100094063" {
		t.Error("seller not stored")
	}
	if len(w.topics.deleted) != 0 {
		t.Error("something was deleted: there is no message")
	}
}

func TestAgentCodeForTheSameSellerReplacesTheTokens(t *testing.T) {
	w := newWorld(t)
	before, _ := w.tokens.Token(context.Background())
	w.lz.AllowCode(agentCode)
	w.svc.AuthCode(agentCode)

	got := w.waitGeneral(1)
	if !strings.HasPrefix(got[0], "Lazada authorized: seller 100094063") {
		t.Errorf("post = %q", got[0])
	}
	if after, _ := w.tokens.Token(context.Background()); after == before {
		t.Error("tokens were not replaced")
	}
}

func TestAgentCodeForAnotherSellerChangesNothingAndWarns(t *testing.T) {
	w := newWorld(t)
	before, _ := w.tokens.Token(context.Background())
	stBefore, _ := w.tokens.Status()
	w.lz.SetSellerID("555")
	w.lz.AllowCode(agentCode)
	w.svc.AuthCode(agentCode)

	got := w.waitGeneral(1)
	want := "⚠️ A Lazada authorization arrived for seller 555, but this Hub is connected to seller 100094063. " +
		"It was ignored and the current authorization stays. To switch shops on purpose, the owner sends /lazada_auth <code>."
	if got[0] != want {
		t.Errorf("post = %q\nwant   %q", got[0], want)
	}
	after, _ := w.tokens.Token(context.Background())
	stAfter, _ := w.tokens.Status()
	if after != before || stAfter != stBefore {
		t.Error("the stored authorization changed")
	}
	if w.svc.PendingAll() {
		t.Error("a fetch was queued")
	}
}

func TestOwnerCodeForAnotherSellerIsAccepted(t *testing.T) {
	w := newWorld(t)
	w.lz.SetSellerID("555")
	w.lz.AllowCode(agentCode)
	w.svc.HandleAuth(context.Background(), AuthRequest{FromID: testOwner, MsgID: 5, Args: agentCode})

	if got := w.postsText(); len(got) != 1 || !strings.HasPrefix(got[0], "Lazada authorized: seller 555") {
		t.Errorf("posts = %q", got)
	}
	if w.storedSeller() != "555" {
		t.Errorf("seller = %q", w.storedSeller())
	}
}

func TestRefusedAgentCodeGetsTheFailureWording(t *testing.T) {
	w := newWorld(t)
	w.svc.AuthCode("never-issued")
	got := w.waitGeneral(1)
	if !strings.Contains(got[0], "Get a new one: "+wantLink) {
		t.Errorf("post = %q", got[0])
	}
}

func TestAgentCodeAppearsInNoPostAndNoLog(t *testing.T) {
	w := newWorld(t)
	w.lz.AllowCode(agentCode)
	w.lz.SetSellerID("555") // the warning path
	w.svc.AuthCode(agentCode)
	w.lz.AllowCode(agentCode + "2")
	w.svc.AuthCode(agentCode + "2")
	w.svc.AuthCode(agentCode + "3") // refused
	w.waitGeneral(3)

	for _, p := range w.topics.Posts() {
		if strings.Contains(p.Text, "SECRETcode") {
			t.Errorf("post carries the code: %q", p.Text)
		}
	}
	for _, e := range w.logs.All() {
		if strings.Contains(e.Message, "SECRETcode") || strings.Contains(fmt.Sprint(e.ContextMap()), "SECRETcode") {
			t.Errorf("log carries the code: %+v", e)
		}
	}
}

func TestRefreshLogCarriesLifetimesAndNoToken(t *testing.T) {
	e := newTokenEnv(t)
	core, logs := observer.New(zap.DebugLevel)
	e.fake.SetLifetimes(24*time.Hour, 24*time.Hour)
	e.fake.AllowCode("c")
	src := NewTokenSource(e.db, NewAuth(e.fake.HTTPClient(), e.fake.URL(), testKey, testSecret, e.clock.Now), e.clock.Now, zap.New(core))
	if _, err := src.Authorize(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	tok, _ := src.Token(context.Background())
	e.clock.Advance(23*time.Hour + 30*time.Minute) // inside the window
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	issued := logs.FilterMessage("lazada: token issued").All()
	refreshed := logs.FilterMessage("lazada: token refreshed").All()
	if len(issued) != 1 || len(refreshed) != 1 {
		t.Fatalf("issued %d refreshed %d", len(issued), len(refreshed))
	}
	m := refreshed[0].ContextMap()
	if m["expires_in_s"] != int64(86400) || m["refresh_expires_in_s"] != int64(86400) || m["new_refresh_token"] != true {
		t.Errorf("fields = %v", m)
	}
	for _, k := range []string{"access_expires_at", "refresh_expires_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	for _, en := range logs.All() {
		s := en.Message + fmt.Sprint(en.ContextMap())
		if strings.Contains(s, tok) || strings.Contains(s, "rt-") || strings.Contains(s, "at-") {
			t.Errorf("log carries a token: %+v", en)
		}
	}
}
