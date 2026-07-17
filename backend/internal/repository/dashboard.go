package repository

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/dashboard"
)

// DashboardSnapshotWindow 定义主趋势、上一周期与活动热力图所需的时间边界。
type DashboardSnapshotWindow struct {
	BucketBoundaries   []time.Time
	ActivityBoundaries []time.Time
}

// DashboardRepository 定义管理台概览所需的只读聚合查询。
type DashboardRepository interface {
	Snapshot(ctx context.Context, window DashboardSnapshotWindow, snapshotAt time.Time) (dashboard.Aggregate, error)
}
