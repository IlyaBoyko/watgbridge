package lzcustomer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantAuth bool
		wantRate bool
		wantCode string
	}{
		{"illegal access token", 200, `{"code":"IllegalAccessToken","type":"ISP","message":"bad"}`, true, false, "IllegalAccessToken"},
		{"illegal refresh token", 200, `{"code":"IllegalRefreshToken","message":"bad"}`, true, false, "IllegalRefreshToken"},
		{"invalid code", 200, `{"code":"InvalidCode","message":"bad"}`, true, false, "InvalidCode"},
		{"assumed: expired access token", 200, `{"code":"AccessTokenExpired","message":"x"}`, true, false, "AccessTokenExpired"},
		{"assumed: expired by message only", 200, `{"code":"ISP","message":"The access token is expired"}`, true, false, "ISP"},
		{"api call limit", 200, `{"code":"ApiCallLimit","message":"slow down"}`, false, true, "ApiCallLimit"},
		{"app call limit", 200, `{"code":"AppCallLimit"}`, false, true, "AppCallLimit"},
		{"service call limit", 200, `{"code":"ServiceCallLimit"}`, false, true, "ServiceCallLimit"},
		{"assumed: other call limit", 200, `{"code":"IpCallLimit"}`, false, true, "IpCallLimit"},
		{"http 401", 401, `{}`, true, false, "HTTP401"},
		{"http 403", 403, ``, true, false, "HTTP403"},
		{"http 429", 429, `{"message":"too many"}`, false, true, "HTTP429"},
		{"http 500", 500, `oops`, false, false, "HTTP500"},
		{"other code", 200, `{"code":"InvalidParameter","message":"nope"}`, false, false, "InvalidParameter"},
		{"success false, err_code set", 200, `{"code":"0","success":"false","err_code":"7","err_message":"seven"}`, false, false, "7"},
		{"success false, no code", 200, `{"success":false}`, false, false, "UnknownError"},
		{"err_code only", 200, `{"code":"0","success":"true","err_code":"ApiCallLimit"}`, false, true, "ApiCallLimit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClient(srv.Client(), srv.URL, "k", "s", nil, staticToken("tok"))

			err := c.ReadSession(context.Background(), "s1", "m1")
			if err == nil {
				t.Fatal("no error")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("%T is not an *APIError: %v", err, err)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", apiErr.Code, tc.wantCode)
			}
			if errors.Is(err, ErrAuth) != tc.wantAuth {
				t.Errorf("Is(ErrAuth) = %v, want %v", !tc.wantAuth, tc.wantAuth)
			}
			if errors.Is(err, ErrRateLimited) != tc.wantRate {
				t.Errorf("Is(ErrRateLimited) = %v, want %v", !tc.wantRate, tc.wantRate)
			}
		})
	}
}

func TestErrorKeepsCodeAndMessage(t *testing.T) {
	_, err := check(200, []byte(`{"code":"InvalidParameter","message":"session_id is bad","request_id":"r1"}`))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "session_id is bad" || apiErr.RequestID != "r1" {
		t.Fatalf("got %#v", err)
	}
	if !strings.Contains(err.Error(), "InvalidParameter") || !strings.Contains(err.Error(), "session_id is bad") {
		t.Errorf("Error() = %q", err)
	}
	// "null" is Lazada's way of saying no message.
	_, err = check(200, []byte(`{"code":"0","success":"false","err_code":"9","err_message":"null"}`))
	if !errors.As(err, &apiErr) || apiErr.Message != "" {
		t.Errorf("message = %q, want empty", apiErr.Message)
	}
}

func TestOKResponses(t *testing.T) {
	for _, body := range []string{
		`{"data":{}}`,
		`{"code":"0","data":{}}`,
		`{"code":"0","success":"true","err_code":"0","err_message":"null"}`,
		`{"code":0,"success":true,"err_code":0}`,
	} {
		if _, err := check(200, []byte(body)); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

// A transport failure must not leak the request URL: its query string carries
// the access token and the signature.
func TestTransportErrorDoesNotLeakURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	c := NewClient(nil, url, "k", "s", nil, staticToken("secret-token"))
	err := c.ReadSession(context.Background(), "s1", "m1")
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "sign=") {
		t.Errorf("error leaks request details: %v", err)
	}
}
