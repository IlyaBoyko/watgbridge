package agentlink

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The tables of the translation features (protocol section 5f). All belong to
// agentlink and are created by Migrate; none touches the bridge's own tables.

// RelaySettingRow is the per-conversation switch of the staff relay. A
// conversation without a row has it off.
type RelaySettingRow struct {
	Conversation string `gorm:"primaryKey;size:190"`
	Enabled      bool   `gorm:"not null"`
	UpdatedAt    time.Time
}

func (RelaySettingRow) TableName() string { return "agent_relay" }

// RelayPostRow marks a staff post the relay took over (relayed, or refused
// because the link was down). Nothing was ever sent to the customer for it, so
// an edit of the post must not be sent either.
type RelayPostRow struct {
	TgChatID  int64     `gorm:"primaryKey;autoIncrement:false"`
	TgMsgID   int64     `gorm:"primaryKey;autoIncrement:false"`
	CreatedAt time.Time `gorm:"index;not null"`
}

func (RelayPostRow) TableName() string { return "agent_relay_post" }

// RelayFailRow is a staff post the Hub refused because the Agent was offline,
// with what the Retry button needs. It is stored so Retry works after a restart.
type RelayFailRow struct {
	ID           uint   `gorm:"primaryKey;autoIncrement"`
	Conversation string `gorm:"not null"`
	TgChatID     int64  `gorm:"not null"`
	TopicID      int64  `gorm:"not null"`
	StaffMsgID   int64  `gorm:"not null"`
	// FailMsgID is the Hub's failure post carrying the buttons.
	FailMsgID  int64
	AuthorID   int64
	AuthorName string
	Text       string `gorm:"type:text;not null"`
	ReplyTo    string
	// Attempt is the attempt a Retry would make: 2 after a first refusal.
	Attempt   int       `gorm:"not null"`
	CreatedAt time.Time `gorm:"index;not null"`
}

func (RelayFailRow) TableName() string { return "agent_relay_fail" }

// TopicPostRow is what the Hub needs to rebuild a topic post it made, so that
// `annotate` can add a line to it: the exact HTML the post was sent with.
type TopicPostRow struct {
	TgMsgID   int64     `gorm:"primaryKey;autoIncrement:false"`
	TgChatID  int64     `gorm:"not null"`
	HTML      string    `gorm:"type:text;not null"`
	Caption   bool      `gorm:"not null"`
	CreatedAt time.Time `gorm:"index;not null"`
}

func (TopicPostRow) TableName() string { return "agent_topic_post" }

// relayRetention is how long the relay's bookkeeping is kept: an edit or an
// annotation long after the fact has no use for it.
const relayRetention = 30 * 24 * time.Hour

// DefaultRelayWatchdog is protocol section 5f.5's 60 seconds.
const DefaultRelayWatchdog = 60 * time.Second

// AfterFunc runs f once after d and returns a function that stops it (true when
// it stopped it before it ran). Tests supply a fake; time.AfterFunc is the real
// one.
type AfterFunc func(d time.Duration, f func()) (stop func() bool)

func realAfter(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// RelayKit is the state of the translation features: the switch, the posts the
// relay took over, the failure posts, the rebuildable topic posts and the
// watchdog timers.
type RelayKit struct {
	db    *gorm.DB
	clock Clock
	log   *zap.Logger
	after AfterFunc
	delay time.Duration

	mu     sync.Mutex
	timers map[string]func() bool
}

// NewRelayKit builds the kit. A nil after is time.AfterFunc; a zero delay is
// the protocol's 60 seconds.
func NewRelayKit(db *gorm.DB, clock Clock, log *zap.Logger, after AfterFunc, delay time.Duration) *RelayKit {
	if after == nil {
		after = realAfter
	}
	if delay <= 0 {
		delay = DefaultRelayWatchdog
	}
	return &RelayKit{db: db, clock: clock, log: log, after: after, delay: delay, timers: map[string]func() bool{}}
}

// SetRelay stores the switch. Setting the value it already has is fine.
func (k *RelayKit) SetRelay(conversation string, on bool) error {
	row := RelaySettingRow{Conversation: conversation, Enabled: on, UpdatedAt: k.clock.Now().UTC()}
	return k.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "conversation"}},
		DoUpdates: clause.AssignmentColumns([]string{"enabled", "updated_at"}),
	}).Create(&row).Error
}

// RelayOn reports the switch; off when never set.
func (k *RelayKit) RelayOn(conversation string) (bool, error) {
	var row RelaySettingRow
	err := k.db.Where("conversation = ?", conversation).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return row.Enabled, err
}

