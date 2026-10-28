// Package lzfake is an in-memory stand-in for the Lazada Open Platform: the
// four IM paths and both auth paths, over a real HTTP listener. It is a
// non-test package so the channel's tests can use it too.
//
// It behaves like Lazada where the client can notice: every request's
// signature is checked, access tokens must have been issued (and not be
// expired), lists page newest-first with start_time meaning "older than this",
// and numbers and booleans in the IM answers are JSON strings.
package lzfake

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"watgbridge/lzcustomer"
)

// Request is one call the server received.
type Request struct {
	Method      string
	Path        string // without the /rest prefix, e.g. /im/message/send
	Query       url.Values
	Form        url.Values // the POST body
	ContentType string
}

// Param returns a parameter from the query string or the form body.
func (r Request) Param(name string) string {
	if v := r.Form.Get(name); v != "" {
		return v
	}
	return r.Query.Get(name)
}

// Sent is a message the seller side sent through /im/message/send.
type Sent struct {
	SessionID  string
	Text       string
	MessageID  string
	TemplateID string
}

// Read is a /im/session/read call.
type Read struct {
	SessionID         string
	LastReadMessageID string
}

// SellerOpts tunes a message added with AddSellerMessage.
type SellerOpts struct {
	AutoReply  bool
	ProcessMsg string // non-empty = Lazada's filter blocked the message
	System     bool   // type 2, a system message
}

type session struct {
	id, buyerID, title string
	unread             int
	lastMsgID          string
	lastTime           int64
	summary            string
}

type message struct {
	id, sessionID string
	from          int
	template      int
	content       string
	sendTime      int64
	typ           int
	autoReply     bool
	processMsg    string
	fromID, toID  string
	fromTy, toTy  int
	status        int // 1 = recalled
}

// Server is the fake. Create it with New and Close it when done.
type Server struct {
	srv         *httptest.Server
	key, secret string

	mu          sync.Mutex
	now         func() time.Time
	sellerID    string
	accessLife  time.Duration
	refreshLife time.Duration
	codes       map[string]bool
	access      map[string]time.Time
	refresh     map[string]time.Time
	seq         int
	lastTime    int64
	sessions    map[string]*session
	messages    map[string][]*message // per session, oldest first
	sent        []Sent
	reads       []Read
	requests    []Request
	failCodes   map[string][]string
	failHTTP    map[string][]int
	blockNext   []string
}

// New starts a fake for one app key and secret.
func New(appKey, appSecret string) *Server {
	s := &Server{
		key: appKey, secret: appSecret,
		now:         time.Now,
		sellerID:    "100094063",
		accessLife:  7 * 24 * time.Hour,
		refreshLife: 30 * 24 * time.Hour,
		codes:       map[string]bool{},
		access:      map[string]time.Time{},
		refresh:     map[string]time.Time{},
		sessions:    map[string]*session{},
		messages:    map[string][]*message{},
		failCodes:   map[string][]string{},
		failHTTP:    map[string][]int{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *Server) Close() { s.srv.Close() }

// URL is the base for both the API and the auth host, shaped like the real
// ".../rest" bases.
func (s *Server) URL() string { return s.srv.URL + "/rest" }

// HTTPClient is a client for the fake's listener.
func (s *Server) HTTPClient() *http.Client { return s.srv.Client() }

// SetNow makes the fake judge token expiry and stamp messages with c.
func (s *Server) SetNow(c func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = c
}

// SetSellerID changes the seller id the fake reports in token answers and
// uses as the seller side of every chat.
func (s *Server) SetSellerID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sellerID = id
}

// SetLifetimes sets how long tokens issued from now on live.
func (s *Server) SetLifetimes(access, refresh time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessLife, s.refreshLife = access, refresh
}

// AllowCode makes code exchangeable once at /auth/token/create.
func (s *Server) AllowCode(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code] = true
}

// IssueToken creates a token pair directly, for tests that don't go through
// the auth calls.
func (s *Server) IssueToken() (access, refreshTok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issueLocked()
}

