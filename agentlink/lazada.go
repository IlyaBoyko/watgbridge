package agentlink

import (
	"context"
	"fmt"
)

// LazadaChannel is the Lazada IM channel as the link sees it. Only the
// lzcustomer package implements it.
type LazadaChannel interface {
	// Poll asks the channel to fetch new messages: one session, or every
	// session when sessionID is "". It must return at once and never block
	// the link's read loop; fetching happens in the channel's own goroutine.
	Poll(sessionID string)

	// SendText delivers text to a Lazada session as one or more messages and
	// returns their Lazada message ids in order. origin is "agent" or "topic".
	//
	// Errors: ErrCustomerRateLimited and ErrCustomerBlocked (not authorized,
	// or the channel is not running) are matched with errors.Is; anything else
	// is an internal failure. When some parts went out before a later one
	// failed, the ids so far come back together with a *LazadaPartialError.
	SendText(ctx context.Context, sessionID, text, origin string) ([]string, error)
}

// LazadaPartialError is a send that stopped part-way: Sent of Total parts are
// on Lazada and the rest are not. SentText is what went out, so the caller can
// mirror exactly that. It unwraps to the failure of the part that did not go.
type LazadaPartialError struct {
	Sent, Total int
	SentText    string
	Err         error
}

func (e *LazadaPartialError) Error() string {
	return fmt.Sprintf("only %d of %d parts reached Lazada: %v", e.Sent, e.Total, e.Err)
}

func (e *LazadaPartialError) Unwrap() error { return e.Err }
