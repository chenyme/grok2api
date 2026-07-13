package repository

import (
	"context"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

// RuntimeSettingsRepository 定义运行设置的单实例持久化边界。
type RuntimeSettingsRepository interface {
	Get(ctx context.Context) (settingsdomain.Config, time.Time, uint64, bool, error)
	Save(ctx context.Context, value settingsdomain.Config, expectedRevision uint64) (time.Time, uint64, error)
}