// RevokeTokens forgets every token, as if the owner removed the authorization.
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.access = map[string]time.Time{}
	s.refresh = map[string]time.Time{}
}

func (s *Server) issueLocked() (string, string) {
	s.seq++
	at, rt := "at-"+strconv.Itoa(s.seq), "rt-"+strconv.Itoa(s.seq)
	s.access[at] = s.now().Add(s.accessLife)
	s.refresh[rt] = s.now().Add(s.refreshLife)
	return at, rt
}

// FailNext makes the next call to path (e.g. "/im/message/send") answer with
// the error code (e.g. "IllegalAccessToken", "ApiCallLimit"). Calls queue.
func (s *Server) FailNext(path, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCodes[path] = append(s.failCodes[path], code)
}

// FailNextHTTP makes the next call to path answer with just an HTTP status.
func (s *Server) FailNextHTTP(path string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failHTTP[path] = append(s.failHTTP[path], status)
}

// BlockNextSend makes the next /im/message/send store its message with
// process_msg set, as Lazada's security filter does.
func (s *Server) BlockNextSend(processMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockNext = append(s.blockNext, processMsg)
}

// Requests returns every call received, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Calls counts the calls received at path.
func (s *Server) Calls(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r.Path == path {
			n++
		}
	}
	return n
}

// Sent returns what was sent through /im/message/send.
func (s *Server) Sent() []Sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sent(nil), s.sent...)
}

// Reads returns the /im/session/read calls.
func (s *Server) Reads() []Read {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Read(nil), s.reads...)
}

// TextContent is the content string of a text message (template 1).
func TextContent(txt string) string {
	b, _ := json.Marshal(map[string]any{"activeContent": map[string]any{}, "txt": txt})
	return string(b)
}

// AddSession creates a chat. Adding messages to an unknown session creates it
// too, with a made-up buyer.
func (s *Server) AddSession(id, buyerID, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionLocked(id, buyerID, title)
}

func (s *Server) sessionLocked(id, buyerID, title string) *session {
	if ss, ok := s.sessions[id]; ok {
		return ss
	}
	if buyerID == "" {
		buyerID = "buyer-" + id
	}
	if title == "" {
		title = "Buyer " + id
	}
	ss := &session{id: id, buyerID: buyerID, title: title, lastTime: s.stampLocked()}
	s.sessions[id] = ss
	return ss
}

// stampLocked returns a strictly increasing Unix ms time.
func (s *Server) stampLocked() int64 {
	t := s.now().UnixMilli()
	if t <= s.lastTime {
		t = s.lastTime + 1
	}
	s.lastTime = t
	return t
}

// AddBuyerMessage adds a message from the buyer and returns its id. content is
// the raw content string, whose shape depends on templateID (see TextContent).
func (s *Server) AddBuyerMessage(sessionID string, templateID int, content string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addLocked(sessionID, lzcustomer.AccountBuyer, templateID, content, SellerOpts{})
}

// AddSellerMessage adds a message from the seller side that did not go
// through the Hub, as if staff typed it in Seller Center.
func (s *Server) AddSellerMessage(sessionID string, templateID int, content string, o SellerOpts) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addLocked(sessionID, lzcustomer.AccountSeller, templateID, content, o)
}

func (s *Server) addLocked(sessionID string, from, templateID int, content string, o SellerOpts) string {
	ss := s.sessionLocked(sessionID, "", "")
	s.seq++
	m := &message{
		id: "m" + strconv.Itoa(s.seq), sessionID: sessionID, from: from, template: templateID,
		content: content, sendTime: s.stampLocked(), typ: lzcustomer.TypeUser,
		autoReply: o.AutoReply, processMsg: o.ProcessMsg,
	}
	if o.System {
		m.typ = lzcustomer.TypeSystem
	}
	if from == lzcustomer.AccountBuyer {
		m.fromID, m.toID, m.fromTy, m.toTy = ss.buyerID, s.sellerID, 1, 2
		ss.unread++
	} else {
		m.fromID, m.toID, m.fromTy, m.toTy = s.sellerID, ss.buyerID, 2, 1
	}
	s.messages[sessionID] = append(s.messages[sessionID], m)
	ss.lastMsgID, ss.lastTime, ss.summary = m.id, m.sendTime, summaryOf(m)
	return m.id
}

