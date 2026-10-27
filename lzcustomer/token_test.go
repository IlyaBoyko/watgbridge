package lzcustomer_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"watgbridge/lzcustomer"
	"watgbridge/lzcustomer/lzfake"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if err := lzcustomer.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

type tokenEnv struct {
	fake   *lzfake.Server
	clock  *fakeClock
	source *lzcustomer.TokenSource
	db     *gorm.DB
}

func newTokenEnv(t *testing.T) *tokenEnv {
	t.Helper()
	clock := newFakeClock()
	fake := lzfake.New(testKey, testSecret)
	t.Cleanup(fake.Close)
	fake.SetNow(clock.Now)
	fake.SetSellerID("seller-77")
	db := newTestDB(t)
	auth := lzcustomer.NewAuth(fake.HTTPClient(), fake.URL(), testKey, testSecret, clock.Now)
	return &tokenEnv{fake: fake, clock: clock, db: db, source: lzcustomer.NewTokenSource(db, auth, clock.Now)}
}

func (e *tokenEnv) authorize(t *testing.T) lzcustomer.Authorized {
	t.Helper()
	e.fake.AllowCode("the-code")
	a, err := e.source.Authorize(context.Background(), "the-code")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestTokenWithoutRowIsNotAuthorized(t *testing.T) {
	e := newTokenEnv(t)
	if _, err := e.source.Token(context.Background()); !errors.Is(err, lzcustomer.ErrNotAuthorized) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := e.source.Status(); ok {
		t.Error("Status reports an authorization that does not exist")
	}
}

func TestAuthorizeStoresTokenSellerAndExpiries(t *testing.T) {
	e := newTokenEnv(t)
	e.fake.SetLifetimes(7*24*time.Hour, 30*24*time.Hour)
	got := e.authorize(t)

	now := e.clock.Now()
	// The fake lists an SG user first; the seller of the token's own country wins.
	if got.SellerID != "seller-77" || got.Country != "my" || got.Account != "seller@example.invalid" {
		t.Errorf("Authorized = %+v", got)
	}
	if !got.AccessExpiresAt.Equal(now.Add(7*24*time.Hour)) || !got.RefreshExpiresAt.Equal(now.Add(30*24*time.Hour)) {
		t.Errorf("expiries = %v / %v", got.AccessExpiresAt, got.RefreshExpiresAt)
	}
	if st, ok := e.source.Status(); !ok || !sameAuth(st, got) {
		t.Errorf("Status = %+v, %v", st, ok)
	}

	var rows []lzcustomer.TokenRow
	if err := e.db.Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].ID != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if rows[0].AccessToken == "" || rows[0].RefreshToken == "" || rows[0].SellerID != "seller-77" {
		t.Errorf("row = %+v", rows[0])
	}

	// The stored token works against the fake.
	tok, err := e.source.Token(context.Background())
	if err != nil || tok != rows[0].AccessToken {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	c := lzcustomer.NewClient(e.fake.HTTPClient(), e.fake.URL(), testKey, testSecret, e.clock.Now, e.source)
	if _, err := c.SessionList(context.Background(), 0, 5, ""); err != nil {
		t.Errorf("call with the stored token: %v", err)
	}
	if e.fake.Calls("/auth/token/refresh") != 0 {
		t.Error("a fresh token was refreshed")
	}

	// Authorizing again replaces the row rather than adding one.
	e.authorize(t)
	var n int64
	e.db.Model(&lzcustomer.TokenRow{}).Count(&n)
	if n != 1 {
		t.Errorf("%d rows after a second authorization", n)
	}
}

func TestAuthorizeWithABadCodeIsAnAuthError(t *testing.T) {
	e := newTokenEnv(t)
	_, err := e.source.Authorize(context.Background(), "never-issued")
	if !errors.Is(err, lzcustomer.ErrAuth) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := e.source.Status(); ok {
		t.Error("a failed authorization stored something")
	}
}

func TestNearExpiryRefreshesOnceUnderConcurrency(t *testing.T) {
	e := newTokenEnv(t)
	e.fake.SetLifetimes(7*24*time.Hour, 30*24*time.Hour)
	e.authorize(t)
	old, _ := e.source.Token(context.Background())

	// 10% of 7 days is 16.8 h: 16 h before expiry is inside the window.
	e.clock.Advance(7*24*time.Hour - 16*time.Hour)

	const callers = 20
	tokens := make([]string, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := e.source.Token(context.Background())
			if err != nil {
				t.Error(err)
			}
			tokens[i] = tok
		}(i)
	}
	wg.Wait()

	if n := e.fake.Calls("/auth/token/refresh"); n != 1 {
		t.Errorf("%d refresh calls, want 1", n)
	}
	for _, tok := range tokens {
		if tok == "" || tok == old || tok != tokens[0] {
			t.Fatalf("callers saw different or stale tokens: old=%q got=%v", old, tokens)
		}
	}
	st, _ := e.source.Status()
	if !st.AccessExpiresAt.Equal(e.clock.Now().Add(7 * 24 * time.Hour)) {
		t.Errorf("new expiry = %v", st.AccessExpiresAt)
	}
	if st.SellerID != "seller-77" {
		t.Errorf("seller id lost in the refresh: %+v", st)
	}
}

