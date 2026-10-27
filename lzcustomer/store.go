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

// Migrate creates the package's own table. It is idempotent and never touches
// the bridge's tables, so it is safe on a database that already holds
// ChatThreadPair and MsgIdPair rows.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&TokenRow{}) }

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
