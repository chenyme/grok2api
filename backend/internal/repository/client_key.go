package repository

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

// ClientKeyRepository 定义下游 API Key 持久化能力。
type ClientKeyRepository interface {
	List(ctx context.Context, query ClientKeyListQuery) ([]clientkey.Key, int64, error)
	Create(ctx context.Context, value clientkey.Key) (clientkey.Key, error)
	Get(ctx context.Context, id uint64) (clientkey.Key, error)
	GetByPrefix(ctx context.Context, prefix string) (clientkey.Key, error)
	Update(ctx context.Context, value clientkey.Key) (clientkey.Key, error)
	UpdateManyEnabled(ctx context.Context, ids []uint64, enabled bool) (int64, error)
	Delete(ctx context.Context, id uint64) error
	DeleteMany(ctx context.Context, ids []uint64) (int64, error)
	Touch(ctx context.Context, id uint64) error
}
