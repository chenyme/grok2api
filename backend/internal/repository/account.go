package repository

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// AccountUpdates 表示批量账号更新中允许持久化的字段。
type AccountUpdates struct {
	Enabled          *bool
	Priority         *int
	MaxConcurrent    *int
	MinimumRemaining *float64
}

type AccountUpsertResult struct {
	ID      uint64
	Created bool
}

// AccountRepository 定义 OAuth 账号和额度快照持久化能力。
type AccountRepository interface {
	List(ctx context.Context, query AccountListQuery) ([]account.Credential, int64, error)
	Summarize(ctx context.Context, now time.Time) ([]AccountSummary, error)
	ListEnabled(ctx context.Context, provider account.Provider) ([]account.Credential, error)
	ListEnabledAccountIDs(ctx context.Context, provider account.Provider, refreshableOnly bool) ([]uint64, error)
	ListUnlinkedWebAccountIDs(ctx context.Context, limit int) ([]uint64, error)
	HasActive(ctx context.Context, provider account.Provider) (bool, error)
	ListRoutingCandidates(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string) ([]account.RoutingCandidate, error)
	Get(ctx context.Context, id uint64) (account.Credential, error)
	LinkWebToBuild(ctx context.Context, webAccountID, buildAccountID uint64) error
	GetBillings(ctx context.Context, accountIDs []uint64) (map[uint64]account.Billing, error)
	GetQuotaRecoveries(ctx context.Context, accountIDs []uint64) (map[uint64]account.QuotaRecovery, error)
	UpsertByIdentity(ctx context.Context, value account.Credential) (account.Credential, bool, error)
	Update(ctx context.Context, value account.Credential) (account.Credential, error)
	UpdateMany(ctx context.Context, ids []uint64, updates AccountUpdates) (int64, error)
	Delete(ctx context.Context, id uint64) error
	DeleteMany(ctx context.Context, ids []uint64) (int64, error)
	UpdateTokens(ctx context.Context, id uint64, accessToken, refreshToken string, expiresAt time.Time) (account.Credential, error)
	UpdateObservedModel(ctx context.Context, id uint64, model string, observedAt time.Time) error
	UpdateHealth(ctx context.Context, id uint64, failureCount int, cooldownUntil *time.Time, lastError string, success bool) error
	SaveBilling(ctx context.Context, value account.Billing) error
	GetBilling(ctx context.Context, accountID uint64) (account.Billing, error)
	GetQuotaRecovery(ctx context.Context, accountID uint64) (account.QuotaRecovery, error)
	SaveQuotaRecovery(ctx context.Context, value account.QuotaRecovery) error
	ClaimQuotaProbe(ctx context.Context, accountID uint64, now, leaseUntil time.Time) (bool, error)
	ClearQuotaRecovery(ctx context.Context, accountID uint64) error
	HasQuotaWindows(ctx context.Context, accountID uint64) (bool, error)
	GetQuotaWindows(ctx context.Context, accountIDs []uint64) (map[uint64][]account.QuotaWindow, error)
	ReplaceQuotaWindows(ctx context.Context, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow) error
	SaveQuotaWindows(ctx context.Context, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow) error
	UpsertManyByIdentity(ctx context.Context, values []account.Credential) ([]AccountUpsertResult, error)
	DecrementQuotaWindow(ctx context.Context, accountID uint64, mode string, now time.Time) (bool, error)
	ExhaustQuotaWindow(ctx context.Context, accountID uint64, mode string, resetAt *time.Time, now time.Time) error
	ListDueQuotaWindows(ctx context.Context, now time.Time, limit int) ([]account.QuotaWindow, error)
	ListQuotaRecoveryWindows(ctx context.Context, limit int) ([]account.QuotaWindow, error)
}
