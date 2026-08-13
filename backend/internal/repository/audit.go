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
	ListDegradeEvents(ctx context.Context, query DegradeEventQuery) ([]DegradeEvent, error)
}

type DegradeEventQuery struct {
	Start           time.Time
	End             time.Time
	MinOutputTokens int64
	Limit           int
}

// DegradeEvent is a slim streaming-success audit plus optional account flags.
type DegradeEvent struct {
	ID                 uint64
	RequestID          string
	AccountID          *uint64
	AccountName        string
	Email              string
	Enabled            *bool
	BuildBotFlagSource int
	EgressNodeID       *uint64
	EgressNodeName     string
	OutputTokens       int64
	FirstTokenMS       int64
	DurationMS         int64
	CreatedAt          time.Time
	Model              string
}
