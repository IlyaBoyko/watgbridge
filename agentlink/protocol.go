// Package agentlink connects the Hub (this bridge) to the support Agent over
// the protocol in clarus-agent/protocol/README.md. All of the link's logic
// lives here; the bridge's own handlers only call the thin hooks in hooks.go.
package agentlink

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"time"
	"unicode/utf8"
)

// ProtocolVersion is the wire version this Hub speaks.
const ProtocolVersion = 1

// Message types.
const (
	TypeHello           = "hello"
	TypeWelcome         = "welcome"
	TypeCustomerMessage = "customer.message"
	TypeStaffMessage    = "staff.message"
	TypeControl         = "control"
	TypeCallback        = "callback"
	TypeResult          = "result"
	TypeError           = "error"
	TypeSend            = "send"
	TypeEditCard        = "edit_card"
	TypeAck             = "ack"
)

// Close codes (protocol §8).
const (
	CloseVersionMismatch = 4001
	CloseTokenRevoked    = 4003
	CloseReplaced        = 4009
)

// Result error codes.
const (
	ErrExpired             = "expired"
	ErrUnknownConversation = "unknown_conversation"
	ErrUnknownCard         = "unknown_card"
	ErrCustomerUnreachable = "customer_unreachable"
	ErrRateLimited         = "rate_limited"
	ErrInvalid             = "invalid"
	ErrInternal            = "internal"
)

// MaxInlineMediaBytes is the largest media body sent inline (protocol §6).
const MaxInlineMediaBytes = 5 * 1024 * 1024

// Envelope is the frame every message travels in.
type Envelope struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	TS      string          `json:"ts"`
	Payload json.RawMessage `json:"payload"`
}

type Media struct {
	Kind     string `json:"kind"`
	Mime     string `json:"mime"`
	Filename string `json:"filename,omitempty"`
	Caption  string `json:"caption,omitempty"`
	Size     int64  `json:"size"`
	Data     string `json:"data,omitempty"`
	TooLarge bool   `json:"too_large,omitempty"`
}

