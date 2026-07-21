package repository

import (
	"context"
	"time"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
)

type ResponseCleanupResult struct {
	OwnershipDeleted int64
	WebStateDeleted  int64
	HasMore          bool
}

// ResponseRepository 定义 Responses 资源与上游账号归属的持久化边界。
type ResponseRepository interface {
	Save(ctx context.Context, value inferencedomain.ResponseOwnership) error
	Get(ctx context.Context, responseID string, clientKeyID uint64, now time.Time) (inferencedomain.ResponseOwnership, error)
	Delete(ctx context.Context, responseID string, clientKeyID uint64) error
	DeleteExpired(ctx context.Context, now time.Time, ownershipLimit, webStateLimit int) (ResponseCleanupResult, error)
	SaveWebState(ctx context.Context, value inferencedomain.WebResponseState) error
	GetWebState(ctx context.Context, responseID string, now time.Time) (inferencedomain.WebResponseState, error)
	DeleteWebState(ctx context.Context, responseID string) error
}
