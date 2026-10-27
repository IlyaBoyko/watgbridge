package lzcustomer_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"watgbridge/lzcustomer"
	"watgbridge/lzcustomer/lzfake"
)

const (
	testKey    = "123456"
	testSecret = "3412gyo124goi3124"
)

// fakeClock is shared by the client and the fake server so that timestamps,
// message times and token expiry all agree.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

type env struct {
	fake   *lzfake.Server
	clock  *fakeClock
	client *lzcustomer.Client
	token  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clock := newFakeClock()
	fake := lzfake.New(testKey, testSecret)
	t.Cleanup(fake.Close)
	fake.SetNow(clock.Now)
	token, _ := fake.IssueToken()
	return &env{
		fake: fake, clock: clock, token: token,
		client: lzcustomer.NewClient(fake.HTTPClient(), fake.URL(), testKey, testSecret, clock.Now, staticToken(token)),
	}
}

func TestSessionListParamsAndDecoding(t *testing.T) {
	e := newEnv(t)
	e.fake.AddSession("s1", "buyer-1", "Aisha")
	e.fake.AddBuyerMessage("s1", 1, lzfake.TextContent("hello"))
	e.fake.AddBuyerMessage("s1", 1, lzfake.TextContent("are you there"))

	e.clock.Advance(time.Minute)
	page, err := e.client.SessionList(context.Background(), 0, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || bool(page.HasMore) {
		t.Fatalf("page = %+v", page)
	}
	s := page.Sessions[0]
	if s.SessionID != "s1" || s.BuyerID != "buyer-1" || s.Title != "Aisha" || s.Summary != "are you there" ||
		s.UnreadCount != 2 || s.SiteID != "MY" || s.LastMessageTime == 0 || s.LastMessageID == "" {
		t.Errorf("session = %+v", s)
	}

	req := e.fake.Requests()[0]
	if req.Method != http.MethodGet || req.Path != "/im/session/list" {
		t.Errorf("request = %s %s", req.Method, req.Path)
	}
	// start_time defaulted to the client's clock, in ms.
	if got, want := req.Query.Get("start_time"), strconv.FormatInt(e.clock.Now().UnixMilli(), 10); got != want {
		t.Errorf("start_time = %s, want %s", got, want)
	}
	for k, want := range map[string]string{
		"page_size": "10", "app_key": testKey, "sign_method": "sha256", "access_token": e.token,
		"timestamp": strconv.FormatInt(e.clock.Now().UnixMilli(), 10),
	} {
		if got := req.Query.Get(k); got != want {
			t.Errorf("query %s = %q, want %q", k, got, want)
		}
	}
	if req.Query.Get("sign") == "" {
		t.Error("no sign parameter")
	}
	if _, present := req.Query["last_session_id"]; present {
		t.Error("an empty last_session_id was sent")
	}
}

func TestSessionListPagingAcrossTwoPages(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"a", "b", "c"} {
		e.fake.AddBuyerMessage(id, 1, lzfake.TextContent("hi "+id))
	}
	e.clock.Advance(time.Minute)
	ctx := context.Background()

	p1, err := e.client.SessionList(ctx, 0, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	// Newest activity first.
	if len(p1.Sessions) != 2 || p1.Sessions[0].SessionID != "c" || p1.Sessions[1].SessionID != "b" || !bool(p1.HasMore) {
		t.Fatalf("page 1 = %+v", p1)
	}
	if p1.LastSessionID != "b" || p1.NextStartTime != p1.Sessions[1].LastMessageTime {
		t.Errorf("page 1 cursors: %+v", p1)
	}

	p2, err := e.client.SessionList(ctx, int64(p1.NextStartTime), 2, string(p1.LastSessionID))
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Sessions) != 1 || p2.Sessions[0].SessionID != "a" || bool(p2.HasMore) {
		t.Fatalf("page 2 = %+v", p2)
	}
	last := e.fake.Requests()[1]
	if last.Query.Get("last_session_id") != "b" || last.Query.Get("start_time") != strconv.FormatInt(int64(p1.NextStartTime), 10) {
		t.Errorf("page 2 request carried %v", last.Query)
	}
}