type Button struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Buttons is used through a pointer so that an empty list ("remove the
// buttons") survives a decode/encode round trip instead of being omitted.
type Buttons = [][]Button

// Copyable is a value the customer must be able to copy exactly (an account
// number, a wallet address). Used through a pointer, like Buttons.
type Copyable struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type Copyables = []Copyable

// SendLink is the single link under a reply (protocol section 5). Used through
// a pointer, like Copyables.
type SendLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// MaxLinkLabel is the longest link label the protocol allows.
const MaxLinkLabel = 64

// Limits from protocol section 5.
const (
	MaxCopyables     = 5
	MaxCopyableValue = 256
	MaxSignature     = 80
	MaxTitle         = 120
)

type Hello struct {
	HubID      string   `json:"hub_id"`
	Protocol   int      `json:"protocol"`
	HubVersion string   `json:"hub_version"`
	Channels   []string `json:"channels"`
}

type Welcome struct {
	Protocol     int    `json:"protocol"`
	AgentVersion string `json:"agent_version"`
}

type Contact struct {
	Name     string `json:"name"`
	Phone    string `json:"phone,omitempty"`
	Username string `json:"username,omitempty"`
}

type CustomerMessage struct {
	Conversation string  `json:"conversation"`
	TopicID      string  `json:"topic_id"`
	HubMsgID     string  `json:"hub_msg_id"`
	Contact      Contact `json:"contact"`
	Text         string  `json:"text,omitempty"`
	Media        []Media `json:"media"`
	ReplyTo      string  `json:"reply_to,omitempty"`
	EditOf       string  `json:"edit_of,omitempty"`
}

type StaffAuthor struct {
	Source   string `json:"source"`
	TgUserID string `json:"tg_user_id,omitempty"`
	Name     string `json:"name,omitempty"`
}

type StaffMessage struct {
	Conversation string      `json:"conversation"`
	TopicID      string      `json:"topic_id"`
	HubMsgID     string      `json:"hub_msg_id"`
	Author       StaffAuthor `json:"author"`
	Text         string      `json:"text,omitempty"`
	Media        []Media     `json:"media"`
	ReplyTo      string      `json:"reply_to,omitempty"`
}

type TgAuthor struct {
	TgUserID string `json:"tg_user_id"`
	Name     string `json:"name"`
}

type Control struct {
	Conversation   string   `json:"conversation"`
	TopicID        string   `json:"topic_id"`
	Command        string   `json:"command"`
	Args           string   `json:"args"`
	TargetHubMsgID string   `json:"target_hub_msg_id,omitempty"`
	Author         TgAuthor `json:"author"`
}

type Callback struct {
	Conversation string   `json:"conversation"`
	CardID       string   `json:"card_id"`
	ButtonID     string   `json:"button_id"`
	Author       TgAuthor `json:"author"`
}

type Result struct {
	CommandID   string `json:"command_id"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	HubMsgID    string `json:"hub_msg_id,omitempty"`
	DeliveredAt string `json:"delivered_at,omitempty"`
}

type Send struct {
	Conversation string     `json:"conversation"`
	Kind         string     `json:"kind"`
	Text         string     `json:"text,omitempty"`
	Media        []Media    `json:"media"`
	ReplyTo      string     `json:"reply_to,omitempty"`
	Buttons      *Buttons   `json:"buttons,omitempty"`
	CardID       string     `json:"card_id,omitempty"`
	Copyables    *Copyables `json:"copyables,omitempty"`
	Link         *SendLink  `json:"link,omitempty"`
	// Signature, ReplacesCard and Title shape how the topic looks (protocol
	// section 5b). All optional: an older Agent sends none of them.
	Signature    string `json:"signature,omitempty"`
	ReplacesCard string `json:"replaces_card,omitempty"`
	Title        string `json:"title,omitempty"`
	// Format "html" says Text is Telegram HTML, escaped by the Agent (protocol
	// section 5c). Empty is plain text. Only a tg: reply looks at it.
	Format    string `json:"format,omitempty"`
	ExpiresAt string `json:"expires_at"`
}

// FormatHTML is the one value of Send.Format.
const FormatHTML = "html"

type EditCard struct {
	Conversation string   `json:"conversation"`
	CardID       string   `json:"card_id"`
	Text         string   `json:"text"`
	Title        string   `json:"title,omitempty"`
	Buttons      *Buttons `json:"buttons,omitempty"`
	ExpiresAt    string   `json:"expires_at"`
}

type Ack struct {
	ID string `json:"id"`
}

type ProtocolError struct {
	RefID   string `json:"ref_id,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// payloadTypes maps every message type to a constructor for its payload
// struct. A type missing here is unknown to protocol v1.
var payloadTypes = map[string]func() any{
	TypeHello:           func() any { return &Hello{} },
	TypeWelcome:         func() any { return &Welcome{} },
	TypeCustomerMessage: func() any { return &CustomerMessage{} },
	TypeStaffMessage:    func() any { return &StaffMessage{} },
	TypeControl:         func() any { return &Control{} },
	TypeCallback:        func() any { return &Callback{} },
	TypeResult:          func() any { return &Result{} },
	TypeError:           func() any { return &ProtocolError{} },
	TypeSend:            func() any { return &Send{} },
	TypeEditCard:        func() any { return &EditCard{} },
	TypeAck:             func() any { return &Ack{} },
}

// KnownType reports whether t is a protocol v1 message type.
func KnownType(t string) bool { _, ok := payloadTypes[t]; return ok }

// DecodeEnvelope parses one text frame and checks the envelope fields. The
// payload is decoded separately by DecodePayload.
func DecodeEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return env, fmt.Errorf("not a JSON envelope: %w", err)
	}
	switch {
	case env.V != ProtocolVersion:
		return env, fmt.Errorf("unsupported protocol version %d", env.V)
	case env.ID == "":
		return env, fmt.Errorf("envelope.id missing")
	case env.Type == "":
		return env, fmt.Errorf("envelope.type missing")
	}
	if _, err := time.Parse(time.RFC3339Nano, env.TS); err != nil {
		return env, fmt.Errorf("envelope.ts is not an ISO-8601 time")
	}
	return env, nil
}

