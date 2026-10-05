package utils

import (
	"errors"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TgIsBadToken reports whether a Telegram login failed because the bot token
// is wrong (the Bot API answered 401 or 404). Retrying cannot fix that.
func TgIsBadToken(err error) bool {
	var te *gotgbot.TelegramError
	return errors.As(err, &te) && (te.Code == 401 || te.Code == 404)
}