func TestRefreshWindowIsAtLeastOneHour(t *testing.T) {
	e := newTokenEnv(t)
	// 10% of 2 h is 12 min, but the window is 1 h.
	e.fake.SetLifetimes(2*time.Hour, 30*24*time.Hour)
	e.authorize(t)

	e.clock.Advance(59 * time.Minute) // 61 min left: outside the window
	if _, err := e.source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.fake.Calls("/auth/token/refresh"); n != 0 {
		t.Fatalf("refreshed too early (%d calls)", n)
	}
	e.clock.Advance(2 * time.Minute) // 59 min left: inside
	if _, err := e.source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.fake.Calls("/auth/token/refresh"); n != 1 {
		t.Fatalf("%d refresh calls, want 1", n)
	}
}

func TestLongLifetimeUsesTenPercent(t *testing.T) {
	e := newTokenEnv(t)
	e.fake.SetLifetimes(100*time.Hour, 30*24*time.Hour) // window = 10 h
	e.authorize(t)
	e.clock.Advance(89 * time.Hour) // 11 h left
	_, _ = e.source.Token(context.Background())
	if n := e.fake.Calls("/auth/token/refresh"); n != 0 {
		t.Fatalf("refreshed with 11 h left (%d)", n)
	}
	e.clock.Advance(2 * time.Hour) // 9 h left
	_, _ = e.source.Token(context.Background())
	if n := e.fake.Calls("/auth/token/refresh"); n != 1 {
		t.Fatalf("%d refresh calls with 9 h left, want 1", n)
	}
}

func TestRefreshRejectedAsAuthErrorIsNotAuthorized(t *testing.T) {
	e := newTokenEnv(t)
	e.authorize(t)
	e.fake.RevokeTokens() // Lazada no longer knows the refresh token
	e.clock.Advance(7*24*time.Hour - 30*time.Minute)

	_, err := e.source.Token(context.Background())
	if !errors.Is(err, lzcustomer.ErrNotAuthorized) {
		t.Fatalf("err = %v", err)
	}
}

// failingAuth lets a test make the refresh fail any way it likes.
type failingAuth struct {
	inner lzcustomer.TokenAPI
	err   error
	calls atomic.Int32
}

func (f *failingAuth) CreateToken(ctx context.Context, code string) (lzcustomer.TokenResponse, error) {
	return f.inner.CreateToken(ctx, code)
}

func (f *failingAuth) RefreshToken(ctx context.Context, rt string) (lzcustomer.TokenResponse, error) {
	f.calls.Add(1)
	if f.err != nil {
		return lzcustomer.TokenResponse{}, f.err
	}
	return f.inner.RefreshToken(ctx, rt)
}

