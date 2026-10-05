package utils

import (
	"html"
	"strings"
	"testing"
)

func TestBoldCustomerBodyWraps(t *testing.T) {
	if got := BoldCustomerBody("🧑: <b>Wei</b>\n", "hello", TgTextLimit); got != "<b>hello</b>" {
		t.Errorf("got %q", got)
	}
}

// The body arrives escaped, so what looks like markup in it is text. Wrapping
// must not touch it, and a link inside stays validly nested.
func TestBoldCustomerBodyKeepsEscapingAndNesting(t *testing.T) {
	body := html.EscapeString(`a < b & c > d *not bold* _not italic_ <i>x</i>`)
	got := BoldCustomerBody("", body, TgTextLimit)
	want := "<b>a &lt; b &amp; c &gt; d *not bold* _not italic_ &lt;i&gt;x&lt;/i&gt;</b>"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	withLink := `hi <a href="https://wa.me/601">@Aiman</a>!`
	if got := BoldCustomerBody("", withLink, TgTextLimit); got != "<b>"+withLink+"</b>" {
		t.Errorf("got %q", got)
	}
}

func TestBoldCustomerBodyLengthGuard(t *testing.T) {
	prefix := "hdr\n" // 4 units
	fits := strings.Repeat("a", TgCaptionLimit-len(prefix)-len("<b></b>"))
	if got := BoldCustomerBody(prefix, fits, TgCaptionLimit); got != "<b>"+fits+"</b>" {
		t.Error("a caption that fits exactly must be bold")
	}
	over := fits + "a"
	if got := BoldCustomerBody(prefix, over, TgCaptionLimit); got != over {
		t.Error("a caption the bold would push over the limit must stay as it was")
	}
	// The same body is fine as a text message.
	if got := BoldCustomerBody(prefix, over, TgTextLimit); got != "<b>"+over+"</b>" {
		t.Error("the text limit is 4096")
	}
	// Telegram counts UTF-16 units: an emoji is two.
	emoji := strings.Repeat("😀", (TgCaptionLimit-len("<b></b>"))/2) // 1017 units: fits only without prefix
	if got := BoldCustomerBody("", emoji, TgCaptionLimit); got != "<b>"+emoji+"</b>" {
		t.Error("emoji body that fits must be bold")
	}
	if got := BoldCustomerBody("hdr\n", emoji, TgCaptionLimit); got != emoji {
		t.Error("emoji count as two units each")
	}
}

func TestBoldCustomerBodyBlankIsLeftAlone(t *testing.T) {
	for _, b := range []string{"", "  ", "\n"} {
		if got := BoldCustomerBody("x", b, TgTextLimit); got != b {
			t.Errorf("blank %q became %q", b, got)
		}
	}
}
