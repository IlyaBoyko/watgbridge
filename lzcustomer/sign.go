// Package lzcustomer is the client for the Lazada Open Platform's Instant
// Messaging API: request signing, the four IM calls, the token lifecycle and
// the table the tokens live in. The channel built on top of it (fetcher,
// topics, events) lives elsewhere; this package knows nothing about Telegram
// or the Agent.
package lzcustomer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Sign computes Lazada's request signature. params holds every request
// parameter, system and API alike; a "sign" entry and empty values are
// skipped. They are sorted by name in byte order, concatenated as name+value
// behind the API path, and HMAC-SHA256'd with the app secret. The result is
// uppercase hex, which is what the gateway compares against.
func Sign(appSecret, apiPath string, params map[string]string) string {
	names := make([]string, 0, len(params))
	for name, value := range params {
		if name == "sign" || value == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var base strings.Builder
	base.WriteString(apiPath)
	for _, name := range names {
		base.WriteString(name)
		base.WriteString(params[name])
	}

	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte(base.String()))
	return strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
}

// VerifyPush checks the Authorization header of a Lazada push: HMAC-SHA256 of
// appKey followed by the raw body, keyed with the app secret. Lazada sends it
// as lowercase hex, but the comparison ignores case and runs in constant time.
// The Agent does this check in production; the Hub needs it for its fake and
// for later use.
func VerifyPush(appKey, appSecret string, body []byte, header string) bool {
	// A Hub with no secret configured must not accept anything.
	if appSecret == "" {
		return false
	}
	got, err := hex.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte(appKey))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// PushSignature is the header value a Lazada push carries for body. Only fakes
// and tests that simulate Lazada need it.
func PushSignature(appKey, appSecret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte(appKey))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
