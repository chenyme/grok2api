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

// BuildBotFlagCredential is the minimal encrypted credential projection used to
// rebuild persisted Build bot-risk metadata outside the request path.
type BuildBotFlagCredential struct {
	AccountID            uint64
	EncryptedAccessToken string
	StoredSource         int
}

type BuildBotFlagSourceUpdate struct {
	AccountID                    uint64
	ExpectedEncryptedAccessToken string
	Source                       int
}

// CredentialRefreshFailure is the bounded diagnostic state persisted for the
// latest failed OAuth refresh. Response must already be redacted by the
// provider adapter before it reaches persistence.
type CredentialRefreshFailure struct {
	Count     int
	RetryAt   time.Time
	Status    int
	Code      string
	Message   string
	Response  string
	Permanent bool
}

// LinkedDeleteResolution is the server-side expansion of root deletes with optional linked peers.
type LinkedDeleteResolution struct {
	RootIDs          []uint64
	FinalIDs         []uint64
	LinkedByProvider map[account.Provider]int
	// RootGroups maps each root to its final peers and defines the media-skip group boundary.
	RootGroups map[uint64][]uint64
	// PeerProviders records each peer provider for post-delete accounting.
	PeerProviders map[uint64]account.Provider
}

// LinkedDeleteOutcome contains actual rows deleted and root groups skipped atomically.
type LinkedDeleteOutcome struct {
	Resolution              LinkedDeleteResolution
	DeletedIDs              []uint64
	Deleted                 int64
	RootsDeleted            int64
	LinkedDeletedByProvider map[account.Provider]int64
	// SkippedRoots identifies groups protected by queued or in-progress video jobs.
	SkippedRoots []uint64
}

// CleanupPreview contains COUNT-only values for the cleanup confirmation dialog.
type CleanupPreview struct {
	RootsByStatus    map[string]int64
	RootCount        int64
	LinkedByProvider map[account.Provider]int64
	Total            int64
}

// ObservedModelWriter reports whether an observed model update changed the authoritative row.
type ObservedModelWriter interface {
	UpdateObservedModelIfNewer(ctx context.Context, id uint64, model string, observedAt time.Time) (bool, error)
}

// RoutingLayerRepository separates reusable account state from model overlays.
type RoutingLayerRepository interface {
	ListRoutingAccountBases(ctx context.Context, provider account.Provider, quotaMode string) ([]account.RoutingAccountBase, error)
	ListRoutingAccountOverlays(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel string) (account.RoutingOverlaySnapshot, error)
}

