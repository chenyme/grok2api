package repository

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/dashboard"
)

// DashboardRepository 定义管理台概览所需的只读聚合查询。
type DashboardRepository interface {
	Snapshot(ctx context.Context, bucketBoundaries []time.Time, snapshotAt time.Time) (dashboard.Aggregate, error)
}