func summaryOf(m *message) string {
	if m.template == 1 {
		var c struct {
			Txt string `json:"txt"`
		}
		if json.Unmarshal([]byte(m.content), &c) == nil && c.Txt != "" {
			return c.Txt
		}
	}
	return "[template " + strconv.Itoa(m.template) + "]"
}

/* ---------------------------------------------------------------- handler -- */

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	path := strings.TrimPrefix(r.URL.Path, "/rest")

	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests = append(s.requests, Request{
		Method: r.Method, Path: path, Query: r.URL.Query(), Form: r.PostForm,
		ContentType: r.Header.Get("Content-Type"),
	})

	if q := s.failHTTP[path]; len(q) > 0 {
		s.failHTTP[path] = q[1:]
		w.WriteHeader(q[0])
		_, _ = w.Write([]byte(`{}`))
		return
	}

	params := map[string]string{}
	for k := range r.Form {
		params[k] = r.Form.Get(k)
	}
	if params["app_key"] != s.key {
		s.errorLocked(w, "InvalidAppKey", "The specified app key is invalid")
		return
	}
	if params["sign"] != lzcustomer.Sign(s.secret, path, params) {
		s.errorLocked(w, "IncompleteSignature", "The request signature does not conform to Lazada standards")
		return
	}
	if params["timestamp"] == "" {
		s.errorLocked(w, "MissingParameter", "timestamp")
		return
	}

	isAuth := strings.HasPrefix(path, "/auth/")
	if !isAuth {
		exp, ok := s.access[params["access_token"]]
		if !ok || !s.now().Before(exp) {
			s.errorLocked(w, "IllegalAccessToken", "The specified access token is invalid or expired")
			return
		}
	}

	if q := s.failCodes[path]; len(q) > 0 {
		s.failCodes[path] = q[1:]
		s.errorLocked(w, q[0], "injected by lzfake")
		return
	}

	switch path {
	case "/auth/token/create":
		s.tokenCreate(w, params)
	case "/auth/token/refresh":
		s.tokenRefresh(w, params)
	case "/im/session/list":
		s.sessionList(w, params)
	case "/im/message/list":
		s.messageList(w, params)
	case "/im/message/send":
		s.messageSend(w, params)
	case "/im/session/read":
		s.sessionRead(w, params)
	default:
		s.errorLocked(w, "InvalidApiPath", "The api path is not supported by lzfake")
	}
}

func (s *Server) errorLocked(w http.ResponseWriter, code, message string) {
	writeJSON(w, map[string]any{
		"code": code, "type": "ISP", "message": message, "request_id": "fake-request",
	})
}