func TestMessageListPagingAndContent(t *testing.T) {
	e := newEnv(t)
	e.fake.AddSession("s1", "buyer-1", "Aisha")
	var ids []string
	for _, txt := range []string{"one", "two", "three", "four", "five"} {
		ids = append(ids, e.fake.AddBuyerMessage("s1", 1, lzfake.TextContent(txt)))
	}
	seller := e.fake.AddSellerMessage("s1", 1, lzfake.TextContent("from seller center"), lzfake.SellerOpts{AutoReply: true})
	card := e.fake.AddBuyerMessage("s1", 10006, `{"itemId":"42","title":"BPC-157"}`)
	e.clock.Advance(time.Minute)
	ctx := context.Background()

	var got []lzcustomer.Message
	var start int64
	last := ""
	pages := 0
	for {
		p, err := e.client.MessageList(ctx, "s1", start, 3, last)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		got = append(got, p.Messages...)
		if !bool(p.HasMore) {
			break
		}
		start, last = int64(p.NextStartTime), string(p.LastMessageID)
		if pages > 5 {
			t.Fatal("paging does not terminate")
		}
	}
	if pages != 3 || len(got) != 7 {
		t.Fatalf("%d pages, %d messages", pages, len(got))
	}
	// Newest first, no repeats, nothing missing.
	want := []string{card, seller, ids[4], ids[3], ids[2], ids[1], ids[0]}
	for i, m := range got {
		if string(m.MessageID) != want[i] {
			t.Errorf("message %d = %s, want %s", i, m.MessageID, want[i])
		}
	}
	if got[0].TemplateID != 10006 || got[0].Content != `{"itemId":"42","title":"BPC-157"}` || got[0].FromAccountType != lzcustomer.AccountBuyer {
		t.Errorf("card message = %+v", got[0])
	}
	if got[1].FromAccountType != lzcustomer.AccountSeller || !bool(got[1].AutoReply) || got[1].Type != lzcustomer.TypeUser {
		t.Errorf("seller message = %+v", got[1])
	}
	if got[0].SendTime <= got[6].SendTime {
		t.Error("send_time does not descend")
	}

	r := e.fake.Requests()[1]
	if r.Query.Get("session_id") != "s1" || r.Query.Get("page_size") != "3" || r.Query.Get("last_message_id") == "" {
		t.Errorf("second page request: %v", r.Query)
	}
}

