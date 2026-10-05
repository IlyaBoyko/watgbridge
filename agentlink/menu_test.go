package agentlink

import (
	"regexp"
	"testing"
	"unicode/utf8"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

var telegramCommandName = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// H5: the entries must be accepted by Telegram's setMyCommands.
func TestMenuCommandsAreValidForTelegram(t *testing.T) {
	cmds := MenuCommands()
	if len(cmds) != 7 {
		t.Fatalf("want 7 agent commands, got %d", len(cmds))
	}
	seen := map[string]bool{}
	for _, c := range cmds {
		if !telegramCommandName.MatchString(c.Command) {
			t.Errorf("invalid command name %q", c.Command)
		}
		if n := utf8.RuneCountInString(c.Description); n < 1 || n > 256 {
			t.Errorf("%s: description length %d out of 1..256", c.Command, n)
		}
		if seen[c.Command] {
			t.Errorf("duplicate command %q", c.Command)
		}
		seen[c.Command] = true
		// Each entry must be caught by the /ai_ interceptor, never forwarded.
		got, _, ok := ParseAICommand("/" + c.Command + " x")
		if !ok || got != c.Command {
			t.Errorf("/%s is not intercepted as an /ai_ command (got %q ok=%v)", c.Command, got, ok)
		}
		if !validControlName(c.Command) {
			t.Errorf("%s is not a valid control name", c.Command)
		}
	}
}

func TestMenuCommandsReturnsACopy(t *testing.T) {
	c := MenuCommands()
	c[0].Command = "tampered"
	if MenuCommands()[0].Command != "ai_status" {
		t.Fatal("MenuCommands exposed its backing array")
	}
}

func TestWithMenuCommandsOnlyWhenAgentEnabled(t *testing.T) {
	base := []gotgbot.BotCommand{{Command: "start", Description: "Start"}, {Command: "help", Description: "Help"}}

	off := WithMenuCommands(base, false)
	if len(off) != len(base) || off[0].Command != "start" || off[1].Command != "help" {
		t.Fatalf("disabled agent must leave the menu as is, got %v", off)
	}

	on := WithMenuCommands(base, true)
	if len(on) != len(base)+len(menuCommands) {
		t.Fatalf("enabled: want %d entries, got %d", len(base)+len(menuCommands), len(on))
	}
	if on[0].Command != "start" || on[1].Command != "help" {
		t.Fatalf("the bridge's own commands must come first, got %v", on[:2])
	}
	if on[2].Command != "ai_status" || on[len(on)-1].Command != "ai_slip" {
		t.Fatalf("agent commands must follow in order, got %v", on[2:])
	}
	if len(base) != 2 {
		t.Fatal("base slice was modified")
	}

	if got := WithMenuCommands(nil, false); len(got) != 0 {
		t.Fatalf("nil base, disabled: want empty, got %v", got)
	}
}
