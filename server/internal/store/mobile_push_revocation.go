package store

import "time"

// MobilePushEndpointRevocation survives deletion of its original grant. ID also
// preserves the associated data required to decrypt SendTokenCiphertext.
type MobilePushEndpointRevocation struct {
	ID                  string    `gorm:"type:uuid;primaryKey"`
	InstallationID      string    `gorm:"type:uuid;not null;index"`
	SendTokenCiphertext []byte    `gorm:"not null"`
	NextAttemptAt       time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	Attempts            int64     `gorm:"not null;default:0"`
	CreatedAt           time.Time `gorm:"not null"`
}

func (MobilePushEndpointRevocation) TableName() string { return "mobile_push_endpoint_revocations" }
