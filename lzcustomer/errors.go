package lzcustomer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

var (
	// ErrAuth: the token (or the authorization code) is not accepted and
	// retrying will not help; the owner has to authorize again.
	ErrAuth = errors.New("lazada: not authorized")
	// ErrRateLimited: Lazada throttled the call. The caller backs off.
	ErrRateLimited = errors.New("lazada: rate limited")
	// ErrNotAuthorized: no usable token is stored, or it could not be
	// refreshed. The owner has to send /lazada_auth.
	ErrNotAuthorized = errors.New("lazada: no usable authorization")
)

// APIError is a failed Lazada call. Auth and rate-limit failures are APIErrors
// too, so the code and message are always reachable with errors.As, while
// errors.Is(err, ErrAuth) and errors.Is(err, ErrRateLimited) tell them apart.
type APIError struct {
	HTTPStatus int
	Code       string
	Message    string
	RequestID  string
	kind       error
}

func (e *APIError) Error() string {
	s := "lazada: " + e.Code
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.HTTPStatus != 0 && e.HTTPStatus != http.StatusOK {
		s += " (http " + strconv.Itoa(e.HTTPStatus) + ")"
	}
	return s
}

func (e *APIError) Unwrap() error { return e.kind }

// Lazada's error codes are not listed in one place we could read offline, so
// besides the documented ones the match is by shape: any *CallLimit code is a
// rate limit, and any token-related code that says illegal, invalid or expired
// is an auth failure.
func classify(code, message string) error {
	lc := strings.ToLower(code)
	switch lc {
	case "apicalllimit", "appcalllimit", "servicecalllimit":
		return ErrRateLimited
	case "illegalaccesstoken", "illegalrefreshtoken", "invalidcode":
		return ErrAuth
	}
	if strings.HasSuffix(lc, "calllimit") {
		return ErrRateLimited
	}
	if strings.Contains(lc, "token") && (strings.Contains(lc, "illegal") ||
		strings.Contains(lc, "invalid") || strings.Contains(lc, "expire")) {
		return ErrAuth
	}
	lm := strings.ToLower(message)
	if (strings.Contains(lm, "access token") || strings.Contains(lm, "refresh token")) &&
		strings.Contains(lm, "expire") {
		return ErrAuth
	}
	return nil
}

// envelope is the frame around every Lazada answer. Errors come as
// {"code":"IllegalAccessToken","type":"ISP","message":"..."}; successes as
// {"code":"0","data":{...},"success":"true","err_code":"0",...}.
type envelope struct {
	Code       FlexString      `json:"code"`
	Message    FlexString      `json:"message"`
	Success    *FlexBool       `json:"success"` // nil when absent
	ErrCode    FlexString      `json:"err_code"`
	ErrMessage FlexString      `json:"err_message"`
	RequestID  FlexString      `json:"request_id"`
	Data       json.RawMessage `json:"data"`
}

// check turns an HTTP answer into either the envelope or a typed error. It is
// OK only when code is "0" (or absent) and, where present, success is true and
// err_code is "0".
func check(status int, body []byte) (*envelope, error) {
	var env envelope
	parsed := json.Unmarshal(body, &env) == nil

	apiErr := &APIError{HTTPStatus: status}
	if parsed {
		apiErr.RequestID = string(env.RequestID)
	}

	// The HTTP status decides first for the statuses that mean the same thing
	// whatever the body says.
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		apiErr.kind = ErrAuth
	case http.StatusTooManyRequests:
		apiErr.kind = ErrRateLimited
	}

	if parsed {
		codeOK := env.Code == "" || env.Code == "0"
		errCodeOK := env.ErrCode == "" || env.ErrCode == "0"
		successOK := env.Success == nil || bool(*env.Success)
		if codeOK && errCodeOK && successOK && status < 300 {
			return &env, nil
		}

		switch {
		case !codeOK:
			apiErr.Code = string(env.Code)
		case !errCodeOK:
			apiErr.Code = string(env.ErrCode)
		}
		apiErr.Message = string(env.Message)
		if apiErr.Message == "" && env.ErrMessage != "null" {
			apiErr.Message = string(env.ErrMessage)
		}
	} else if status < 300 {
		return nil, fmt.Errorf("lazada: undecodable response (http %d)", status)
	}

	if apiErr.Code == "" {
		if status >= 300 {
			apiErr.Code = "HTTP" + strconv.Itoa(status)
		} else {
			apiErr.Code = "UnknownError" // success:false with no code
		}
	}
	if apiErr.kind == nil {
		apiErr.kind = classify(apiErr.Code, apiErr.Message)
	}
	return nil, apiErr
}
