package agentlink

import (
	"context"
	"errors"
	"html"
	"strconv"
	"strings"
	"unicode/utf16"

	"go.uber.org/zap"
)

// Telegram's limits for what one message may hold, counted in UTF-16 code
// units after formatting is parsed.
const (
	tgCaptionLimit = 1024
	tgTextLimit    = 4096
)

func tgLen(s string) int { return len(utf16.Encode([]rune(s))) }

// tgMessage is one message the customer bot sends for a `reply`.
type tgMessage struct {
	file    *OutMedia // nil for a text message
	html    string    // the text, or the file's caption
	buttons [][]CustomerButton
	// body and lines are what the topic mirror shows of this message: the
	// reply's own text or caption, and the copyables and link as extra lines.
	body  string
	lines []string
	// formatted says html and body hold the Agent's own Telegram HTML (protocol
	// section 5c) instead of escaped plain text.
	formatted bool
}

// planTelegramReply decides which messages a reply becomes (protocol §5):
//
//   - copyables are appended to the text as `label: <code>value</code>` lines,
//     and every other character is HTML-escaped;
//   - one copy_text button per copyable, then the link in a row of its own;
//   - media carries the text as its caption, unless that would pass Telegram's
//     caption limit: then the media goes first and the text follows as a second
//     message, which also holds the buttons that belong to it.
//
// With formatted set, text is the Agent's own Telegram HTML (protocol §5c) and
// goes out as given; limits are measured on what Telegram will show of it.
//
// It reports false when the text cannot fit in a message at all.
func planTelegramReply(text string, formatted bool, media []OutMedia, copyables []Copyable, link *SendLink) ([]tgMessage, bool) {
	textHTML, textPlain := html.EscapeString(text), text
	if formatted {
		textHTML, textPlain = text, StripHTML(text)
	}
	var kb [][]CustomerButton
	for _, c := range copyables {
		textHTML = appendLine(textHTML, html.EscapeString(c.Label)+": <code>"+html.EscapeString(c.Value)+"</code>")
		textPlain = appendLine(textPlain, c.Label+": "+c.Value)
		kb = append(kb, []CustomerButton{{Text: "Copy " + c.Label, CopyText: c.Value}})
	}
	if link != nil {
		kb = append(kb, []CustomerButton{{Text: link.Label, URL: link.URL}})
	}
	// The mirror lists the copyables and the link on the message that carries
	// the keyboard.
	lines := extraLines(copyables, link)

	if len(media) == 0 {
		if tgLen(textPlain) > tgTextLimit {
			return nil, false
		}
		return []tgMessage{{html: textHTML, body: text, lines: lines, buttons: kb, formatted: formatted}}, true
	}

	var out []tgMessage
	for i := range media {
		m := media[i]
		msg := tgMessage{file: &m, html: html.EscapeString(m.Caption), body: m.Caption}
		if i == 0 {
			switch {
			case textPlain == "":
				msg.buttons, msg.lines = kb, lines
			case tgLen(textPlain) <= tgCaptionLimit:
				msg.html, msg.body, msg.lines, msg.buttons, msg.formatted = textHTML, text, lines, kb, formatted
			default:
				out = append(out, msg)
				if tgLen(textPlain) > tgTextLimit {
					return nil, false
				}
				msg = tgMessage{html: textHTML, body: text, lines: lines, buttons: kb, formatted: formatted}
			}
		}
		out = append(out, msg)
	}
	return out, true
}

// customerErrCode is the result code of a failed send to a customer.
func customerErrCode(err error) string {
	switch {
	case errors.Is(err, ErrCustomerBlocked):
		return ErrCustomerUnreachable
	case errors.Is(err, ErrCustomerRateLimited):
		return ErrRateLimited
	default:
		return ErrInternal
	}
}

// quotedCustomerMsg turns a reply_to hub_msg_id (`<chat>:<message>`) into the
// message id to quote in the customer's chat, or 0 when it names no message of
// that chat. An unquotable reply is still sent: the answer matters more.
func quotedCustomerMsg(chatID int64, replyTo string) int64 {
	chat, msg, ok := strings.Cut(replyTo, ":")
	if !ok {
		return 0
	}
	c, err1 := strconv.ParseInt(chat, 10, 64)
	m, err2 := strconv.ParseInt(msg, 10, 64)
	if err1 != nil || err2 != nil || c != chatID || m <= 0 {
		return 0
	}
	return m
}

// HubMsgIDForTelegram is the hub_msg_id of a message in a customer's chat.
func HubMsgIDForTelegram(chatID, msgID int64) string {
	return strconv.FormatInt(chatID, 10) + ":" + strconv.FormatInt(msgID, 10)
}