func (k *RelayKit) markRelayPost(chatID, msgID int64) error {
	row := RelayPostRow{TgChatID: chatID, TgMsgID: msgID, CreatedAt: k.clock.Now().UTC()}
	return k.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

// IsRelayPost reports whether the relay took over that staff post.
func (k *RelayKit) IsRelayPost(chatID, msgID int64) bool {
	var n int64
	err := k.db.Model(&RelayPostRow{}).Where("tg_chat_id = ? AND tg_msg_id = ?", chatID, msgID).Count(&n).Error
	return err == nil && n > 0
}

func (k *RelayKit) createFail(row RelayFailRow) (RelayFailRow, error) {
	row.ID, row.CreatedAt = 0, k.clock.Now().UTC()
	return row, k.db.Create(&row).Error
}

func (k *RelayKit) fail(id uint) (RelayFailRow, bool, error) {
	var row RelayFailRow
	err := k.db.Where("id = ?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RelayFailRow{}, false, nil
	}
	return row, err == nil, err
}

func (k *RelayKit) setFailMsg(id uint, msgID int64) error {
	return k.db.Model(&RelayFailRow{}).Where("id = ?", id).Update("fail_msg_id", msgID).Error
}

func (k *RelayKit) deleteFail(id uint) error { return k.db.Delete(&RelayFailRow{}, id).Error }

// SaveTopicPost remembers the HTML of a post so a line can be added to it later.
func (k *RelayKit) SaveTopicPost(row TopicPostRow) error {
	row.CreatedAt = k.clock.Now().UTC()
	return k.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

func (k *RelayKit) topicPost(msgID int64) (TopicPostRow, bool, error) {
	var row TopicPostRow
	err := k.db.Where("tg_msg_id = ?", msgID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return TopicPostRow{}, false, nil
	}
	return row, err == nil, err
}

// Prune forgets old bookkeeping and returns how many rows went.
func (k *RelayKit) Prune() (int64, error) {
	cutoff := k.clock.Now().Add(-relayRetention).UTC()
	var total int64
	for _, m := range []any{&RelayPostRow{}, &RelayFailRow{}, &TopicPostRow{}} {
		res := k.db.Where("created_at < ?", cutoff).Delete(m)
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
	}
	return total, nil
}

/* -------------------------------------------------------------- watchdog -- */

// startWatchdog runs fire once after the delay unless cancelWatchdog came first.
// It lives in memory only (protocol section 5f.5).
func (k *RelayKit) startWatchdog(relayID string, fire func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if stop, ok := k.timers[relayID]; ok {
		stop()
	}
	var stop func() bool
	stop = k.after(k.delay, func() {
		k.mu.Lock()
		_, live := k.timers[relayID]
		delete(k.timers, relayID)
		k.mu.Unlock()
		if live {
			fire()
		}
	})
	k.timers[relayID] = stop
}

// cancelWatchdog is safe on a nil kit and for a relay that has no watchdog.
func (k *RelayKit) cancelWatchdog(relayID string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if stop, ok := k.timers[relayID]; ok {
		stop()
		delete(k.timers, relayID)
	}
}

/* ----------------------------------------------------------- relay ids -- */

// RelayID is `<chat_id>:<message_id>` of the staff post (protocol section 5f.4).
func RelayID(chatID, msgID int64) string {
	return strconv.FormatInt(chatID, 10) + ":" + strconv.FormatInt(msgID, 10)
}

// ParseRelayID is the inverse of RelayID.
func ParseRelayID(s string) (chatID, msgID int64, ok bool) {
	c, m, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	chatID, err1 := strconv.ParseInt(c, 10, 64)
	msgID, err2 := strconv.ParseInt(m, 10, 64)
	if err1 != nil || err2 != nil || msgID <= 0 {
		return 0, 0, false
	}
	return chatID, msgID, true
}

// relayReplyTarget is the staff post a relay_of names, or 0.
func relayReplyTarget(relayID string) int64 {
	if _, id, ok := ParseRelayID(relayID); ok {
		return id
	}
	return 0
}

// RelayEventID is the deterministic id of a staff.relay event. The first
// attempt has no suffix; a Retry is a new attempt and so a new event (protocol
// section 5f.4).
func RelayEventID(conversation, relayID string, attempt int) string {
	if attempt <= 1 {
		return EventID(TypeStaffRelay, conversation, relayID)
	}
	return EventID(TypeStaffRelay, conversation, fmt.Sprintf("%s|%d", relayID, attempt))
}

// decorateMirror adds what a send's mirror shows besides the text: the globe
// prefix of a relay on every post, and the translation and the link to the
// staff post on the post that carries the text (first).
func decorateMirror(p *Send, parts *mirrorParts, first bool) {
	if p.RelayOf != "" {
		parts.Prefix = relayPrefix
	}
	if first {
		parts.Translation = p.Translation
		parts.ReplyTo = relayReplyTarget(p.RelayOf)
	}
}

// textMessageIndex is the index of the planned Telegram message that carries
// the reply's own text: where the translation and the reply link belong.
func textMessageIndex(plan []tgMessage, text string) int {
	if text == "" {
		return 0
	}
	for i, m := range plan {
		if m.body == text {
			return i
		}
	}
	return 0
}
