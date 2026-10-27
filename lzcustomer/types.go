package lzcustomer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Lazada's API answers send every number and boolean as a JSON string
// ("from_account_type": "2", "has_more": "true"), while its pushes carry real
// numbers and booleans. These three types accept both so one struct decodes
// either, and callers get plain Go values by converting: int64(m.SendTime).

// FlexString is a string that also accepts a number, bool or object. A
// non-string value keeps its literal JSON text, so a 19-digit id sent as a
// number is not rounded through a float.
type FlexString string

func (s *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*s = ""
		return nil
	}
	if b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = FlexString(v)
		return nil
	}
	*s = FlexString(b)
	return nil
}

// FlexInt is an int64 that accepts 1517820392000 and "1517820392000". An empty
// string or null is 0.
type FlexInt int64

func (n *FlexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*n = 0
		return nil
	}
	text := string(b)
	if b[0] == '"' {
		if err := json.Unmarshal(b, &text); err != nil {
			return err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			*n = 0
			return nil
		}
	}
	if v, err := strconv.ParseInt(text, 10, 64); err == nil {
		*n = FlexInt(v)
		return nil
	}
	// "1.5E12" style numbers.
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return fmt.Errorf("lazada: %q is not a number", text)
	}
	*n = FlexInt(int64(f))
	return nil
}

// FlexBool is a bool that accepts true and "true" (and 1/0, "1"/"0"). An empty
// string or null is false.
type FlexBool bool

func (v *FlexBool) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	text := string(b)
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &text); err != nil {
			return err
		}
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "true", "1":
		*v = true
	case "false", "0", "", "null":
		*v = false
	default:
		return fmt.Errorf("lazada: %q is not a boolean", text)
	}
	return nil
}

// Session is one buyer conversation as /im/session/list returns it.
type Session struct {
	SessionID       FlexString      `json:"session_id"`
	BuyerID         FlexString      `json:"buyer_id"`
	Title           FlexString      `json:"title"` // the buyer's display name
	HeadURL         FlexString      `json:"head_url"`
	Summary         FlexString      `json:"summary"`
	UnreadCount     FlexInt         `json:"unread_count"`
	LastMessageID   FlexString      `json:"last_message_id"`
	LastMessageTime FlexInt         `json:"last_message_time"` // Unix ms
	SelfPosition    FlexString      `json:"self_position"`
	ToPosition      FlexString      `json:"to_position"`
	SiteID          FlexString      `json:"site_id"`
	Tags            json.RawMessage `json:"tags,omitempty"` // shape not documented; kept raw
}

// SessionPage is one page of sessions, newest activity first.
type SessionPage struct {
	Sessions      []Session  `json:"session_list"`
	HasMore       FlexBool   `json:"has_more"`
	NextStartTime FlexInt    `json:"next_start_time"`
	LastSessionID FlexString `json:"last_session_id"`
}

// From-account types in Message.FromAccountType.
const (
	AccountBuyer  = 1
	AccountSeller = 2
)

// Message types in Message.Type.
const (
	TypeUser   = 1
	TypeSystem = 2
)

// Message is one chat message. Content stays the raw string Lazada sent: it is
// itself JSON whose shape depends on TemplateID, and the channel parses it.
type Message struct {
	MessageID       FlexString `json:"message_id"`
	SessionID       FlexString `json:"session_id"`
	FromAccountType FlexInt    `json:"from_account_type"` // 1 buyer, 2 seller
	ToAccountType   FlexInt    `json:"to_account_type"`
	FromAccountID   FlexString `json:"from_account_id"`
	ToAccountID     FlexString `json:"to_account_id"`
	TemplateID      FlexInt    `json:"template_id"`
	Type            FlexInt    `json:"type"` // 1 user, 2 system
	Content         FlexString `json:"content"`
	SendTime        FlexInt    `json:"send_time"` // Unix ms
	Status          FlexInt    `json:"status"`    // 1 = recalled
	AutoReply       FlexBool   `json:"auto_reply"`
	ProcessMsg      FlexString `json:"process_msg,omitempty"` // set when Lazada's filter blocked the message
	SiteID          FlexString `json:"site_id"`
}

// MessagePage is one page of a session's messages, newest first.
type MessagePage struct {
	Messages      []Message  `json:"message_list"`
	HasMore       FlexBool   `json:"has_more"`
	NextStartTime FlexInt    `json:"next_start_time"`
	LastMessageID FlexString `json:"last_message_id"`
}

// CountryUser is one entry of a token response's country_user_info.
type CountryUser struct {
	Country   FlexString `json:"country"`
	UserID    FlexString `json:"user_id"`
	SellerID  FlexString `json:"seller_id"`
	ShortCode FlexString `json:"short_code"`
}

// TokenResponse is what /auth/token/create and /auth/token/refresh return.
// Lifetimes are seconds.
type TokenResponse struct {
	AccessToken      FlexString    `json:"access_token"`
	RefreshToken     FlexString    `json:"refresh_token"`
	ExpiresIn        FlexInt       `json:"expires_in"`
	RefreshExpiresIn FlexInt       `json:"refresh_expires_in"`
	Country          FlexString    `json:"country"`
	Account          FlexString    `json:"account"`
	CountryUserInfo  []CountryUser `json:"country_user_info"`
}
