package utils

import (
	"strings"
	"unicode/utf16"
)

// Telegram's limits for the text of a message and for a caption.
const (
	TgTextLimit    = 4096
	TgCaptionLimit = 1024
)

// BoldCustomerBody wraps a customer's message body in <b>…</b> for the staff
// topic (telegram.bold_customer_messages). bodyHTML must already be escaped
// Telegram HTML; whatever it holds (a mention link, say) stays validly nested
// inside the bold. prefixHTML is the bridge's own text that goes before the
// body in the same message, and only counts toward the limit.
//
// Nothing is wrapped when the result would not fit limit, or when the body is
// blank: the message is then sent exactly as it was. The length is measured as
// written, in UTF-16 units, which is conservative because Telegram counts the
// text after parsing.
func BoldCustomerBody(prefixHTML, bodyHTML string, limit int) string {
	if strings.TrimSpace(bodyHTML) == "" {
		return bodyHTML
	}
	wrapped := "<b>" + bodyHTML + "</b>"
	if len(utf16.Encode([]rune(prefixHTML+wrapped))) > limit {
		return bodyHTML
	}
	return wrapped
}
