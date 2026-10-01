package agentlink

import (
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// OutboxRow is one Hub event waiting for the Agent's ack. Every event is
// written here before it is sent, so a crash or a dropped connection never
// loses one.
type OutboxRow struct {
	Seq       uint      `gorm:"primaryKey;autoIncrement"`
	EventID   string    `gorm:"uniqueIndex;size:64;not null"`
	JSON      string    `gorm:"type:text;not null"`
	CreatedAt time.Time `gorm:"index;not null"`
}

func (OutboxRow) TableName() string { return "agent_outbox" }

// CommandResultRow remembers the result of an Agent command so a retry of the
// same command id is answered without sending anything again.
type CommandResultRow struct {
	CommandID  string    `gorm:"primaryKey;size:64"`
	ResultJSON string    `gorm:"type:text;not null"`
	CreatedAt  time.Time `gorm:"index;not null"`
}

func (CommandResultRow) TableName() string { return "agent_command_result" }

// Migrate creates the link's own tables. It is idempotent and never touches
// the bridge's tables, so it is safe on a database that already holds
// ChatThreadPair and MsgIdPair rows.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&OutboxRow{}, &CommandResultRow{})
}

// Outbox is the persistent queue of Hub events.
type Outbox struct {
	db    *gorm.DB
	clock Clock
}

func NewOutbox(db *gorm.DB, clock Clock) *Outbox { return &Outbox{db: db, clock: clock} }

// Add stores an event. An event id already queued is left as it is (the same
// message processed twice must not be sent twice); ok is false in that case.
func (o *Outbox) Add(eventID string, frame []byte) (ok bool, err error) {
	row := OutboxRow{EventID: eventID, JSON: string(frame), CreatedAt: o.clock.Now().UTC()}
	res := o.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// Pending returns up to limit rows with Seq greater than after, in order.
func (o *Outbox) Pending(after uint, limit int) ([]OutboxRow, error) {
	var rows []OutboxRow
	err := o.db.Where("seq > ?", after).Order("seq ASC").Limit(limit).Find(&rows).Error
	return rows, err
}

// Delete removes an acknowledged event.
func (o *Outbox) Delete(eventID string) error {
	return o.db.Where("event_id = ?", eventID).Delete(&OutboxRow{}).Error
}

// DropOlderThan removes events created before cutoff and returns how many.
func (o *Outbox) DropOlderThan(cutoff time.Time) (int64, error) {
	res := o.db.Where("created_at < ?", cutoff.UTC()).Delete(&OutboxRow{})
	return res.RowsAffected, res.Error
}

func (o *Outbox) Count() (int64, error) {
	var n int64
	err := o.db.Model(&OutboxRow{}).Count(&n).Error
	return n, err
}

// commandRetention is how long command results are remembered (protocol §5).
const commandRetention = 24 * time.Hour

// CommandMemory stores command results by command id.
type CommandMemory struct {
	db    *gorm.DB
	clock Clock
}

func NewCommandMemory(db *gorm.DB, clock Clock) *CommandMemory {
	return &CommandMemory{db: db, clock: clock}
}

// Get returns the stored result for a command id, if one is remembered.
func (m *CommandMemory) Get(commandID string) (Result, bool, error) {
	var row CommandResultRow
	err := m.db.Where("command_id = ?", commandID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	var res Result
	if err := json.Unmarshal([]byte(row.ResultJSON), &res); err != nil {
		return Result{}, false, err
	}
	return res, true, nil
}

// Put remembers a result. The first result for an id wins.
func (m *CommandMemory) Put(res Result) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	row := CommandResultRow{CommandID: res.CommandID, ResultJSON: string(raw), CreatedAt: m.clock.Now().UTC()}
	return m.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

// Prune forgets results older than 24 hours and returns how many.
func (m *CommandMemory) Prune() (int64, error) {
	cutoff := m.clock.Now().Add(-commandRetention).UTC()
	res := m.db.Where("created_at < ?", cutoff).Delete(&CommandResultRow{})
	return res.RowsAffected, res.Error
}
