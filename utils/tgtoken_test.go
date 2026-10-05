package utils

import (
	"errors"
	"fmt"
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestTgIsBadToken(t *testing.T) {
	wrap := func(code int) error {
		return fmt.Errorf("failed to check bot token: %w", &gotgbot.TelegramError{Method: "getMe", Code: code})
	}
	for code, want := range map[int]bool{401: true, 404: true, 429: false, 502: false} {
		if got := TgIsBadToken(wrap(code)); got != want {
			t.Errorf("code %d: got %v, want %v", code, got, want)
		}
	}
	if TgIsBadToken(errors.New("Post https://api.telegram.org/bot/getMe: proxyconnect")) {
		t.Error("a network error is not a bad token")
	}
	if TgIsBadToken(nil) {
		t.Error("nil is not a bad token")
	}
}
