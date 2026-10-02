package tgcustomer

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// MsgPair ties a message in a customer's chat to its twin in the customer's
// topic. Several rows can share one customer message (a long reply is mirrored
// as two posts, an edit adds a post), so lookups take the earliest row.
type MsgPair struct {
	ID             uint  `gorm:"primaryKey;autoIncrement"`
	CustomerChatID int64 `gorm:"not null;index:idx_tgc_pair_customer,priority:1"`
	CustomerMsgID  int64 `gorm:"not null;index:idx_tgc_pair_customer,priority:2"`
	TgThreadID     int64 `gorm:"not null;index:idx_tgc_pair_topic,priority:1"`
	TopicMsgID     int64 `gorm:"not null;index:idx_tgc_pair_topic,priority:2"`
	CreatedAt      time.Time
}

func (MsgPair) TableName() string { return "tg_customer_msg_pair" }

// Migrate creates the package's own table. It is idempotent and never touches
// the bridge's tables, so it is safe on a database that already holds
// ChatThreadPair and MsgIdPair rows.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&MsgPair{}) }

// Pairs stores and looks up MsgPair rows.
type Pairs struct{ db *gorm.DB }

func NewPairs(db *gorm.DB) *Pairs { return &Pairs{db: db} }

// Record stores a pair. The same pair twice is stored once.
func (p *Pairs) Record(chatID, customerMsgID, threadID, topicMsgID int64) error {
	var n int64
	err := p.db.Model(&MsgPair{}).
		Where("customer_chat_id = ? AND customer_msg_id = ? AND tg_thread_id = ? AND topic_msg_id = ?",
			chatID, customerMsgID, threadID, topicMsgID).
		Count(&n).Error
	if err != nil || n > 0 {
		return err
	}
	return p.db.Create(&MsgPair{
		CustomerChatID: chatID, CustomerMsgID: customerMsgID, TgThreadID: threadID, TopicMsgID: topicMsgID,
	}).Error
}

// TopicMsgFor returns the topic message that mirrors a customer's message.
func (p *Pairs) TopicMsgFor(chatID, customerMsgID int64) (threadID, topicMsgID int64, found bool, err error) {
	var row MsgPair
	err = p.db.Where("customer_chat_id = ? AND customer_msg_id = ?", chatID, customerMsgID).
		Order("id ASC").Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, 0, false, nil
	}
	return row.TgThreadID, row.TopicMsgID, err == nil, err
}

// CustomerMsgFor returns the customer-chat message that a topic message
// mirrors or answers.
func (p *Pairs) CustomerMsgFor(threadID, topicMsgID int64) (chatID, customerMsgID int64, found bool, err error) {
	var row MsgPair
	err = p.db.Where("tg_thread_id = ? AND topic_msg_id = ?", threadID, topicMsgID).
		Order("id ASC").Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, 0, false, nil
	}
	return row.CustomerChatID, row.CustomerMsgID, err == nil, err
}
