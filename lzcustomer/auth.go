package lzcustomer

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	// authCommand is the owner's command that connects Lazada.
	authCommand = "/lazada_auth"
	// expiryWarningWindow is how long before the refresh token lapses the
	// daily warning starts.
	expiryWarningWindow = 14 * 24 * time.Hour
	authTimeout         = 30 * time.Second
	dateFormat          = "2006-01-02"
	// timeFormat is what the messages show: the token lapses at a moment, and "today"
	// is not enough to know whether it is before or after tomorrow's work starts.
	timeFormat = "2006-01-02 15:04 MST"
)

// ParseAuthCommand recognises "/lazada_auth" (also "/lazada_auth@bot") at the
// start of a message and returns what follows it.
func ParseAuthCommand(text string) (args string, ok bool) {
	rest, ok := strings.CutPrefix(text, authCommand)
	if !ok {
		return "", false
	}
	if rest == "" {
		return "", true
	}
	switch rest[0] {
	case ' ', '\t', '\n', '\r', '@':
	default:
		return "", false // /lazada_authx is another command
	}
	if rest[0] == '@' {
		end := strings.IndexAny(rest, " \t\r\n")
		if end < 0 {
			return "", true
		}
		rest = rest[end:]
	}
	return strings.TrimSpace(rest), true
}

// authLink is where the owner approves the app. Lazada then redirects to the
// callback URL with ?code=.
func (s *Service) authLink() string {
	return "https://auth.lazada.com/oauth/authorize?response_type=code&force_auth=true&redirect_uri=" +
		url.QueryEscape(s.o.CallbackURL) + "&client_id=" + url.QueryEscape(s.o.AppKey)
}

// AuthRequest is a /lazada_auth message, reduced to what the handler needs.
type AuthRequest struct {
	FromID int64
	Thread int64 // the topic it was sent in, 0 for General
	MsgID  int64
	Args   string // the code, or "" for the link
}

// NotOwnerReply is what everyone but the owner gets.
const NotOwnerReply = "Only the owner can authorize Lazada."

func (s *Service) reply(ctx context.Context, thread int64, text string) {
	if _, err := s.o.Topics.Post(ctx, thread, TopicPost{Kind: "text", Text: text}); err != nil {
		s.log.Warn("lazada: could not answer /lazada_auth", zap.Error(err))
	}
}

// HandleAuth is /lazada_auth. Only the owner may use it. Without a code it
// gives the authorization link. With one it deletes the command message (it
// holds the code) before doing anything else, exchanges the code, and starts
// a full fetch. The code is never echoed or logged.
func (s *Service) HandleAuth(ctx context.Context, req AuthRequest) {
	if req.FromID != s.o.OwnerID {
		s.reply(ctx, req.Thread, NotOwnerReply)
		return
	}
	if req.Args == "" {
		s.reply(ctx, req.Thread, "Open this link and approve the app: "+s.authLink()+
			"\n\nApprove, then copy the code= value from the address bar and send /lazada_auth <code> within 30 minutes.")
		return
	}

	if err := s.o.Topics.Delete(ctx, req.MsgID); err != nil {
		s.log.Warn("lazada: could not delete the /lazada_auth message", zap.Error(err))
	}

	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	got, err := s.o.Auth.Authorize(ctx, req.Args)
	if err != nil {
		s.reply(ctx, req.Thread, s.authFailure(err))
		return
	}

	account := got.Account
	if account == "" {
		account = "(not given)"
	}
	s.reply(ctx, req.Thread, "Lazada authorized: seller "+got.SellerID+" ("+got.Country+"), account "+account+
		". Token valid until "+s.date(got.AccessExpiresAt)+"; re-authorize by "+s.date(got.RefreshExpiresAt)+
		". Put seller_id "+got.SellerID+" in the agent config (markets.my.lazada.sellerId).")

	s.blocked.Store(false)
	s.queue("") // a full fetch
}

// authFailure words an error in plain terms. The error text itself is not
// shown: it is Lazada's, and the code is not to be repeated anywhere.
func (s *Service) authFailure(err error) string {
	var api *APIError
	switch {
	case errors.Is(err, ErrAuth):
		return "That code was not accepted (expired or already used). Get a new one: " + s.authLink()
	case errors.Is(err, ErrRateLimited):
		return "Lazada is limiting requests right now. Wait a minute and send the code again."
	case errors.As(err, &api):
		return "Lazada refused that code (" + api.Code + "). Get a new one: " + s.authLink()
	}
	s.log.Warn("lazada: authorization failed", zap.Error(err))
	return "Could not reach Lazada to check that code. Try again; if it keeps failing, get a new code: " + s.authLink()
}

func (s *Service) date(t time.Time) string {
	if t.IsZero() {
		return "(unknown)"
	}
	return t.In(s.loc).Format(timeFormat)
}

// checkExpiry posts, once per local day, that the refresh token lapses soon.
func (s *Service) checkExpiry(ctx context.Context) {
	st, ok := s.o.Auth.Status()
	if !ok || st.RefreshExpiresAt.IsZero() {
		return
	}
	now := s.clock.Now()
	if st.RefreshExpiresAt.Sub(now) > expiryWarningWindow {
		return
	}
	today := now.In(s.loc).Format(dateFormat)
	meta, _, err := s.store.meta(now.UnixMilli())
	if err != nil {
		s.log.Warn("lazada: could not read the expiry warning state", zap.Error(err))
		return
	}
	if meta.LastExpiryWarningDay == today {
		return
	}
	text := "Lazada authorization expires on " + s.date(st.RefreshExpiresAt) + ". Re-authorize: " + s.authLink() +
		", then /lazada_auth <code>."
	if _, err := s.o.Topics.Post(ctx, 0, TopicPost{Kind: "text", Text: text}); err != nil {
		s.log.Warn("lazada: could not post the expiry warning", zap.Error(err))
		return
	}
	meta.LastExpiryWarningDay = today
	if err := s.store.saveMeta(meta); err != nil {
		s.log.Warn("lazada: could not store the expiry warning day", zap.Error(err))
	}
}
