package agentlink

import (
	"html"
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

// mirrorParts is one message of a reply as the topic mirror shows it. Everything
// is plain text; renderMirror escapes it.
type mirrorParts struct {
	Text      string   // the reply's own text or caption
	Lines     []string // copyable and link lines, `label: value`
	Signature string   // set on the last message of a reply only
}

// renderMirror builds the mirror post: the robot, the reply in a blockquote,
// the extra lines, then the signature. The bool is false when even the plain
// fallback is over limit (counted the way Telegram does, in UTF-16 units).
//
// The HTML form is measured as written, so escaping counts against the limit:
// that is conservative, since Telegram counts the text after parsing.
func renderMirror(p mirrorParts, limit int) (TopicText, bool) {
	rich, plain := "🤖", "🤖"
	if p.Text != "" {
		rich += " <blockquote>" + esc(p.Text) + "</blockquote>"
		plain += " " + p.Text
	}
	for _, l := range p.Lines {
		rich = appendLine(rich, esc(l))
		plain = appendLine(plain, l)
	}
	if p.Signature != "" {
		rich = appendLine(rich, "<i>✓ "+esc(p.Signature)+"</i>")
		plain = appendLine(plain, "✓ "+p.Signature)
	}
	rich, plain = strings.TrimSpace(rich), strings.TrimSpace(plain)
	if tgLen(rich) <= limit {
		return TopicText{Text: rich, HTML: true}, true
	}
	return plainTopic(plain), tgLen(plain) <= limit
}

// renderCard puts the title above the text. Without a title the card is the
// text alone, as it always was.
func renderCard(title, text string) TopicText {
	title = strings.TrimSpace(title)
	if title == "" {
		return plainTopic(text)
	}
	rich := "📝 <b>" + esc(title) + "</b>\n<blockquote>" + esc(text) + "</blockquote>"
	if tgLen(rich) <= tgTextLimit {
		return TopicText{Text: rich, HTML: true}
	}
	return plainTopic("📝 " + title + "\n" + text)
}

func renderNote(text string) TopicText {
	rich := notePrefix + "<i>" + esc(text) + "</i>"
	if tgLen(rich) <= tgTextLimit {
		return TopicText{Text: rich, HTML: true}
	}
	return plainTopic(notePrefix + text)
}

// renderSigned is what a card shrinks to when Telegram would not let the Hub
// delete it: the signature alone, without buttons.
func renderSigned(signature string) TopicText {
	if signature == "" {
		signature = "Sent"
	}
	return TopicText{Text: "<i>✓ " + esc(signature) + "</i>", HTML: true}
}