func TestSendTextIsAFormPost(t *testing.T) {
	e := newEnv(t)
	e.fake.AddBuyerMessage("s1", 1, lzfake.TextContent("hi"))

	id, err := e.client.SendText(context.Background(), "s1", "Hello & welcome, 100% sure?")
	if err != nil || id == "" {
		t.Fatalf("SendText = %q, %v", id, err)
	}
	sent := e.fake.Sent()
	if len(sent) != 1 || sent[0].Text != "Hello & welcome, 100% sure?" || sent[0].SessionID != "s1" ||
		sent[0].TemplateID != "1" || sent[0].MessageID != id {
		t.Fatalf("sent = %+v", sent)
	}
	req := e.fake.Requests()[0]
	if req.Method != http.MethodPost || req.ContentType != "application/x-www-form-urlencoded" {
		t.Errorf("request = %s %q", req.Method, req.ContentType)
	}
	if req.Form.Get("txt") != "Hello & welcome, 100% sure?" || req.Form.Get("template_id") != "1" || req.Form.Get("session_id") != "s1" {
		t.Errorf("form = %v", req.Form)
	}
	if req.Query.Get("app_key") != testKey || req.Query.Get("sign") == "" || req.Query.Get("access_token") != e.token {
		t.Errorf("system parameters not in the query: %v", req.Query)
	}

	// The sent message shows up in the list like any other.
	p, err := e.client.MessageList(context.Background(), "s1", 0, 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(p.Messages[0].MessageID) != id || p.Messages[0].FromAccountType != lzcustomer.AccountSeller {
		t.Errorf("newest message = %+v", p.Messages[0])
	}
}

func TestSendTextReportsBlockedMessage(t *testing.T) {
	e := newEnv(t)
	e.fake.AddBuyerMessage("s1", 1, lzfake.TextContent("hi"))
	e.fake.BlockNextSend("NOTE: The message has not been sent.")
	if _, err := e.client.SendText(context.Background(), "s1", "call me"); err != nil {
		t.Fatal(err)
	}
	p, _ := e.client.MessageList(context.Background(), "s1", 0, 5, "")
	if p.Messages[0].ProcessMsg != "NOTE: The message has not been sent." {
		t.Errorf("process_msg = %q", p.Messages[0].ProcessMsg)
	}
}

func TestReadSession(t *testing.T) {
	e := newEnv(t)
	last := e.fake.AddBuyerMessage("s1", 1, lzfake.TextContent("hi"))
	if err := e.client.ReadSession(context.Background(), "s1", last); err != nil {
		t.Fatal(err)
	}
	reads := e.fake.Reads()
	if len(reads) != 1 || reads[0].SessionID != "s1" || reads[0].LastReadMessageID != last {
		t.Fatalf("reads = %+v", reads)
	}
	req := e.fake.Requests()[0]
	if req.Method != http.MethodPost || req.Path != "/im/session/read" || req.Form.Get("last_read_message_id") != last {
		t.Errorf("request = %+v", req)
	}
	page, _ := e.client.SessionList(context.Background(), 0, 5, "")
	if page.Sessions[0].UnreadCount != 0 {
		t.Errorf("unread = %d after read", page.Sessions[0].UnreadCount)
	}
}

func TestBadSignatureIsRejectedByTheFake(t *testing.T) {
	e := newEnv(t)
	wrong := lzcustomer.NewClient(e.fake.HTTPClient(), e.fake.URL(), testKey, "not-the-secret", e.clock.Now, staticToken(e.token))
	_, err := wrong.SessionList(context.Background(), 0, 5, "")
	var apiErr *lzcustomer.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "IncompleteSignature" {
		t.Fatalf("err = %v", err)
	}
}

func TestInjectedErrorsThroughTheClient(t *testing.T) {
	e := newEnv(t)
	e.fake.AddBuyerMessage("s1", 1, lzfake.TextContent("hi"))
	ctx := context.Background()

	e.fake.FailNext("/im/session/list", "ApiCallLimit")
	if _, err := e.client.SessionList(ctx, 0, 5, ""); !errors.Is(err, lzcustomer.ErrRateLimited) {
		t.Errorf("ApiCallLimit -> %v", err)
	}
	e.fake.FailNext("/im/session/list", "IllegalAccessToken")
	if _, err := e.client.SessionList(ctx, 0, 5, ""); !errors.Is(err, lzcustomer.ErrAuth) {
		t.Errorf("IllegalAccessToken -> %v", err)
	}
	// The injection is one-shot.
	if _, err := e.client.SessionList(ctx, 0, 5, ""); err != nil {
		t.Errorf("third call: %v", err)
	}

	e.fake.FailNextHTTP("/im/message/send", http.StatusTooManyRequests)
	if _, err := e.client.SendText(ctx, "s1", "x"); !errors.Is(err, lzcustomer.ErrRateLimited) {
		t.Errorf("http 429 -> %v", err)
	}
	e.fake.FailNextHTTP("/im/message/send", http.StatusForbidden)
	if _, err := e.client.SendText(ctx, "s1", "x"); !errors.Is(err, lzcustomer.ErrAuth) {
		t.Errorf("http 403 -> %v", err)
	}
	e.fake.FailNext("/im/message/send", "InvalidParameter")
	var apiErr *lzcustomer.APIError
	err := e.client.ReadSession(ctx, "s1", "m1") // no injection on this path
	if err != nil {
		t.Errorf("read: %v", err)
	}
	_, err = e.client.SendText(ctx, "s1", "x")
	if !errors.As(err, &apiErr) || errors.Is(err, lzcustomer.ErrAuth) || errors.Is(err, lzcustomer.ErrRateLimited) || apiErr.Code != "InvalidParameter" {
		t.Errorf("other code -> %v", err)
	}
}

func TestClientStopsWhenThereIsNoToken(t *testing.T) {
	e := newEnv(t)
	c := lzcustomer.NewClient(e.fake.HTTPClient(), e.fake.URL(), testKey, testSecret, e.clock.Now, noToken{})
	if _, err := c.SessionList(context.Background(), 0, 5, ""); !errors.Is(err, lzcustomer.ErrNotAuthorized) {
		t.Fatalf("err = %v", err)
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Errorf("%d requests went out without a token", n)
	}
}

type noToken struct{}

func (noToken) Token(context.Context) (string, error) { return "", lzcustomer.ErrNotAuthorized }
