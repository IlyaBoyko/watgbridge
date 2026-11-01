package agentlink

import (
	"html"
	"regexp"
	"strings"
)

// How the Agent's posts look in the staff topic (protocol section 5b). This is
// the topic only: what the customer receives is never formatted here.

// TopicText is a text for the staff topic. HTML says Text is Telegram HTML
// whose dynamic parts are already escaped; otherwise it is plain, and the
// poster escapes it. Plain is also the fallback when the formatted text would
// not fit Telegram's limits.
type TopicText struct {
	Text string
	HTML bool
}

func plainTopic(s string) TopicText { return TopicText{Text: s} }

func esc(s string) string { return html.EscapeString(s) }

// Prefixes of the Agent's mirror posts: the robot for what it sent by itself,
// the globe for staff text it translated and sent on a staff member's behalf.
const (
	robotPrefix = "🤖"
	relayPrefix = "🌐"
)

// mirrorParts is one message of a reply as the topic mirror shows it. Everything
// is plain text; renderMirror escapes it.
type mirrorParts struct {
	Text      string   // the reply's own text or caption
	Lines     []string // copyable and link lines, `label: value`
	Signature string   // set on the last message of a reply only
	// HTML says Text is the Agent's own Telegram HTML (protocol section 5c):
	// the mirror keeps it instead of escaping it.
	HTML bool
	// Prefix opens the post; empty means the robot. The globe marks a relay.
	Prefix string
	// Translation is the English of Text (protocol section 5f), shown under it.
	Translation string
	// ReplyTo is the topic message the post answers (the staff post of a
	// relay); 0 for none. Only a text post can carry it.
	ReplyTo int64
}

var (
	anchorRe = regexp.MustCompile(`(?is)<a\s+[^>]*?href\s*=\s*"([^"]*)"[^>]*>(.*?)</a>`)
	tagRe    = regexp.MustCompile(`<[^>]*>`)
)

// StripHTML turns the Agent's Telegram HTML into plain text: tags go, entities
// are unescaped, and a link keeps its address after its label, since a plain
// message has no other way to show it.
func StripHTML(s string) string {
	s = anchorRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := anchorRe.FindStringSubmatch(m)
		label, href := tagRe.ReplaceAllString(sub[2], ""), html.UnescapeString(sub[1])
		if label == "" || html.UnescapeString(label) == href {
			return sub[1]
		}
		return label + " (" + sub[1] + ")"
	})
	return html.UnescapeString(tagRe.ReplaceAllString(s, ""))
}

// translationLine is the staff-only English line (protocol section 5f).
func translationLine(tr string, rich bool) string {
	if rich {
		return "🌐 <i>" + esc(tr) + "</i>"
	}
	return "🌐 " + tr
}

// shortened cuts tr to its first n runes and closes it with an ellipsis.
func shortened(tr string, n int) string {
	return strings.TrimRight(string([]rune(tr)[:n]), " \t\r\n") + "…"
}

// fitTranslation builds a post with the translation in it. When the post would
// pass limit it shortens the translation with an ellipsis, never the text
// around it, and drops the translation if even a single character will not
// fit. The bool says the result fits.
func fitTranslation(tr string, limit int, build func(tr string) string) (string, bool) {
	s := build(tr)
	if tr == "" || tgLen(s) <= limit {
		return s, tgLen(s) <= limit
	}
	// Escaping adds length rune by rune, so the longest cut that fits can be
	// found by bisection.
	lo, hi, best := 1, len([]rune(tr))-1, 0
	for lo <= hi {
		mid := (lo + hi) / 2
		if tgLen(build(shortened(tr, mid))) <= limit {
			best, lo = mid, mid+1
		} else {
			hi = mid - 1
		}
	}
	if best == 0 {
		s = build("")
		return s, tgLen(s) <= limit
	}
	return build(shortened(tr, best)), true
}

// fitRichOrPlain returns the formatted post when it fits limit, else the plain
// one (the poster escapes it), each with its translation shortened to fit. The
// bool is false when even the plain fallback is over limit.
func fitRichOrPlain(tr string, limit int, build func(tr string, rich bool) string) (TopicText, bool) {
	if s, ok := fitTranslation(tr, limit, func(t string) string { return build(t, true) }); ok {
		return TopicText{Text: s, HTML: true}, true
	}
	s, ok := fitTranslation(tr, limit, func(t string) string { return build(t, false) })
	return plainTopic(s), ok
}

// renderMirror builds the mirror post: the robot and the reply on one line
// (plain: the owner finds a quote box hard to scan), the English translation
// when there is one, the extra lines, then the signature. The bool is false
// when even the plain fallback is over limit (counted the way Telegram does,
// in UTF-16 units).
//
// The HTML form is measured as written, so escaping counts against the limit:
// that is conservative, since Telegram counts the text after parsing.
func renderMirror(p mirrorParts, limit int) (TopicText, bool) {
	prefix := p.Prefix
	if prefix == "" {
		prefix = robotPrefix
	}
	return fitRichOrPlain(p.Translation, limit, func(tr string, rich bool) string {
		out := prefix
		if p.Text != "" {
			switch {
			case p.HTML && rich:
				out += " " + p.Text
			case p.HTML:
				out += " " + StripHTML(p.Text)
			case rich:
				out += " " + esc(p.Text)
			default:
				out += " " + p.Text
			}
		}
		if tr != "" {
			out = appendLine(out, translationLine(tr, rich))
		}
		for _, l := range p.Lines {
			if rich {
				l = esc(l)
			}
			out = appendLine(out, l)
		}
		if p.Signature != "" {
			if rich {
				out = appendLine(out, "<i>✓ "+esc(p.Signature)+"</i>")
			} else {
				out = appendLine(out, "✓ "+p.Signature)
			}
		}
		return strings.TrimSpace(out)
	})
}

// renderCard puts the title above the text. Without a title the card is the
// text alone, as it always was.
func renderCard(title, text string) TopicText { return renderCardTr(title, text, "") }

// renderCardTr is renderCard with the staff-only English under the text.
func renderCardTr(title, text, translation string) TopicText {
	title = strings.TrimSpace(title)
	if title == "" && translation == "" {
		return plainTopic(text)
	}
	got, _ := fitRichOrPlain(translation, tgTextLimit, func(tr string, rich bool) string {
		var out string
		switch {
		case title != "" && rich:
			out = "📝 <b>" + esc(title) + "</b>\n<blockquote>" + esc(text) + "</blockquote>"
		case title != "":
			out = "📝 " + title + "\n" + text
		case rich:
			out = esc(text)
		default:
			out = text
		}
		if tr != "" {
			out = appendLine(out, translationLine(tr, rich))
		}
		return out
	})
	return got
}

func renderNote(text string) TopicText { return renderNoteTr(text, "") }

// renderNoteTr is renderNote with the staff-only English on a second line.
func renderNoteTr(text, translation string) TopicText {
	got, _ := fitRichOrPlain(translation, tgTextLimit, func(tr string, rich bool) string {
		out := notePrefix + text
		if rich {
			out = notePrefix + "<i>" + esc(text) + "</i>"
		}
		if tr != "" {
			out = appendLine(out, translationLine(tr, rich))
		}
		return out
	})
	return got
}

// renderSigned is what a card shrinks to when Telegram would not let the Hub
// delete it: the signature alone, without buttons.
func renderSigned(signature string) TopicText {
	if signature == "" {
		signature = "Sent"
	}
	return TopicText{Text: "<i>✓ " + esc(signature) + "</i>", HTML: true}
}