// DecodePayload decodes the envelope's payload into the struct for its type
// and validates it. Unknown fields are ignored on purpose: a peer that adds an
// optional field must not break us.
func DecodePayload(env Envelope) (any, error) {
	mk, ok := payloadTypes[env.Type]
	if !ok {
		return nil, fmt.Errorf("type %q is not part of protocol v1", env.Type)
	}
	if len(bytes.TrimSpace(env.Payload)) == 0 {
		return nil, fmt.Errorf("payload missing")
	}
	p := mk()
	if err := json.Unmarshal(env.Payload, p); err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	if v, ok := p.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// FormatTS renders a time the way the protocol wants it: UTC, milliseconds.
func FormatTS(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// NewEnvelope marshals payload into an envelope.
func NewEnvelope(id, typ string, ts time.Time, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{V: ProtocolVersion, ID: id, Type: typ, TS: FormatTS(ts), Payload: raw}, nil
}

/* ------------------------------------------------------------ validation -- */

var (
	conversationRe = regexp.MustCompile(`^(wa|tg|lz):.+$`)
	mediaKinds     = map[string]bool{"image": true, "video": true, "audio": true, "voice": true, "document": true, "sticker": true}
)

func (m Media) validate() error {
	if !mediaKinds[m.Kind] {
		return fmt.Errorf("media.kind %q is not valid", m.Kind)
	}
	if m.Mime == "" {
		return fmt.Errorf("media.mime missing")
	}
	if m.Size < 0 {
		return fmt.Errorf("media.size is negative")
	}
	return nil
}

func validateMedia(ms []Media) error {
	for _, m := range ms {
		if err := m.validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateButtons(b *Buttons) error {
	if b == nil {
		return nil
	}
	for _, row := range *b {
		for _, btn := range row {
			if btn.ID == "" || len(btn.ID) > 32 {
				return fmt.Errorf("button.id must be 1 to 32 characters")
			}
			if btn.Label == "" {
				return fmt.Errorf("button.label missing")
			}
		}
	}
	return nil
}

func validateCopyables(c *Copyables) error {
	if c == nil {
		return nil
	}
	if len(*c) > MaxCopyables {
		return fmt.Errorf("send.copyables holds more than %d items", MaxCopyables)
	}
	for _, cp := range *c {
		if cp.Label == "" {
			return fmt.Errorf("copyable.label missing")
		}
		if n := utf8.RuneCountInString(cp.Value); n == 0 || n > MaxCopyableValue {
			return fmt.Errorf("copyable.value must be 1 to %d characters", MaxCopyableValue)
		}
	}
	return nil
}

// validateLink needs an http(s) URL: a Telegram URL button rejects anything
// else, and WhatsApp only links those.
func validateLink(l *SendLink) error {
	if l == nil {
		return nil
	}
	if n := utf8.RuneCountInString(l.Label); n == 0 || n > MaxLinkLabel {
		return fmt.Errorf("link.label must be 1 to %d characters", MaxLinkLabel)
	}
	u, err := url.Parse(l.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("link.url is not an http(s) URL")
	}
	return nil
}

func validateISO(field, v string) error {
	if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
		return fmt.Errorf("%s is missing or not an ISO-8601 time", field)
	}
	return nil
}

func (s *Send) Validate() error {
	if !conversationRe.MatchString(s.Conversation) {
		return fmt.Errorf("send.conversation is not a channel-prefixed id")
	}
	switch s.Kind {
	case "reply", "note", "card":
	default:
		return fmt.Errorf("send.kind %q is not valid", s.Kind)
	}
	if s.Media == nil {
		return fmt.Errorf("send.media missing")
	}
	if err := validateMedia(s.Media); err != nil {
		return err
	}
	if err := validateButtons(s.Buttons); err != nil {
		return err
	}
	if err := validateCopyables(s.Copyables); err != nil {
		return err
	}
	if err := validateLink(s.Link); err != nil {
		return err
	}
	if utf8.RuneCountInString(s.Signature) > MaxSignature {
		return fmt.Errorf("send.signature is longer than %d characters", MaxSignature)
	}
	if utf8.RuneCountInString(s.Title) > MaxTitle {
		return fmt.Errorf("send.title is longer than %d characters", MaxTitle)
	}
	if s.Format != "" && s.Format != FormatHTML {
		return fmt.Errorf("send.format %q is not valid", s.Format)
	}
	return validateISO("send.expires_at", s.ExpiresAt)
}

func (e *EditCard) Validate() error {
	if !conversationRe.MatchString(e.Conversation) {
		return fmt.Errorf("edit_card.conversation is not a channel-prefixed id")
	}
	if e.CardID == "" {
		return fmt.Errorf("edit_card.card_id missing")
	}
	if err := validateButtons(e.Buttons); err != nil {
		return err
	}
	if utf8.RuneCountInString(e.Title) > MaxTitle {
		return fmt.Errorf("edit_card.title is longer than %d characters", MaxTitle)
	}
	return validateISO("edit_card.expires_at", e.ExpiresAt)
}

func (a *Ack) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("ack.id missing")
	}
	return nil
}

func (e *ProtocolError) Validate() error {
	if e.Code == "" {
		return fmt.Errorf("error.code missing")
	}
	return nil
}
