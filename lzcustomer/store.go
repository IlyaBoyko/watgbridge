package lzcustomer

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// tokenRowID is the only row: a Hub serves one seller.
const tokenRowID = 1

// TokenRow is the stored authorization. The expiry times are computed from the
// lifetimes in Lazada's answer with the injected clock, never hard-coded.
type TokenRow struct {
	ID               uint `gorm:"primaryKey;autoIncrement:false"`
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	SellerID         string
	Country          string
	Account          string
	// UpdatedAt is when the tokens were issued; the access token's lifetime
	// is AccessExpiresAt minus this. Set from the clock, not by gorm.
	UpdatedAt time.Time `gorm:"autoUpdateTime:false"`
}

func (TokenRow) TableName() string { return "lazada_tokens" }

// Migrate creates the package's own tables. It is idempotent and never touches
// the bridge's tables, so it is safe on a database that already holds
// ChatThreadPair and MsgIdPair rows.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&TokenRow{}, &CursorRow{}, &MetaRow{}, &SentRow{})
}

// tokenStore reads and writes the single token row.
type tokenStore struct{ db *gorm.DB }

func (s tokenStore) load() (TokenRow, bool, error) {
	var row TokenRow
	err := s.db.Where("id = ?", tokenRowID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return TokenRow{}, false, nil
	}
	return row, err == nil, err
}

func (s tokenStore) save(row TokenRow) error {
	row.ID = tokenRowID
	return s.db.Save(&row).Error
}

/* ---------------------------------------------------------------- fetcher -- */

// CursorRow is how far one session has been read. A session has none until its
// first message is handled.
type CursorRow struct {
	SessionID     string `gorm:"primaryKey"`
	LastMessageID string
	// LastSendTime is the newest message handled, in Unix ms.
	LastSendTime int64
	Title        string
	// PostedMessageID is the last message whose topic post was made. When an
	// event write fails after the post, the retry must not post it twice.
	PostedMessageID string
	UpdatedAt       time.Time
}

func (CursorRow) TableName() string { return "lazada_cursors" }

// MetaRow is the channel's one row of bookkeeping.
type MetaRow struct {
	ID uint `gorm:"primaryKey;autoIncrement:false"`
	// StartedAt (Unix ms) is the first time the channel ran. Messages older
	// than it are never emitted: there is no backfill.
	StartedAt int64
	// FullScanWatermark (Unix ms) is the newest session activity a completed
	// full scan saw.
	FullScanWatermark int64
	// LastExpiryWarningDay is the local date ("2006-01-02") of the last
	// "authorization expires" post.
	LastExpiryWarningDay string
}

func (MetaRow) TableName() string { return "lazada_meta" }

const metaRowID = 1

// Origins of a row in lazada_sent.
const (
	OriginAgent = "agent"
	OriginTopic = "topic"
)

// SentRow is a seller message the Hub itself sent. It was mirrored and
// reported when it went out, so the fetcher must not report it again.
type SentRow struct {
	ID        uint   `gorm:"primaryKey;autoIncrement"`
	SessionID string `gorm:"not null;uniqueIndex:idx_lz_sent,priority:1"`
	MessageID string `gorm:"not null;uniqueIndex:idx_lz_sent,priority:2"`
	Origin    string
	CreatedAt time.Time `gorm:"index"`
}

func (SentRow) TableName() string { return "lazada_sent" }

// sentRetention is how long lazada_sent rows are kept: far longer than the
// fetcher can lag behind.
const sentRetention = 30 * 24 * time.Hour

// Store reads and writes the fetcher's tables.
type Store struct {
	db  *gorm.DB
	now func() time.Time
}

func NewStore(db *gorm.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

// cursor returns the session's row. The bool says there is a real cursor: a
// row that only holds a posted-message marker (written before the first
// message was fully handled) is not one.
func (s *Store) cursor(sessionID string) (CursorRow, bool, error) {
	var row CursorRow
	err := s.db.Where("session_id = ?", sessionID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CursorRow{}, false, nil
	}
	return row, err == nil && row.LastMessageID != "", err
}

func (s *Store) saveCursor(row CursorRow) error {
	row.UpdatedAt = s.now()
	return s.db.Save(&row).Error
}

// newestActivity is the newest message time any cursor holds, 0 for none.
func (s *Store) newestActivity() (int64, error) {
	var n *int64
	err := s.db.Model(&CursorRow{}).Select("MAX(last_send_time)").Scan(&n).Error
	if n == nil {
		return 0, err
	}
	return *n, err
}

// meta returns the bookkeeping row, creating it with StartedAt = startedAt the
// first time. created says this call was that first time.
func (s *Store) meta(startedAt int64) (row MetaRow, created bool, err error) {
	err = s.db.Where("id = ?", metaRowID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		row = MetaRow{ID: metaRowID, StartedAt: startedAt}
		return row, true, s.db.Create(&row).Error
	}
	return row, false, err
}

func (s *Store) saveMeta(row MetaRow) error {
	row.ID = metaRowID
	return s.db.Save(&row).Error
}

// RecordSent notes a message the Hub sent. The same pair twice is stored once.
func (s *Store) RecordSent(sessionID, messageID, origin string) error {
	return s.db.Where(SentRow{SessionID: sessionID, MessageID: messageID}).
		Attrs(SentRow{Origin: origin, CreatedAt: s.now()}).FirstOrCreate(&SentRow{}).Error
}

// WasSent reports whether the Hub sent that message.
func (s *Store) WasSent(sessionID, messageID string) (bool, error) {
	var n int64
	err := s.db.Model(&SentRow{}).Where("session_id = ? AND message_id = ?", sessionID, messageID).Count(&n).Error
	return n > 0, err
}

// PruneSent drops rows older than sentRetention.
func (s *Store) PruneSent() (int64, error) {
	res := s.db.Where("created_at < ?", s.now().Add(-sentRetention)).Delete(&SentRow{})
	return res.RowsAffected, res.Error
}
