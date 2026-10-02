package agentlink

import (
	"context"
	"errors"
)

// What the Telegram customer bot's channel returns when a send fails in a way
// the Agent must tell apart (protocol §5 result errors). Anything else is an
// internal failure.
var (
	// ErrCustomerBlocked: the customer blocked the bot, or the chat is gone.
	ErrCustomerBlocked = errors.New("the customer blocked the bot or the chat no longer exists")
	// ErrCustomerRateLimited: Telegram answered 429.
	ErrCustomerRateLimited = errors.New("Telegram is rate limiting the customer bot")
)

// CustomerButton is one inline button under a customer message. Exactly one of
// CopyText and URL is set.
type CustomerButton struct {
	Text     string
	CopyText string
	URL      string
}

// CustomerText is a text message to a customer. HTML is already escaped and
// formatted: the channel sends it with HTML parse mode as it is.
type CustomerText struct {
	HTML    string
	Buttons [][]CustomerButton
	ReplyTo int64 // a message of the same chat to quote; 0 for none
}

// CustomerFile is a file to a customer. Kind is image, video, voice, audio,
// document or sticker.
type CustomerFile struct {
	Kind        string
	Data        []byte
	Filename    string
	Mime        string
	HTMLCaption string
	Buttons     [][]CustomerButton
	ReplyTo     int64
}

// CustomerChannel is the Telegram customer bot as the Executor and the hooks
// see it. Only the tgcustomer package implements it against Telegram.
type CustomerChannel interface {
	SendText(ctx context.Context, chatID int64, m CustomerText) (msgID int64, err error)
	SendFile(ctx context.Context, chatID int64, m CustomerFile) (msgID int64, err error)
	// RecordPair stores the message pair of a message in the customer's chat
	// and its twin in the topic, so quotes work in both directions.
	RecordPair(chatID, customerMsgID, threadID, topicMsgID int64) error
	// HubMsgIDOfTopicMsg returns the hub_msg_id of the customer-chat message
	// that a topic message mirrors, or "" when it is not paired.
	HubMsgIDOfTopicMsg(threadID, topicMsgID int64) string
}
