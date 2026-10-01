package agentlink

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// CardRow remembers a card the Agent posted in a topic: where it is, so
// edit_card can find the Telegram message, and which row id button presses
// carry (callback data is limited to 64 bytes, a card id plus a button id can
// exceed that).
type CardRow struct {
	ID           uint   `gorm:"primaryKey;autoIncrement"`
	CardID       string `gorm:"uniqueIndex;size:128;not null"`
	Conversation string `gorm:"not null"`
	TgChatID     int64  `gorm:"not null"`
	TgThreadID   int64  `gorm:"not null"`
	TgMsgID      int64  `gorm:"not null"`
	// Keyboard is the Agent's last buttons as JSON. edit_card without
	// `buttons` keeps the keyboard, but Telegram drops a keyboard that an edit
	// does not repeat, so the Hub has to be able to repeat it.
	Keyboard  string    `gorm:"type:text"`
	CreatedAt time.Time `gorm:"index;not null"`
}

func (CardRow) TableName() string { return "agent_card" }

// cardRetention is how long a card stays addressable.
const cardRetention = 30 * 24 * time.Hour

// ErrCardExists is returned when a card id is already recorded.
var ErrCardExists = errors.New("card id already exists")

// CardStore is the table of cards the Agent posted.
type CardStore struct {
	db    *gorm.DB
	clock Clock
}

func NewCardStore(db *gorm.DB, clock Clock) *CardStore { return &CardStore{db: db, clock: clock} }

// Create records a card before it is posted, because the row id is part of
// its buttons' callback data. A card id seen before gives ErrCardExists.
func (s *CardStore) Create(row CardRow) (CardRow, error) {
	var n int64
	if err := s.db.Model(&CardRow{}).Where("card_id = ?", row.CardID).Count(&n).Error; err != nil {
		return CardRow{}, err
	}
	if n > 0 {
		return CardRow{}, ErrCardExists
	}
	row.ID = 0
	row.CreatedAt = s.clock.Now().UTC()
	if err := s.db.Create(&row).Error; err != nil {
		return CardRow{}, err
	}
	return row, nil
}

func (s *CardStore) Delete(id uint) error { return s.db.Delete(&CardRow{}, id).Error }

// SetMessage records where Telegram put the card.
func (s *CardStore) SetMessage(id uint, chatID, msgID int64) error {
	return s.db.Model(&CardRow{}).Where("id = ?", id).Updates(map[string]any{"tg_chat_id": chatID, "tg_msg_id": msgID}).Error
}

func (s *CardStore) SetKeyboard(id uint, keyboard string) error {
	return s.db.Model(&CardRow{}).Where("id = ?", id).Update("keyboard", keyboard).Error
}

func (s *CardStore) find(where string, arg any) (CardRow, bool, error) {
	var row CardRow
	err := s.db.Where(where, arg).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CardRow{}, false, nil
	}
	return row, err == nil, err
}

func (s *CardStore) ByCardID(cardID string) (CardRow, bool, error) {
	return s.find("card_id = ?", cardID)
}

func (s *CardStore) ByRowID(id uint) (CardRow, bool, error) { return s.find("id = ?", id) }

// Prune forgets cards older than 30 days and returns how many.
func (s *CardStore) Prune() (int64, error) {
	cutoff := s.clock.Now().Add(-cardRetention).UTC()
	res := s.db.Where("created_at < ?", cutoff).Delete(&CardRow{})
	return res.RowsAffected, res.Error
}

/* -------------------------------------------------------------- keyboard -- */

// KeyboardButton is one inline button as it is posted.
type KeyboardButton struct {
	Label        string
	CallbackData string
}

// Keyboard is a list of rows of inline buttons. An empty one means no keyboard.
type Keyboard [][]KeyboardButton

const callbackPrefix = "ag:"

// maxCallbackDataBytes is Telegram's limit on a button's callback data.
const maxCallbackDataBytes = 64

// EncodeCallbackData builds `ag:<row id>:<button id>`. It uses the row id, not
// the card id, so the longest legal button id (32 bytes) always fits.
func EncodeCallbackData(rowID uint, buttonID string) string {
	return callbackPrefix + strconv.FormatUint(uint64(rowID), 10) + ":" + buttonID
}

// IsCardCallback reports whether callback data belongs to the Agent's cards.
func IsCardCallback(data string) bool { return strings.HasPrefix(data, callbackPrefix) }

// DecodeCallbackData is the inverse of EncodeCallbackData. A button id may
// itself contain colons.
func DecodeCallbackData(data string) (rowID uint, buttonID string, ok bool) {
	rest, found := strings.CutPrefix(data, callbackPrefix)
	if !found {
		return 0, "", false
	}
	idPart, buttonID, found := strings.Cut(rest, ":")
	if !found || buttonID == "" {
		return 0, "", false
	}
	n, err := strconv.ParseUint(idPart, 10, 64)
	if err != nil || n == 0 {
		return 0, "", false
	}
	return uint(n), buttonID, true
}

// BuildKeyboard turns the Agent's buttons into a keyboard, keeping the rows
// and their order. A row with no buttons is dropped (Telegram refuses it).
func BuildKeyboard(rowID uint, buttons Buttons) (Keyboard, error) {
	var kb Keyboard
	for _, row := range buttons {
		if len(row) == 0 {
			continue
		}
		out := make([]KeyboardButton, 0, len(row))
		for _, b := range row {
			data := EncodeCallbackData(rowID, b.ID)
			if len(data) > maxCallbackDataBytes {
				return nil, fmt.Errorf("callback data for button %q is %d bytes", b.ID, len(data))
			}
			out = append(out, KeyboardButton{Label: b.Label, CallbackData: data})
		}
		kb = append(kb, out)
	}
	return kb, nil
}

func encodeButtons(b Buttons) string {
	if len(b) == 0 {
		return ""
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

func decodeButtons(s string) Buttons {
	if s == "" {
		return nil
	}
	var b Buttons
	if err := json.Unmarshal([]byte(s), &b); err != nil {
		return nil
	}
	return b
}
