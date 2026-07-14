package repository

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// AuditRepository 定义请求元数据审计持久化能力。
type AuditRepository interface {
	Create(ctx context.Context, value audit.Record) error
	CreateBatch(ctx context.Context, values []audit.Record) error
	Get(ctx context.Context, id uint64) (audit.Record, error)
	List(ctx context.Context, offset, limit int) ([]audit.Record, int64, error)
	ListCursor(ctx context.Context, query AuditCursorQuery) ([]audit.Record, bool, error)
	Summarize(ctx context.Context, query AuditSummaryQuery) (audit.Summary, error)
	SumTokensByAccountsSince(ctx context.Context, accountIDs []uint64, since time.Time) (map[uint64]int64, error)
}
