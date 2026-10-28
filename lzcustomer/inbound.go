package lzcustomer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"watgbridge/agentlink"

	"go.uber.org/zap"
)

// Lazada's template ids (Message.TemplateID).
const (
	tplText    = 1
	tplImage   = 3
	tplEmoji   = 4
	tplVideo   = 6
	tplItem    = 10006
	tplOrder   = 10007
	tplVoucher = 10008
)

// statusRecalled is Message.Status of a message the sender took back.
const statusRecalled = 1

const sellerMirrorPrefix = "🧑: You [Seller Center]\n"

// content is a message's content string parsed as far as it parses. Lazada's
// shapes for the non-text templates are not documented, so every access is
// lenient: a missing or odd field is just absent.
type content map[string]any

func parseContent(raw string) content {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber() // a long id must not be rounded through a float
	var c content
	if err := dec.Decode(&c); err != nil {
		return nil
	}
	return c
}

// str returns the first of keys that holds a non-empty string or a number.
func (c content) str(keys ...string) string {
	for _, k := range keys {
		switch v := c[k].(type) {
		case string:
			if t := strings.TrimSpace(v); t != "" {
				return t
			}
		case json.Number:
			return v.String()
		}
	}
	return ""
}

func (c content) keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// logKeys records, once per template id and process, which fields a non-text
// message's content has. Names only, never values: this is how the real shapes
// get confirmed from live traffic.
func (s *Service) logKeys(template int, c content) {
	if _, seen := s.keysLogged.LoadOrStore(template, true); seen {
		return
	}
	s.log.Info("lazada: content keys for template " + strconv.Itoa(template) + ": " + fmt.Sprint(c.keys()))
}

// rendered is a message reduced to what the topic and the Agent get.
type rendered struct {
	text string
	// image is a downloaded picture; nil for every other kind of message.
	image []byte
	mime  string
}

// describe is the text form of a message for places that show no pictures (the
// context note).
func (s *Service) describe(m Message) string {
	return s.render(nil, m, false).text
}

// render turns a message into text and, for a picture, the downloaded file.
// ctx may be nil with fetch false.
func (s *Service) render(ctx context.Context, m Message, fetch bool) rendered {
	tpl := int(m.TemplateID)
	c := parseContent(string(m.Content))
	if tpl != tplText {
		s.logKeys(tpl, c)
	}
	switch tpl {
	case tplText:
		text := c.str("txt")
		if text == "" && c == nil && !strings.HasPrefix(strings.TrimSpace(string(m.Content)), "{") {
			text = strings.TrimSpace(string(m.Content)) // plain text, not JSON
		}
		if text == "" {
			return rendered{text: "[message type 1]"}
		}
		return rendered{text: text}
	case tplEmoji:
		if e := c.str("txt", "emoji", "text"); e != "" {
			return rendered{text: e}
		}
		return rendered{text: "[emoji]"}
	case tplImage:
		url := c.str("imgUrl", "img_url", "url", "originUrl")
		if url == "" || !fetch {
			return rendered{text: "[image]"}
		}
		data, mime, err := s.o.Images.Fetch(ctx, url)
		if err != nil || len(data) == 0 {
			// The URL is not logged: it can carry a token.
			s.log.Warn("lazada: could not download a picture", zap.Error(err))
			return rendered{text: "[image]"}
		}
		return rendered{image: data, mime: mime}
	case tplVideo:
		return rendered{text: "[video]"}
	case tplItem:
		out := "[product card"
		if title := c.str("title", "itemTitle", "name"); title != "" {
			out += ": " + title
		}
		if id := c.str("itemId", "item_id"); id != "" {
			out += " (item " + id + ")"
		}
		return rendered{text: out + "]"}
	case tplOrder:
		if id := c.str("orderId", "order_id", "tradeOrderId"); id != "" {
			return rendered{text: "[order card: " + id + "]"}
		}
		return rendered{text: "[order card]"}
	case tplVoucher:
		return rendered{text: "[voucher]"}
	}
	return rendered{text: "[message type " + strconv.Itoa(tpl) + "]"}
}

func imageExt(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	}
	return "jpg"
}

// media is the protocol's media item for a downloaded picture. One over the
// inline limit is described but not carried (protocol section 6).
func (r rendered) media() []agentlink.Media {
	if r.image == nil {
		return nil
	}
	m := agentlink.Media{Kind: "image", Mime: r.mime, Filename: "image." + imageExt(r.mime), Size: int64(len(r.image))}
	if m.Size > agentlink.MaxInlineMediaBytes {
		m.TooLarge = true
	} else {
		m.Data = base64.StdEncoding.EncodeToString(r.image)
	}
	return []agentlink.Media{m}
}

// advance records that a message is done, so a crash resumes after it.
func (s *Service) advance(sc *sessionState, m Message, posted bool) error {
	sc.has = true
	sc.cursor.LastMessageID = string(m.MessageID)
	if t := int64(m.SendTime); t > sc.cursor.LastSendTime {
		sc.cursor.LastSendTime = t
	}
	sc.cursor.Title = sc.title
	if posted {
		sc.cursor.PostedMessageID = string(m.MessageID)
	}
	return s.store.saveCursor(sc.cursor)
}

