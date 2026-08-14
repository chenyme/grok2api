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
	SummarizeDegrade(ctx context.Context, query DegradeSummaryQuery) (DegradeSummaryResult, error)
}

type DegradeSummaryQuery struct {
	Start           time.Time
	End             time.Time
	SoftTPS         float64
	HardTPS         float64
	MinGenerationMS int64
	MinOutputTokens int64
	FailClosed      bool
	AccountSearch   string
	AccountStatus   string
	AccountClass    string
	MinHits         int
	AccountOffset   int
	AccountLimit    int
	Buckets         []DegradeBucketRange
	RecentLimit     int
}

type DegradeBucketRange struct {
	Start time.Time
	End   time.Time
}

type DegradeSummaryResult struct {
	Totals        DegradeTotals
	Accounts      []DegradeAccount
	AccountTotal  int64
	AccountOffset int
	Nodes         []DegradeNode
	Buckets       []DegradeBucket
	Events        []DegradeEvent
}

type DegradeTotals struct {
	Hits         int64
	Accounts     int64
	StillEnabled int64
	Disabled     int64
	Deleted      int64
	Hard         int64
	Soft         int64
	Burst        int64
	MaxTPS       float64
}

type DegradeAccount struct {
	ID                 uint64
	Name               string
	Email              string
	Hits               int64
	MaxTPS             float64
	Burst              int64
	Soft               int64
	Hard               int64
	Last               time.Time
	Enabled            bool
	Found              bool
	BuildBotFlagSource int
	Nodes              []string
}

type DegradeNode struct {
	Name     string
	Hits     int64
	Accounts int64
	MaxTPS   float64
}

type DegradeBucket struct {
	Index  int
	Count  int64
	Severe int64
}

// DegradeEvent is a classified streaming-success audit.
type DegradeEvent struct {
	ID             uint64
	RequestID      string
	AccountID      *uint64
	AccountName    string
	EgressNodeID   *uint64
	EgressNodeName string
	OutputTokens   int64
	TPS            float64
	Class          string
	CreatedAt      time.Time
	Model          string
}
