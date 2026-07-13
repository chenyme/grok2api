package relational

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

const runtimeSettingsKey = "gateway"

type runtimeSettingsPayload struct {
	Config                      settingsdomain.Config `json:"config"`
	EncryptedStatsigManualValue string                `json:"encryptedStatsigManualValue,omitempty"`
}

type RuntimeSettingsRepository struct {
	database *Database
	cipher   *security.Cipher
}

func NewRuntimeSettingsRepository(database *Database, cipher *security.Cipher) *RuntimeSettingsRepository {
	return &RuntimeSettingsRepository{database: database, cipher: cipher}
}

func (r *RuntimeSettingsRepository) Get(ctx context.Context) (settingsdomain.Config, time.Time, uint64, bool, error) {
	var row runtimeSettingsModel
	err := r.database.db.WithContext(ctx).Where("key = ?", runtimeSettingsKey).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return settingsdomain.Config{}, time.Time{}, 0, false, nil
	}
	if err != nil {
		return settingsdomain.Config{}, time.Time{}, 0, false, err
	}
	var payload runtimeSettingsPayload
	if err := json.Unmarshal([]byte(row.ValueJSON), &payload); err != nil {
		return settingsdomain.Config{}, time.Time{}, 0, false, fmt.Errorf("解析运行设置: %w", err)
	}
	manualValue, err := r.cipher.Decrypt(payload.EncryptedStatsigManualValue)
	if err != nil {
		return settingsdomain.Config{}, time.Time{}, 0, false, fmt.Errorf("解密 Statsig 手动值: %w", err)
	}
	payload.Config.ProviderWeb.StatsigManualValue = manualValue
	return payload.Config, row.UpdatedAt, row.Revision, true, nil
}

func (r *RuntimeSettingsRepository) Save(ctx context.Context, value settingsdomain.Config, expectedRevision uint64) (time.Time, uint64, error) {
	manualValue, err := r.cipher.Encrypt(value.ProviderWeb.StatsigManualValue)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("加密 Statsig 手动值: %w", err)
	}
	value.ProviderWeb.StatsigManualValue = ""
	payload, err := json.Marshal(runtimeSettingsPayload{Config: value, EncryptedStatsigManualValue: manualValue})
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("编码运行设置: %w", err)
	}
	now := time.Now().UTC()
	nextRevision := expectedRevision + 1
	if expectedRevision == 0 {
		row := runtimeSettingsModel{Key: runtimeSettingsKey, ValueJSON: string(payload), Revision: nextRevision, UpdatedAt: now}
		if err := r.database.db.WithContext(ctx).Create(&row).Error; err != nil {
			if errors.Is(mapError(err), repository.ErrConflict) {
				return time.Time{}, 0, repository.ErrConflict
			}
			return time.Time{}, 0, err
		}
		return now, nextRevision, nil
	}
	result := r.database.db.WithContext(ctx).Model(&runtimeSettingsModel{}).
		Where("key = ? AND revision = ?", runtimeSettingsKey, expectedRevision).
		Updates(map[string]any{"value_json": string(payload), "revision": nextRevision, "updated_at": now})
	if result.Error != nil {
		return time.Time{}, 0, result.Error
	}
	if result.RowsAffected != 1 {
		return time.Time{}, 0, repository.ErrConflict
	}
	return now, nextRevision, nil
}
