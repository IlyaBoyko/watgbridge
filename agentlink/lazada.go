package agentlink

// LazadaChannel is the Lazada IM channel as the link sees it. Only the
// lzcustomer package implements it. Sending to Lazada will add its methods
// here; for now the link only has to tell the channel to fetch.
type LazadaChannel interface {
	// Poll asks the channel to fetch new messages: one session, or every
	// session when sessionID is "". It must return at once and never block
	// the link's read loop; fetching happens in the channel's own goroutine.
	Poll(sessionID string)
}