func TestTransientRefreshFailureKeepsTheOldToken(t *testing.T) {
	e := newTokenEnv(t)
	e.fake.AllowCode("c")
	auth := &failingAuth{inner: lzcustomer.NewAuth(e.fake.HTTPClient(), e.fake.URL(), testKey, testSecret, e.clock.Now)}
	src := lzcustomer.NewTokenSource(e.db, auth, e.clock.Now)
	if _, err := src.Authorize(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	old, _ := src.Token(context.Background())

	e.clock.Advance(7*24*time.Hour - 2*time.Hour) // inside the window, still valid
	auth.err = errors.New("connection reset")
	tok, err := src.Token(context.Background())
	if err != nil || tok != old {
		t.Fatalf("Token = %q, %v; want the old token", tok, err)
	}
	// A rate limit is transient too.
	auth.err = lzcustomer.ErrRateLimited
	if tok, err := src.Token(context.Background()); err != nil || tok != old {
		t.Fatalf("rate limited: Token = %q, %v", tok, err)
	}

	// The next call tries again and succeeds.
	auth.err = nil
	tok, err = src.Token(context.Background())
	if err != nil || tok == old || tok == "" {
		t.Fatalf("after recovery: Token = %q, %v", tok, err)
	}
	if auth.calls.Load() != 3 {
		t.Errorf("%d refresh attempts, want 3", auth.calls.Load())
	}
}

func TestTransientRefreshFailureOnAnExpiredTokenIsAnError(t *testing.T) {
	e := newTokenEnv(t)
	e.fake.AllowCode("c")
	auth := &failingAuth{inner: lzcustomer.NewAuth(e.fake.HTTPClient(), e.fake.URL(), testKey, testSecret, e.clock.Now)}
	src := lzcustomer.NewTokenSource(e.db, auth, e.clock.Now)
	if _, err := src.Authorize(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(7*24*time.Hour + time.Minute)
	auth.err = errors.New("connection reset")
	_, err := src.Token(context.Background())
	if err == nil || errors.Is(err, lzcustomer.ErrNotAuthorized) {
		t.Fatalf("err = %v, want the transient error", err)
	}
}

func TestExpiredRefreshTokenIsNotAuthorizedOnceTheAccessTokenLapses(t *testing.T) {
	e := newTokenEnv(t)
	e.fake.SetLifetimes(7*24*time.Hour, 3*24*time.Hour) // refresh lapses first
	e.authorize(t)

	e.clock.Advance(6*24*time.Hour + 20*time.Hour) // 4 h left: inside the window
	if _, err := e.source.Token(context.Background()); err != nil {
		t.Fatalf("access token is still valid: %v", err)
	}
	if n := e.fake.Calls("/auth/token/refresh"); n != 0 {
		t.Errorf("tried to refresh with an expired refresh token (%d)", n)
	}
	e.clock.Advance(2 * 24 * time.Hour)
	if _, err := e.source.Token(context.Background()); !errors.Is(err, lzcustomer.ErrNotAuthorized) {
		t.Fatalf("err = %v", err)
	}
}

// Migrate must be safe on a database that already has it and its rows.
func TestMigrateIsIdempotentAndKeepsTheRow(t *testing.T) {
	e := newTokenEnv(t)
	e.authorize(t)
	before, _ := e.source.Status()
	if err := lzcustomer.Migrate(e.db); err != nil {
		t.Fatal(err)
	}
	if err := lzcustomer.Migrate(e.db); err != nil {
		t.Fatal(err)
	}
	if after, ok := e.source.Status(); !ok || !sameAuth(after, before) {
		t.Errorf("row changed across Migrate: %+v -> %+v", before, after)
	}
}

// sameAuth compares by instant: a time read back from SQLite may carry a
// different location than the one written.
func sameAuth(a, b lzcustomer.Authorized) bool {
	return a.SellerID == b.SellerID && a.Country == b.Country && a.Account == b.Account &&
		a.AccessExpiresAt.Equal(b.AccessExpiresAt) && a.RefreshExpiresAt.Equal(b.RefreshExpiresAt)
}