// handle bridges one message: into the topic, and to the Agent where it is
// speech. An error stops the batch at this message, and the cursor stays in
// front of it.
func (s *Service) handle(ctx context.Context, sc *sessionState, m Message) error {
	id := string(m.MessageID)
	// The topic post is made at most once, even if the event write failed on a
	// previous attempt.
	alreadyPosted := sc.cursor.PostedMessageID == id

	post := func(p TopicPost) error {
		if alreadyPosted {
			return nil
		}
		thread, err := s.topicFor(ctx, sc)
		if err != nil {
			return err
		}
		_, err = s.o.Topics.Post(ctx, thread, p)
		if err != nil {
			s.log.Warn("lazada: could not post into the topic", zap.String("kind", p.Kind), zap.Error(err))
		}
		return err
	}
	note := func(text string) { _ = post(TopicPost{Kind: "text", Prefix: notePrefix, Text: text}) }

	// Everything below needs the topic. Creating it can fail, and then nothing
	// may be lost: the batch stops at this message and Lazada still has it.
	needTopic := func() error {
		_, err := s.topicFor(ctx, sc)
		return err
	}

	switch {
	case int64(m.Type) == TypeSystem:
		if err := needTopic(); err != nil {
			return err
		}
		text := parseContent(string(m.Content)).str("txt", "text", "content")
		if text == "" {
			text = "[Lazada system message]"
		}
		note(text)
		return s.advance(sc, m, true)

	case int64(m.FromAccountType) == AccountBuyer:
		if err := needTopic(); err != nil {
			return err
		}
		if int64(m.Status) == statusRecalled {
			note("The buyer recalled a message.")
			return s.advance(sc, m, true)
		}
		r := s.render(ctx, m, true)
		s.postRendered(post, "", r, s.o.BoldCustomer)
		if err := s.markPosted(sc, m); err != nil {
			return err
		}
		err := s.o.Events.Customer(agentlink.CustomerInput{
			ChatKey: sc.key, TopicID: sc.thread, HubMsgID: id, ContactName: sc.buyerName(),
			Text: r.text, Media: r.media(),
		})
		if err != nil {
			return fmt.Errorf("could not queue the buyer message: %w", err)
		}
		return s.advance(sc, m, true)

	case int64(m.FromAccountType) == AccountSeller:
		return s.handleSeller(ctx, sc, m, post, note, needTopic)
	}

	s.log.Debug("lazada: message from an unknown account type skipped", zap.String("message_id", id))
	return s.advance(sc, m, false)
}

// postRendered posts a message's words or picture. A picture Telegram refuses
// goes up as a file instead.
func (s *Service) postRendered(post func(TopicPost) error, prefix string, r rendered, bold bool) {
	if r.image == nil {
		_ = post(TopicPost{Kind: "text", Prefix: prefix, Text: r.text, Bold: bold})
		return
	}
	p := TopicPost{Kind: "photo", Prefix: prefix, Data: r.image, Mime: r.mime, Filename: "image." + imageExt(r.mime)}
	if post(p) != nil {
		p.Kind = "document"
		_ = post(p)
	}
}

func (s *Service) handleSeller(ctx context.Context, sc *sessionState, m Message,
	post func(TopicPost) error, note func(string), needTopic func() error) error {
	id := string(m.MessageID)
	warn := func() {
		if m.ProcessMsg != "" {
			post(TopicPost{Kind: "text", Text: "⚠️ Lazada blocked this message: " + string(m.ProcessMsg)})
		}
	}

	sent, err := s.store.WasSent(sc.id, id)
	if err != nil {
		return err
	}
	if sent {
		// Mirrored and reported when the Hub sent it. Only Lazada's verdict is new.
		if m.ProcessMsg != "" {
			if err := needTopic(); err != nil {
				return err
			}
			warn()
		}
		return s.advance(sc, m, m.ProcessMsg != "")
	}

	if int64(m.Status) == statusRecalled {
		return s.advance(sc, m, false) // staff took it back; nothing to report
	}
	if err := needTopic(); err != nil {
		return err
	}

	if bool(m.AutoReply) {
		text := s.render(ctx, m, false).text
		note("Lazada auto-reply: " + text)
		return s.advance(sc, m, true)
	}

	// Staff answered from Seller Center or the seller app: the same case as a
	// reply typed on the WhatsApp phone.
	r := s.render(ctx, m, true)
	s.postRendered(post, sellerMirrorPrefix, r, false)
	warn()
	if err := s.markPosted(sc, m); err != nil {
		return err
	}
	err = s.o.Events.Staff(agentlink.StaffInput{
		ChatKey: sc.key, TopicID: sc.thread, HubMsgID: id, Source: "phone",
		Text: r.text, Media: r.media(),
	})
	if err != nil {
		return fmt.Errorf("could not queue the staff message: %w", err)
	}
	return s.advance(sc, m, true)
}

// markPosted records that a message's topic posts are made, before its event
// is written: if that write fails, the retry must not post again.
func (s *Service) markPosted(sc *sessionState, m Message) error {
	sc.cursor.PostedMessageID = string(m.MessageID)
	return s.store.saveCursor(sc.cursor)
}
