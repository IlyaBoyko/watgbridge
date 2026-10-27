package lzcustomer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// minRefreshWindow is the least time before expiry at which a token is
// refreshed. A token that is renewed with an hour to spare survives an
// overnight Lazada or network outage.
const minRefreshWindow = time.Hour

// TokenAPI is the two auth calls. *Auth implements it; tests substitute their
// own to inject failures.
type TokenAPI interface {
	CreateToken(ctx context.Context, code string) (TokenResponse, error)
	RefreshToken(ctx context.Context, refreshToken string) (TokenResponse, error)
}

// Auth calls the auth host (https://auth.lazada.com/rest). Its requests are
// signed like the IM ones but carry no access token.
type Auth struct{ signedCaller }

var _ TokenAPI = (*Auth)(nil)

func NewAuth(hc *http.Client, authURL, appKey, appSecret string, now func() time.Time) *Auth {
	return &Auth{newSignedCaller(hc, authURL, appKey, appSecret, now)}
}

// tokenCall decodes a token answer. Lazada puts the fields at the top level of
// the body; they are read from "data" instead if a gateway ever wraps them.
func (a *Auth) tokenCall(ctx context.Context, apiPath string, params map[string]string) (TokenResponse, error) {
	var out TokenResponse
	env, raw, err := a.call(ctx, http.MethodPost, apiPath, "", params)
	if err != nil {
		return out, err
	}
	src := raw
	if len(env.Data) > 0 && string(env.Data) != "null" {
		src = env.Data
	}
	if err := json.Unmarshal(src, &out); err != nil {
		return out, fmt.Errorf("lazada: decoding token response: %w", err)
	}
	if out.AccessToken == "" {
		return out, fmt.Errorf("lazada: token response has no access_token")
	}
	// Without a lifetime the token would look expired the moment it is stored.
	if out.ExpiresIn <= 0 {
		return out, fmt.Errorf("lazada: token response has no expires_in")
	}
	return out, nil
}

// CreateToken exchanges the single-use authorization code.
func (a *Auth) CreateToken(ctx context.Context, code string) (TokenResponse, error) {
	return a.tokenCall(ctx, "/auth/token/create", map[string]string{"code": code})
}

func (a *Auth) RefreshToken(ctx context.Context, refreshToken string) (TokenResponse, error) {
	return a.tokenCall(ctx, "/auth/token/refresh", map[string]string{"refresh_token": refreshToken})
}

// Authorized describes the stored authorization, without the tokens.
type Authorized struct {
	SellerID         string
	Country          string
	Account          string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

// TokenSource keeps the access token fresh. It implements TokenProvider.
type TokenSource struct {
	store tokenStore
	api   TokenAPI
	now   func() time.Time

	// mu spans the read-check-refresh-write cycle so that concurrent callers
	// refresh once: the second one finds the new token when it gets in.
	mu sync.Mutex
}

var _ TokenProvider = (*TokenSource)(nil)

func NewTokenSource(db *gorm.DB, api TokenAPI, now func() time.Time) *TokenSource {
	if now == nil {
		now = time.Now
	}
	return &TokenSource{store: tokenStore{db}, api: api, now: now}
}

// Token returns the access token, refreshing it first when it expires within
// max(10% of its lifetime, 1 h).
//
//   - No stored row: ErrNotAuthorized.
//   - The refresh is refused as an auth failure: ErrNotAuthorized.
//   - The refresh fails any other way while the old token is still valid: the
//     old token is returned and the next call tries again.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row, found, err := s.store.load()
	if err != nil {
		return "", err
	}
	if !found || row.AccessToken == "" {
		return "", ErrNotAuthorized
	}

	now := s.now()
	window := row.AccessExpiresAt.Sub(row.UpdatedAt) / 10
	if row.UpdatedAt.IsZero() || window < minRefreshWindow {
		window = minRefreshWindow
	}
	if now.Before(row.AccessExpiresAt.Add(-window)) {
		return row.AccessToken, nil
	}

	stillValid := now.Before(row.AccessExpiresAt)
	if row.RefreshToken == "" || !now.Before(row.RefreshExpiresAt) {
		// Nothing to refresh with. The access token is good until it isn't.
		if stillValid {
			return row.AccessToken, nil
		}
		return "", ErrNotAuthorized
	}

	resp, err := s.api.RefreshToken(ctx, row.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrAuth) {
			return "", fmt.Errorf("%w: %w", ErrNotAuthorized, err)
		}
		if stillValid {
			return row.AccessToken, nil
		}
		return "", err
	}
	row = s.merge(row, resp)
	if err := s.store.save(row); err != nil {
		return "", err
	}
	return row.AccessToken, nil
}

// Authorize exchanges an authorization code and stores the result, replacing
// any earlier authorization.
func (s *TokenSource) Authorize(ctx context.Context, code string) (Authorized, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	resp, err := s.api.CreateToken(ctx, strings.TrimSpace(code))
	if err != nil {
		return Authorized{}, err
	}
	row := s.merge(TokenRow{}, resp)
	if err := s.store.save(row); err != nil {
		return Authorized{}, err
	}
	return authorizedFrom(row), nil
}

// Status reports what is stored, for the channel's expiry warning. The bool is
// false when nothing is stored (or the table cannot be read).
func (s *TokenSource) Status() (Authorized, bool) {
	row, found, err := s.store.load()
	if err != nil || !found {
		return Authorized{}, false
	}
	return authorizedFrom(row), true
}

func authorizedFrom(row TokenRow) Authorized {
	return Authorized{
		SellerID: row.SellerID, Country: row.Country, Account: row.Account,
		AccessExpiresAt: row.AccessExpiresAt, RefreshExpiresAt: row.RefreshExpiresAt,
	}
}

// merge applies a token answer to a row. Fields the answer leaves empty keep
// their old value: a refresh answer that omits the seller list must not erase
// the seller id.
func (s *TokenSource) merge(row TokenRow, resp TokenResponse) TokenRow {
	now := s.now()
	row.AccessToken = string(resp.AccessToken)
	row.AccessExpiresAt = now.Add(time.Duration(resp.ExpiresIn) * time.Second)
	row.UpdatedAt = now
	if resp.RefreshToken != "" {
		row.RefreshToken = string(resp.RefreshToken)
		row.RefreshExpiresAt = now.Add(time.Duration(resp.RefreshExpiresIn) * time.Second)
	}
	if resp.Country != "" {
		row.Country = string(resp.Country)
	}
	if resp.Account != "" {
		row.Account = string(resp.Account)
	}
	if id := sellerIDFor(resp); id != "" {
		row.SellerID = id
	}
	return row
}

// sellerIDFor picks the seller of the token's own country, else the first one
// listed.
func sellerIDFor(resp TokenResponse) string {
	for _, u := range resp.CountryUserInfo {
		if strings.EqualFold(string(u.Country), string(resp.Country)) && u.SellerID != "" {
			return string(u.SellerID)
		}
	}
	for _, u := range resp.CountryUserInfo {
		if u.SellerID != "" {
			return string(u.SellerID)
		}
	}
	return ""
}
