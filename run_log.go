package agents

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// RunLogRecord is one persisted agent execution. Optional payload fields are
// populated only when the corresponding LoggingOptions flag is enabled.
type RunLogRecord struct {
	ID                 uint `gorm:"primaryKey"`
	StartedAt          time.Time
	EndedAt            time.Time
	ExpiresAt          time.Time `gorm:"index"`
	AgentKey           string    `gorm:"size:128;index"`
	AgentVersion       string    `gorm:"size:64;index"`
	Model              string    `gorm:"size:128"`
	PreviousResponseID string    `gorm:"size:128"`
	ResponseIDsJSON    string    `gorm:"type:text"`
	MetadataJSON       string    `gorm:"type:text"`
	ContextJSON        string    `gorm:"type:text"`
	ResponseJSON       string    `gorm:"type:text"`
	ErrorMessage       string    `gorm:"type:text"`
	InputTokens        int64
	CachedTokens       int64
	CacheWriteTokens   int64
	OutputTokens       int64
	TotalTokens        int64
}

func (RunLogRecord) TableName() string { return "agent_run_logs" }

// RunLogger persists completed (or failed) agent run records.
type RunLogger interface {
	SaveRun(ctx context.Context, record RunLogRecord) error
}

// GormRunLogger stores run records using a caller-configured GORM database.
type GormRunLogger struct {
	db *gorm.DB
}

func NewGormRunLogger(db *gorm.DB) *GormRunLogger {
	return &GormRunLogger{db: db}
}

func (l *GormRunLogger) AutoMigrate(ctx context.Context) error {
	if err := l.checkDB(); err != nil {
		return err
	}
	return l.db.WithContext(ctx).AutoMigrate(&RunLogRecord{})
}

func (l *GormRunLogger) SaveRun(ctx context.Context, record RunLogRecord) error {
	if err := l.checkDB(); err != nil {
		return err
	}
	if err := l.db.WithContext(ctx).Create(&record).Error; err != nil {
		return err
	}
	return nil
}

// DeleteExpired removes records whose configured expiry time has passed.
func (l *GormRunLogger) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	if err := l.checkDB(); err != nil {
		return 0, err
	}
	result := l.db.WithContext(ctx).Where("expires_at > ? AND expires_at <= ?", time.Time{}, before).Delete(&RunLogRecord{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

func (l *GormRunLogger) checkDB() error {
	if l == nil || l.db == nil {
		return errors.New("run logger has no gorm database")
	}
	return nil
}