// okLocked writes a success in the real format: everything is a string.
func (s *Server) okLocked(w http.ResponseWriter, data map[string]any) {
	body := map[string]any{
		"code": "0", "success": "true", "err_code": "0", "err_message": "null",
		"request_id": "fake-request",
	}
	if data != nil {
		body["data"] = data
	}
	writeJSON(w, body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

/* ------------------------------------------------------------------- auth -- */

// tokenAnswer is shaped like the real token answers: fields at the top level,
// lifetimes as numbers, and an extra country listed first so that picking the
// seller of the token's own country is exercised.
func (s *Server) tokenAnswer(w http.ResponseWriter, at, rt string) {
	writeJSON(w, map[string]any{
		"code": "0", "request_id": "fake-request",
		"access_token": at, "refresh_token": rt,
		"expires_in":         int64(s.accessLife / time.Second),
		"refresh_expires_in": int64(s.refreshLife / time.Second),
		"country":            "my",
		"account":            "seller@example.invalid",
		"account_platform":   "seller_center",
		"country_user_info": []map[string]string{
			{"country": "SG", "user_id": "sg-user", "seller_id": "sg-seller", "short_code": "SGAAA"},
			{"country": "MY", "user_id": "my-user", "seller_id": s.sellerID, "short_code": "MYAAA"},
		},
	})
}

func (s *Server) tokenCreate(w http.ResponseWriter, p map[string]string) {
	code := p["code"]
	if !s.codes[code] {
		s.errorLocked(w, "InvalidCode", "The authorization code is invalid or expired")
		return
	}
	delete(s.codes, code)
	at, rt := s.issueLocked()
	s.tokenAnswer(w, at, rt)
}

func (s *Server) tokenRefresh(w http.ResponseWriter, p map[string]string) {
	exp, ok := s.refresh[p["refresh_token"]]
	if !ok || !s.now().Before(exp) {
		s.errorLocked(w, "IllegalRefreshToken", "The specified refresh token is invalid or expired")
		return
	}
	delete(s.refresh, p["refresh_token"])
	at, rt := s.issueLocked()
	s.tokenAnswer(w, at, rt)
}

/* --------------------------------------------------------------------- IM -- */

func pageSizeOf(p map[string]string) (int, bool) {
	n, err := strconv.Atoi(p["page_size"])
	return n, err == nil && n > 0
}

// startTimeOf reads start_time. A first page asks for "now"; message times are
// bumped to stay strictly increasing and can sit a few ms ahead of the clock,
// so any start_time at or after the fake's now means "from the newest".
func (s *Server) startTimeOf(p map[string]string) (int64, bool) {
	n, err := strconv.ParseInt(p["start_time"], 10, 64)
	if err == nil && n >= s.now().UnixMilli() {
		n = math.MaxInt64
	}
	return n, err == nil
}

// olderThan reports whether an item stamped t belongs on the page that starts
// at start. Without a cursor id, start_time is exclusive (older than this); with
// one, the id's position already excluded what was served, so equal times may
// still follow.
func olderThan(t, start int64, hasCursor bool) bool {
	if hasCursor {
		return t <= start
	}
	return t < start
}

func (s *Server) sessionList(w http.ResponseWriter, p map[string]string) {
	size, ok := pageSizeOf(p)
	start, ok2 := s.startTimeOf(p)
	if !ok || !ok2 {
		s.errorLocked(w, "MissingParameter", "start_time and page_size are required")
		return
	}
	all := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		all = append(all, ss)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].lastTime != all[j].lastTime {
			return all[i].lastTime > all[j].lastTime
		}
		return all[i].id > all[j].id
	})
	if last := p["last_session_id"]; last != "" {
		for i, ss := range all {
			if ss.id == last {
				all = all[i+1:]
				break
			}
		}
	}
	var cand []*session
	for _, ss := range all {
		if olderThan(ss.lastTime, start, p["last_session_id"] != "") {
			cand = append(cand, ss)
		}
	}
	page := cand
	if len(page) > size {
		page = page[:size]
	}
	list := make([]map[string]any, 0, len(page))
	for _, ss := range page {
		list = append(list, map[string]any{
			"session_id": ss.id, "buyer_id": ss.buyerID, "title": ss.title,
			"head_url": "https://example.invalid/head.png", "summary": ss.summary,
			"unread_count": strconv.Itoa(ss.unread), "last_message_id": ss.lastMsgID,
			"last_message_time": strconv.FormatInt(ss.lastTime, 10),
			"self_position":     "1", "to_position": "2", "site_id": "MY",
		})
	}
	next, last := s.startStr(start), p["last_session_id"]
	if n := len(page); n > 0 {
		next, last = strconv.FormatInt(page[n-1].lastTime, 10), page[n-1].id
	}
	s.okLocked(w, map[string]any{
		"session_list": list, "has_more": strconv.FormatBool(len(cand) > len(page)),
		"next_start_time": next, "last_session_id": last,
	})
}

