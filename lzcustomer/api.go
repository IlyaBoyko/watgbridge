package lzcustomer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// callTimeout bounds one call, connection and body included.
const callTimeout = 20 * time.Second

// maxBody guards against a runaway answer; a page of 100 messages is far
// below it.
const maxBody = 8 << 20

// API is what the channel and its fakes use. Lists are paged newest first:
// startTimeMs is the first page's "now" and then the previous page's
// NextStartTime, and the last-id argument is the previous page's last id.
type API interface {
	SessionList(ctx context.Context, startTimeMs int64, pageSize int, lastSessionID string) (SessionPage, error)
	MessageList(ctx context.Context, sessionID string, startTimeMs int64, pageSize int, lastMessageID string) (MessagePage, error)
	SendText(ctx context.Context, sessionID, text string) (messageID string, err error)
	ReadSession(ctx context.Context, sessionID, lastReadMessageID string) error
}

// TokenProvider hands the Client the access token for a call. TokenSource is
// the production one.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// signedCaller sends signed requests to one base URL. The Client and the Auth
// client differ only in the base URL and whether a token goes along.
type signedCaller struct {
	hc      *http.Client
	baseURL string
	key     string
	secret  string
	now     func() time.Time
}

func newSignedCaller(hc *http.Client, baseURL, key, secret string, now func() time.Time) signedCaller {
	if hc == nil {
		hc = &http.Client{}
	}
	if now == nil {
		now = time.Now
	}
	return signedCaller{hc: hc, baseURL: strings.TrimRight(baseURL, "/"), key: key, secret: secret, now: now}
}

// call sends one signed request and returns the checked envelope and the raw
// body. accessToken is empty for the auth calls. Request bodies and tokens are
// never logged here or anywhere else in the package.
func (c signedCaller) call(ctx context.Context, method, apiPath, accessToken string, params map[string]string) (*envelope, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	sys := map[string]string{
		"app_key":     c.key,
		"timestamp":   strconv.FormatInt(c.now().UnixMilli(), 10),
		"sign_method": "sha256",
	}
	if accessToken != "" {
		sys["access_token"] = accessToken
	}
	all := make(map[string]string, len(sys)+len(params))
	for k, v := range sys {
		all[k] = v
	}
	api := url.Values{}
	for k, v := range params {
		if v == "" {
			continue
		}
		all[k] = v
		api.Set(k, v)
	}
	sys["sign"] = Sign(c.secret, apiPath, all)

	query := url.Values{}
	for k, v := range sys {
		query.Set(k, v)
	}

	var body io.Reader
	target := c.baseURL + apiPath
	switch method {
	case http.MethodGet:
		// Everything in the query string.
		for k, v := range api {
			query[k] = v
		}
	case http.MethodPost:
		// As the official SDKs do: system parameters in the query string, the
		// API's own parameters as a form body.
		body = strings.NewReader(api.Encode())
	default:
		return nil, nil, fmt.Errorf("lazada: unsupported method %s", method)
	}
	req, err := http.NewRequestWithContext(ctx, method, target+"?"+query.Encode(), body)
	if err != nil {
		return nil, nil, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, redactURLError(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, fmt.Errorf("lazada: reading response: %w", err)
	}
	env, err := check(resp.StatusCode, raw)
	return env, raw, err
}

// redactURLError drops the request URL from a transport error: it carries the
// access token and signature in its query string.
func redactURLError(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return fmt.Errorf("lazada: %s: %w", ue.Op, ue.Err)
	}
	return err
}

// Client is the signed IM client.
type Client struct {
	signedCaller
	tokens TokenProvider
}

var _ API = (*Client)(nil)

// NewClient builds an IM client. hc may be nil; now is the clock behind the
// request timestamps (nil = time.Now).
func NewClient(hc *http.Client, apiURL, appKey, appSecret string, now func() time.Time, tokens TokenProvider) *Client {
	return &Client{signedCaller: newSignedCaller(hc, apiURL, appKey, appSecret, now), tokens: tokens}
}

func (c *Client) call(ctx context.Context, method, apiPath string, params map[string]string) (json.RawMessage, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	env, _, err := c.signedCaller.call(ctx, method, apiPath, token, params)
	if err != nil {
		return nil, err
	}
	return env.Data, nil
}

func decodeData(data json.RawMessage, into any) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("lazada: decoding response: %w", err)
	}
	return nil
}

// startTime resolves the first page's "now". Paging then passes the previous
// page's NextStartTime.
func (c *Client) startTime(ms int64) string {
	if ms <= 0 {
		ms = c.now().UnixMilli()
	}
	return strconv.FormatInt(ms, 10)
}

func (c *Client) SessionList(ctx context.Context, startTimeMs int64, pageSize int, lastSessionID string) (SessionPage, error) {
	data, err := c.call(ctx, http.MethodGet, "/im/session/list", map[string]string{
		"start_time":      c.startTime(startTimeMs),
		"page_size":       strconv.Itoa(pageSize),
		"last_session_id": lastSessionID,
	})
	var page SessionPage
	if err != nil {
		return page, err
	}
	return page, decodeData(data, &page)
}

func (c *Client) MessageList(ctx context.Context, sessionID string, startTimeMs int64, pageSize int, lastMessageID string) (MessagePage, error) {
	data, err := c.call(ctx, http.MethodGet, "/im/message/list", map[string]string{
		"session_id":      sessionID,
		"start_time":      c.startTime(startTimeMs),
		"page_size":       strconv.Itoa(pageSize),
		"last_message_id": lastMessageID,
	})
	var page MessagePage
	if err != nil {
		return page, err
	}
	return page, decodeData(data, &page)
}

// templateText is Lazada's text template id.
const templateText = 1

func (c *Client) SendText(ctx context.Context, sessionID, text string) (string, error) {
	data, err := c.call(ctx, http.MethodPost, "/im/message/send", map[string]string{
		"session_id":  sessionID,
		"template_id": strconv.Itoa(templateText),
		"txt":         text,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		MessageID FlexString `json:"message_id"`
	}
	if err := decodeData(data, &out); err != nil {
		return "", err
	}
	return string(out.MessageID), nil
}

func (c *Client) ReadSession(ctx context.Context, sessionID, lastReadMessageID string) error {
	_, err := c.call(ctx, http.MethodPost, "/im/session/read", map[string]string{
		"session_id":           sessionID,
		"last_read_message_id": lastReadMessageID,
	})
	return err
}