func (e *Executor) sendReplyTelegram(ctx context.Context, p *Send) Result {
	userID, _ := ParseTgChatKey(p.Conversation)
	if e.Customer == nil {
		return fail(ErrUnknownConversation)
	}
	media, ok := decodeOutMedia(p.Media)
	if !ok || (len(media) == 0 && strings.TrimSpace(p.Text) == "") {
		return fail(ErrInvalid)
	}
	// Without a topic there is nowhere to mirror to; the Hub does not know
	// this customer.
	thread, found, err := e.Bridge.ThreadFor(p.Conversation)
	if err != nil {
		e.Log.Error("agent link: topic lookup failed", zap.Error(err))
		return fail(ErrInternal)
	}
	if !found {
		return fail(ErrUnknownConversation)
	}

	var copyables []Copyable
	if p.Copyables != nil {
		copyables = *p.Copyables
	}
	plan, ok := planTelegramReply(p.Text, p.Format == FormatHTML, media, copyables, p.Link)
	if !ok {
		return fail(ErrInvalid)
	}
	quote := quotedCustomerMsg(userID, p.ReplyTo)

	var first string
	mirrored := true
	for i, m := range plan {
		// Only the first message quotes; the rest are part of the same answer.
		var replyTo int64
		if i == 0 {
			replyTo = quote
		}
		var id int64
		var err error
		if m.file == nil {
			id, err = e.Customer.SendText(ctx, userID, CustomerText{HTML: m.html, Formatted: m.formatted, Buttons: m.buttons, ReplyTo: replyTo})
		} else {
			id, err = e.Customer.SendFile(ctx, userID, CustomerFile{
				Kind: m.file.Kind, Data: m.file.Data, Filename: m.file.Filename, Mime: m.file.Mime,
				HTMLCaption: m.html, FormattedCaption: m.formatted, Buttons: m.buttons, ReplyTo: replyTo,
			})
		}
		if err != nil {
			e.Log.Warn("agent link: Telegram send to a customer failed", zap.Error(err))
			if i == 0 {
				return fail(customerErrCode(err))
			}
			// The customer already has part of the answer; staff must know.
			if _, err := e.Topics.PostText(ctx, thread, plainTopic(notePrefix+"Only part of the last answer reached the customer, please check.")); err != nil {
				e.Log.Error("agent link: could not post the note about a partial answer", zap.Error(err))
			}
			return Result{OK: false, Error: ErrInternal, HubMsgID: first, DeliveredAt: FormatTS(e.Clock.Now())}
		}
		hubID := HubMsgIDForTelegram(userID, id)
		if i == 0 {
			first = hubID
		}
		if e.Guard != nil {
			e.Guard.Mark(hubID)
		}
		parts := mirrorParts{Text: m.body, Lines: m.lines, HTML: m.formatted}
		if i == len(plan)-1 {
			parts.Signature = p.Signature
		}
		if !e.mirrorTelegram(ctx, userID, thread, id, m, parts) {
			mirrored = false
		}
	}
	e.replaceCard(ctx, p, mirrored)
	return Result{OK: true, HubMsgID: first, DeliveredAt: FormatTS(e.Clock.Now())}
}

// mirrorTelegram posts what was just sent into the topic and pairs the posts
// with the customer's message. A failure is logged and nothing more: the
// customer already has the message. It reports whether the whole mirror is in
// the topic.
func (e *Executor) mirrorTelegram(ctx context.Context, chatID, thread, customerMsgID int64, m tgMessage, parts mirrorParts) bool {
	var topicIDs []int64
	post := func(id int64, err error) bool {
		if err != nil {
			e.Log.Error("agent link: could not mirror the reply into the topic", zap.Int64("customer_msg_id", customerMsgID), zap.Error(err))
			return false
		}
		topicIDs = append(topicIDs, id)
		return true
	}
	// The Agent's own HTML may be something Telegram refuses; the customer got
	// the plain fallback of it, so the topic gets the same instead of nothing.
	plainParts := parts
	plainParts.HTML, plainParts.Text = false, StripHTML(parts.Text)
	postText := func(limit int) (int64, error) {
		text, _ := renderMirror(parts, limit)
		id, err := e.Topics.PostText(ctx, thread, text)
		if err != nil && parts.HTML {
			e.Log.Warn("agent link: the topic refused the formatted mirror, posting it as plain text", zap.Error(err))
			text, _ = renderMirror(plainParts, limit)
			return e.Topics.PostText(ctx, thread, text)
		}
		return id, err
	}
	postCaption := func(mm OutMedia, caption TopicText) (int64, error) {
		mm.Caption, mm.HTML = caption.Text, caption.HTML
		id, err := e.Topics.PostMedia(ctx, thread, mm)
		if err != nil && parts.HTML {
			e.Log.Warn("agent link: the topic refused the formatted mirror, posting it as plain text", zap.Error(err))
			plain, _ := renderMirror(plainParts, tgCaptionLimit)
			mm.Caption, mm.HTML = plain.Text, plain.HTML
			return e.Topics.PostMedia(ctx, thread, mm)
		}
		return id, err
	}
	posted := true
	if m.file == nil {
		posted = post(postText(tgTextLimit))
	} else if caption, fits := renderMirror(parts, tgCaptionLimit); fits {
		posted = post(postCaption(*m.file, caption))
	} else {
		// The topic's own caption limit is the same: the text follows the file.
		mm := *m.file
		mm.Caption = strings.TrimSpace(mirrorPrefix)
		if posted = post(e.Topics.PostMedia(ctx, thread, mm)); posted {
			posted = post(postText(tgTextLimit))
		}
	}
	for _, id := range topicIDs {
		if err := e.Customer.RecordPair(chatID, customerMsgID, thread, id); err != nil {
			e.Log.Error("agent link: could not record the mirror's message pair", zap.String("hub_msg_id", HubMsgIDForTelegram(chatID, customerMsgID)), zap.Error(err))
		}
	}
	return posted
}