func (s *Server) messageList(w http.ResponseWriter, p map[string]string) {
	size, ok := pageSizeOf(p)
	start, ok2 := s.startTimeOf(p)
	id := p["session_id"]
	if !ok || !ok2 || id == "" {
		s.errorLocked(w, "MissingParameter", "session_id, start_time and page_size are required")
		return
	}
	if _, known := s.sessions[id]; !known {
		s.errorLocked(w, "InvalidParameter", "unknown session")
		return
	}
	stored := s.messages[id]
	all := make([]*message, 0, len(stored))
	for i := len(stored) - 1; i >= 0; i-- { // newest first
		all = append(all, stored[i])
	}
	if last := p["last_message_id"]; last != "" {
		for i, m := range all {
			if m.id == last {
				all = all[i+1:]
				break
			}
		}
	}
	var cand []*message
	for _, m := range all {
		if olderThan(m.sendTime, start, p["last_message_id"] != "") {
			cand = append(cand, m)
		}
	}
	page := cand
	if len(page) > size {
		page = page[:size]
	}
	list := make([]map[string]any, 0, len(page))
	for _, m := range page {
		e := map[string]any{
			"message_id": m.id, "session_id": m.sessionID,
			"from_account_type": strconv.Itoa(m.fromTy), "to_account_type": strconv.Itoa(m.toTy),
			"from_account_id": m.fromID, "to_account_id": m.toID,
			"template_id": strconv.Itoa(m.template), "type": strconv.Itoa(m.typ),
			"content": m.content, "send_time": strconv.FormatInt(m.sendTime, 10),
			"status": strconv.Itoa(m.status), "auto_reply": strconv.FormatBool(m.autoReply), "site_id": "MY",
		}
		if m.processMsg != "" {
			e["process_msg"] = m.processMsg
		}
		list = append(list, e)
	}
	next, last := s.startStr(start), p["last_message_id"]
	if n := len(page); n > 0 {
		next, last = strconv.FormatInt(page[n-1].sendTime, 10), page[n-1].id
	}
	s.okLocked(w, map[string]any{
		"message_list": list, "has_more": strconv.FormatBool(len(cand) > len(page)),
		"next_start_time": next, "last_message_id": last,
	})
}

func (s *Server) messageSend(w http.ResponseWriter, p map[string]string) {
	id, txt := p["session_id"], p["txt"]
	if _, known := s.sessions[id]; !known || txt == "" || p["template_id"] != "1" {
		s.errorLocked(w, "InvalidParameter", "session_id, template_id=1 and txt are required")
		return
	}
	var o SellerOpts
	if len(s.blockNext) > 0 {
		o.ProcessMsg, s.blockNext = s.blockNext[0], s.blockNext[1:]
	}
	msgID := s.addLocked(id, lzcustomer.AccountSeller, 1, TextContent(txt), o)
	s.sent = append(s.sent, Sent{SessionID: id, Text: txt, MessageID: msgID, TemplateID: p["template_id"]})
	s.okLocked(w, map[string]any{
		"message_id": msgID, "template_id": "1",
		"current_time": strconv.FormatInt(s.messages[id][len(s.messages[id])-1].sendTime, 10),
	})
}

func (s *Server) sessionRead(w http.ResponseWriter, p map[string]string) {
	ss, known := s.sessions[p["session_id"]]
	if !known || p["last_read_message_id"] == "" {
		s.errorLocked(w, "InvalidParameter", "session_id and last_read_message_id are required")
		return
	}
	ss.unread = 0
	s.reads = append(s.reads, Read{SessionID: ss.id, LastReadMessageID: p["last_read_message_id"]})
	s.okLocked(w, nil)
}

// startStr is next_start_time for an empty page: the start it was asked for,
// or the clock when that was "from the newest".
func (s *Server) startStr(start int64) string {
	if start == math.MaxInt64 {
		start = s.now().UnixMilli()
	}
	return strconv.FormatInt(start, 10)
}

// RecallMessage marks a message as recalled (status 1), as when its sender
// takes it back. It reports whether the message exists.
func (s *Server) RecallMessage(sessionID, messageID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.messages[sessionID] {
		if m.id == messageID {
			m.status = 1
			return true
		}
	}
	return false
}