// AccountRepository 定义 OAuth 账号和额度快照持久化能力。
type AccountRepository interface {
	List(ctx context.Context, query AccountListQuery) ([]account.Credential, int64, error)
	// ListProviderAccountBatch 以 ID 游标取一批账号；total 仅在 afterID 为 0 时返回。
	ListProviderAccountBatch(ctx context.Context, provider account.Provider, afterID uint64, limit int) ([]account.Credential, int64, error)
	Summarize(ctx context.Context, now time.Time) ([]AccountSummary, error)
	ListEnabled(ctx context.Context, provider account.Provider) ([]account.Credential, error)
	ListEnabledAccountIDs(ctx context.Context, provider account.Provider, refreshableOnly bool) ([]uint64, error)
	// ListEnabledCredentialRefreshAccountIDs includes enabled active and
	// reauthRequired accounts so an explicit administrator refresh can retry a
	// previously rejected refresh token once.
	ListEnabledCredentialRefreshAccountIDs(ctx context.Context, provider account.Provider, refreshableOnly bool) ([]uint64, error)
	CountProviderAccountsByIDs(ctx context.Context, provider account.Provider, ids []uint64) (int64, error)
	// CountAvailableAmong counts how many of the given account IDs currently match the
	// same "available/schedulable" predicate used by Summarize for the provider.
	CountAvailableAmong(ctx context.Context, provider account.Provider, ids []uint64, now time.Time) (int64, error)
	// FilterMissingBuildConversionIDs 从指定账号中排除已经关联 Build 的 Web 账号。
	FilterMissingBuildConversionIDs(ctx context.Context, ids []uint64) ([]uint64, error)
	// ListUnlinkedWebAccountIDs 以 ID 游标取未关联 Web 账号；total 仅在 afterID 为 0 时返回。
	ListUnlinkedWebAccountIDs(ctx context.Context, afterID uint64, limit int) ([]uint64, int64, error)
	// ListMissingConsoleSyncAccounts 从指定账号中排除已有对应 Console 账号的 Web 账号。
	ListMissingConsoleSyncAccounts(ctx context.Context, ids []uint64) ([]account.Credential, error)
	// ListMissingConsoleSyncBatch 以 ID 游标取缺少 Console 账号的 Web 账号；total/skipped 仅在 afterID 为 0 时返回。
	ListMissingConsoleSyncBatch(ctx context.Context, afterID uint64, limit int) ([]account.Credential, int64, int64, error)
	HasActive(ctx context.Context, provider account.Provider) (bool, error)
	ListRoutingCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string) ([]account.RoutingCandidate, error)
	GetCredentialMaterial(ctx context.Context, accountID uint64, provider account.Provider) (account.CredentialMaterial, error)
	Get(ctx context.Context, id uint64) (account.Credential, error)
	LinkWebToBuild(ctx context.Context, webAccountID, buildAccountID uint64) error
	GetBillings(ctx context.Context, accountIDs []uint64) (map[uint64]account.Billing, error)
	GetQuotaRecoveries(ctx context.Context, accountIDs []uint64) (map[uint64]account.QuotaRecovery, error)
	UpsertByIdentity(ctx context.Context, value account.Credential) (account.Credential, bool, error)
	Update(ctx context.Context, value account.Credential) (account.Credential, error)
	UpdateMany(ctx context.Context, provider account.Provider, ids []uint64, updates AccountUpdates) (int64, error)
	Delete(ctx context.Context, id uint64) error
	DeleteMany(ctx context.Context, ids []uint64) (int64, error)
	// ResolveLinkedDeleteIDs expands root account IDs with one-hop (or Build/Console two-hop via Web)
	// peers from link tables for optional linked deletion. It never guesses by email/name/userId.
	ResolveLinkedDeleteIDs(ctx context.Context, provider account.Provider, rootIDs []uint64, targets []account.Provider) (LinkedDeleteResolution, error)
	// DeleteManyWithLinked locks roots, resolves linked peers, checks media jobs, and deletes
	// the final set inside a single DB transaction (avoids resolve/delete TOCTOU).
	// skipMedia=false rejects on active media; skipMedia=true skips the complete root group.
	DeleteManyWithLinked(ctx context.Context, provider account.Provider, rootIDs []uint64, targets []account.Provider, skipMedia bool) (LinkedDeleteOutcome, error)
	// DeleteAccountStatusBatchWithLinked selects at most limit roots by state and ID cursor,
	// expands links, skips protected groups, and returns the candidate count and next cursor.
	DeleteAccountStatusBatchWithLinked(ctx context.Context, provider account.Provider, status string, now time.Time, afterID uint64, limit int, targets []account.Provider) (LinkedDeleteOutcome, int, uint64, error)
	// CountCleanupWithLinked returns root and linked-peer counts using SQL COUNT queries only.
	CountCleanupWithLinked(ctx context.Context, provider account.Provider, statuses []string, now time.Time, targets []account.Provider) (CleanupPreview, error)
	// ListAutoCleanReauthCandidates 以 ID 游标列出达到清理年龄的 reauthRequired 账号。
	ListAutoCleanReauthCandidates(ctx context.Context, markedBefore time.Time, includeDisabled bool, afterID uint64, limit int) ([]uint64, error)
	// DeleteAutoCleanReauthCandidates 在事务内重新校验状态与年龄并跳过活动视频任务，返回实际删除 ID。
	DeleteAutoCleanReauthCandidates(ctx context.Context, markedBefore time.Time, includeDisabled bool, candidateIDs []uint64) ([]uint64, error)
	UpdateTokens(ctx context.Context, id uint64, accessToken, refreshToken string, expiresAt time.Time, buildBotFlagSource int) (account.Credential, error)
	BackfillCredentialRefreshSchedules(ctx context.Context, now time.Time, limit int) (int, error)
	ListCriticalCredentialRefreshIDs(ctx context.Context, now, expiresBefore time.Time, limit int) ([]uint64, error)
	ListDueCredentialRefreshIDs(ctx context.Context, now time.Time, limit int) ([]uint64, error)
	NextCredentialRefreshDueAt(ctx context.Context) (*time.Time, error)
	UpdateCredentialRefreshFailure(ctx context.Context, id uint64, failure CredentialRefreshFailure) error
	UpdateObservedModel(ctx context.Context, id uint64, model string, observedAt time.Time) error
	UpdateHealth(ctx context.Context, id uint64, provider account.Provider, failureCount int, cooldownUntil *time.Time, lastError string, success bool) error
	// TouchLastUsed persists request activity without changing routing health or
	// invalidating candidate snapshots.
	TouchLastUsed(ctx context.Context, id uint64, usedAt time.Time) error
	// MarkBuildAPIFallback 幂等写入 Build 账号的 XAI 推理回退标记；非 Build 账号返回错误。
	MarkBuildAPIFallback(ctx context.Context, id uint64, enabled bool) error
	// MarkWebNSFWEnabled 幂等记录 Web 账号首次确认 NSFW 已开启的时间。
	MarkWebNSFWEnabled(ctx context.Context, id uint64, enabledAt time.Time) error
	// MarkWebTermsAccepted 幂等记录 Web 账号已完整接受的产品协议版本与时间。
	MarkWebTermsAccepted(ctx context.Context, id uint64, version int, acceptedAt time.Time) error
	// MarkWebBirthDateSet 幂等记录 Web 账号首次确认生日已设置的时间。
	MarkWebBirthDateSet(ctx context.Context, id uint64, setAt time.Time) error
	UpsertModelQuotaBlock(ctx context.Context, value account.ModelQuotaBlock) error
	PruneExpiredModelQuotaBlocks(ctx context.Context, now time.Time, limit int) (int64, error)
	SaveBilling(ctx context.Context, value account.Billing) error
	GetBilling(ctx context.Context, accountID uint64) (account.Billing, error)
	GetQuotaRecovery(ctx context.Context, accountID uint64) (account.QuotaRecovery, error)
	SaveQuotaRecovery(ctx context.Context, value account.QuotaRecovery) error
	ClaimQuotaProbe(ctx context.Context, accountID uint64, now, leaseUntil time.Time) (bool, error)
	ClearQuotaRecovery(ctx context.Context, accountID uint64) error
	ResetQuotaState(ctx context.Context, provider account.Provider, accountIDs []uint64) error
	ResetProviderQuotaState(ctx context.Context, provider account.Provider, activeOnly bool) (int64, error)
	HasQuotaWindows(ctx context.Context, accountID uint64) (bool, error)
	GetQuotaWindows(ctx context.Context, accountIDs []uint64) (map[uint64][]account.QuotaWindow, error)
	ReplaceQuotaWindows(ctx context.Context, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow) error
	ReplaceQuotaWindowGroup(ctx context.Context, accountID uint64, syncedAt time.Time, modes []string, values []account.QuotaWindow) error
	SaveQuotaWindows(ctx context.Context, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow) error
	UpsertManyByIdentity(ctx context.Context, values []account.Credential) ([]AccountUpsertResult, error)
	DecrementQuotaWindow(ctx context.Context, accountID uint64, mode string, now time.Time) (bool, error)
	ExhaustQuotaWindow(ctx context.Context, accountID uint64, mode string, resetAt *time.Time, now time.Time) error
	ListDueQuotaWindows(ctx context.Context, now time.Time, limit int) ([]account.QuotaWindow, error)
	ListQuotaRecoveryWindows(ctx context.Context, limit int) ([]account.QuotaWindow, error)
	ListStaleWebQuotaAccountIDs(ctx context.Context, before time.Time, limit int) ([]uint64, error)
}
