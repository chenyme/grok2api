package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

type accountLease struct {
	Credential     account.Credential
	Billing        *account.Billing
	QuotaProbe     bool
	QuotaProbeKind account.QuotaRecoveryKind
	QuotaMode      string
	release        func()
}

const quotaProbeLease = 5 * time.Minute
const successPersistInterval = 30 * time.Second
const candidateCacheTTL = time.Second

type candidateSnapshot struct {
	values    []account.RoutingCandidate
	expiresAt time.Time
}

type candidateCacheKey struct {
	provider      account.Provider
	upstreamModel string
	quotaMode     string
}

type SelectionUnavailableReason string

const (
	SelectionNoAccounts       SelectionUnavailableReason = "no_accounts"
	SelectionUnsupportedModel SelectionUnavailableReason = "unsupported_model"
	SelectionCooling          SelectionUnavailableReason = "cooling"
	SelectionModelCooling     SelectionUnavailableReason = "model_cooling"
	SelectionQuotaExhausted   SelectionUnavailableReason = "quota_exhausted"
	SelectionSaturated        SelectionUnavailableReason = "saturated"
)

// SelectionUnavailableError 保留选号失败的真实原因，避免所有情况都退化成模糊的 503。
type SelectionUnavailableError struct {
	Reason     SelectionUnavailableReason
	RetryAfter time.Duration
}

func (e *SelectionUnavailableError) Error() string {
	if e == nil {
		return "没有可用上游账号"
	}
	switch e.Reason {
	case SelectionUnsupportedModel:
		return "当前账号池不支持该模型"
	case SelectionCooling:
		return "可用上游账号正在冷却"
	case SelectionModelCooling:
		return "可用上游账号的目标模型正在冷却"
	case SelectionQuotaExhausted:
		return "可用上游账号额度等待恢复"
	case SelectionSaturated:
		return "可用上游账号均达到并发上限"
	default:
		return "没有可用上游账号"
	}
}

func (l *accountLease) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

// Selector 实现可替换的 balanced 账号选择策略。
type Selector struct {
	accounts       repository.AccountRepository
	concurrency    repository.ConcurrencyLimiter
	sticky         repository.StickySessionRepository
	stickyTTL      time.Duration
	cooldownBase   time.Duration
	cooldownMax    time.Duration
	capacityWait   time.Duration
	mu             sync.Mutex
	leaseWakeMu    sync.Mutex
	leaseWake      chan struct{}
	lastSelectedAt map[uint64]time.Time
	lastSuccessAt  map[uint64]time.Time
	candidates     map[candidateCacheKey]candidateSnapshot
	candidateLoads singleflight.Group
	tierOrders     interface {
		TierOrder(account.Provider, string) []account.WebTier
	}
}

func NewSelector(accounts repository.AccountRepository, concurrency repository.ConcurrencyLimiter, sticky repository.StickySessionRepository, tierOrders interface {
	TierOrder(account.Provider, string) []account.WebTier
}, stickyTTL, cooldownBase, cooldownMax time.Duration, capacityWait ...time.Duration) *Selector {
	wait := time.Duration(0)
	if len(capacityWait) > 0 && capacityWait[0] > 0 {
		wait = capacityWait[0]
	}
	return &Selector{accounts: accounts, concurrency: concurrency, sticky: sticky, tierOrders: tierOrders, stickyTTL: stickyTTL, cooldownBase: cooldownBase, cooldownMax: cooldownMax, capacityWait: wait, leaseWake: make(chan struct{}), lastSelectedAt: make(map[uint64]time.Time), lastSuccessAt: make(map[uint64]time.Time), candidates: make(map[candidateCacheKey]candidateSnapshot)}
}

func (s *Selector) UpdateConfig(stickyTTL, cooldownBase, cooldownMax time.Duration, capacityWait ...time.Duration) {
	s.mu.Lock()
	s.stickyTTL = stickyTTL
	s.cooldownBase = cooldownBase
	s.cooldownMax = cooldownMax
	if len(capacityWait) > 0 {
		s.capacityWait = max(time.Duration(0), capacityWait[0])
	}
	s.mu.Unlock()
}

func (s *Selector) routingConfig() (time.Duration, time.Duration, time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stickyTTL, s.cooldownBase, s.cooldownMax, s.capacityWait
}

func (s *Selector) Acquire(ctx context.Context, provider account.Provider, upstreamModel, quotaMode, promptCacheKey string, excluded map[uint64]bool, allowQuotaProbe bool) (*accountLease, error) {
	now := time.Now().UTC()
	stickyKey := promptCacheStickyKey(promptCacheKey)
	values, err := s.loadCandidates(ctx, provider, upstreamModel, quotaMode, now)
	if err != nil {
		return nil, err
	}
	normalCandidates := make([]account.RoutingCandidate, 0, len(values))
	probeCandidates := make([]account.RoutingCandidate, 0, len(values))
	supportedCandidates := 0
	consideredCandidates := 0
	coolingCandidates := 0
	modelCoolingCandidates := 0
	quotaCandidates := 0
	var earliestRetry time.Time
	for _, candidate := range values {
		value := candidate.Credential
		if excluded[value.ID] || value.AuthStatus != account.AuthStatusActive {
			continue
		}
		consideredCandidates++
		if candidate.ModelCapabilityKnown && !candidate.SupportsModel {
			continue
		}
		supportedCandidates++
		if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
			modelCoolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, candidate.ModelQuotaBlock.CooldownUntil, now)
			continue
		}
		if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
			coolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, *value.CooldownUntil, now)
			continue
		}
		quotaRecovery := candidate.QuotaRecovery
		if quotaRecovery != nil && quotaRecovery.Status != account.QuotaRecoveryStatusActive {
			if allowQuotaProbe && quotaRecovery.NextProbeAt != nil && !now.Before(*quotaRecovery.NextProbeAt) {
				probeCandidates = append(probeCandidates, candidate)
			} else {
				quotaCandidates++
				if quotaRecovery.NextProbeAt != nil {
					earliestRetry = earlierFuture(earliestRetry, *quotaRecovery.NextProbeAt, now)
				}
			}
			continue
		}
		if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
			quotaCandidates++
			continue
		}
		if candidate.QuotaWindow != nil && candidate.QuotaWindow.Remaining <= 0 {
			quotaCandidates++
			if candidate.QuotaWindow.ResetAt != nil {
				earliestRetry = earlierFuture(earliestRetry, *candidate.QuotaWindow.ResetAt, now)
			}
			continue
		}
		normalCandidates = append(normalCandidates, candidate)
	}
	if len(normalCandidates) == 0 && len(probeCandidates) == 0 {
		reason := SelectionNoAccounts
		switch {
		case consideredCandidates > 0 && supportedCandidates == 0:
			reason = SelectionUnsupportedModel
		case modelCoolingCandidates > 0:
			reason = SelectionModelCooling
		case coolingCandidates > 0:
			reason = SelectionCooling
		case quotaCandidates > 0:
			reason = SelectionQuotaExhausted
		}
		return nil, &SelectionUnavailableError{Reason: reason, RetryAfter: retryDelay(now, earliestRetry)}
	}
	if len(probeCandidates) > 0 {
		plan, err := s.planCandidates(ctx, probeCandidates, now, s.resolveTierOrder(provider, upstreamModel))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			lease, err := s.claimAccountSlot(ctx, candidate.Credential)
			if err != nil {
				return nil, err
			}
			if lease == nil {
				continue
			}
			claimed, err := s.accounts.ClaimQuotaProbe(ctx, candidate.Credential.ID, now, now.Add(quotaProbeLease))
			if err != nil || !claimed {
				lease.Release()
				if err != nil {
					return nil, err
				}
				continue
			}
			lease.QuotaProbe = true
			lease.QuotaProbeKind = candidate.QuotaRecovery.Kind
			lease.Billing = candidate.Billing
			return lease, nil
		}
	}
	if stickyKey != "" {
		stickyID, ok, err := s.sticky.Get(ctx, stickyKey, now)
		if err != nil {
			return nil, fmt.Errorf("读取会话粘滞状态: %w", err)
		}
		if ok {
			for _, candidate := range normalCandidates {
				if candidate.Credential.ID == stickyID {
					lease, acquireErr := s.claimAccountSlot(ctx, candidate.Credential)
					if acquireErr != nil {
						return nil, acquireErr
					}
					if lease != nil {
						lease.Billing = candidate.Billing
						lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
						return lease, nil
					}
				}
			}
		}
	}
	_, _, _, capacityWait := s.routingConfig()
	waitDeadline := time.Now().Add(capacityWait)
	for {
		currentTime := time.Now().UTC()
		plan, err := s.planCandidates(ctx, normalCandidates, currentTime, s.resolveTierOrder(provider, upstreamModel))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			lease, err := s.claimAccountSlot(ctx, candidate.Credential)
			if err != nil {
				return nil, err
			}
			if lease == nil {
				continue
			}
			if stickyKey != "" {
				stickyTTL, _, _, _ := s.routingConfig()
				if err := s.sticky.Set(ctx, stickyKey, candidate.Credential.ID, currentTime.Add(stickyTTL)); err != nil {
					lease.Release()
					return nil, fmt.Errorf("写入会话粘滞状态: %w", err)
				}
			}
			lease.Billing = candidate.Billing
			lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
			return lease, nil
		}
		if capacityWait <= 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, waitDeadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

// promptCacheStickyKey 将调用方缓存键压缩为固定长度，仅用于本地账号粘滞索引。
func promptCacheStickyKey(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// AcquirePinned 为 previous_response_id 等账号归属请求获取指定账号租约。
func (s *Selector) AcquirePinned(ctx context.Context, provider account.Provider, accountID uint64, upstreamModel, quotaMode string, inference bool) (*accountLease, error) {
	now := time.Now().UTC()
	values, err := s.loadCandidates(ctx, provider, upstreamModel, quotaMode, now)
	if err != nil {
		return nil, err
	}
	for _, candidate := range values {
		value := candidate.Credential
		if value.ID != accountID {
			continue
		}
		if !value.Enabled || value.AuthStatus != account.AuthStatusActive {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
		if inference {
			if candidate.ModelCapabilityKnown && !candidate.SupportsModel {
				return nil, &SelectionUnavailableError{Reason: SelectionUnsupportedModel}
			}
			if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
				return nil, &SelectionUnavailableError{Reason: SelectionModelCooling, RetryAfter: retryDelay(now, candidate.ModelQuotaBlock.CooldownUntil)}
			}
			if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
				return nil, &SelectionUnavailableError{Reason: SelectionCooling, RetryAfter: retryDelay(now, *value.CooldownUntil)}
			}
			if recovery := candidate.QuotaRecovery; recovery != nil && recovery.Status != account.QuotaRecoveryStatusActive {
				if recovery.NextProbeAt == nil || now.Before(*recovery.NextProbeAt) {
					var retryAfter time.Duration
					if recovery.NextProbeAt != nil {
						retryAfter = retryDelay(now, *recovery.NextProbeAt)
					}
					return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
				}
				lease, err := s.acquirePinnedCapacity(ctx, value)
				if err != nil {
					return nil, err
				}
				claimed, err := s.accounts.ClaimQuotaProbe(ctx, value.ID, now, now.Add(quotaProbeLease))
				if err != nil || !claimed {
					lease.Release()
					if err != nil {
						return nil, err
					}
					return nil, fmt.Errorf("绑定的上游账号恢复探测已被占用")
				}
				lease.QuotaProbe = true
				lease.QuotaProbeKind = recovery.Kind
				lease.Billing = candidate.Billing
				return lease, nil
			}
			if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
				return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted}
			}
			if candidate.QuotaWindow != nil && candidate.QuotaWindow.Remaining <= 0 {
				var retryAfter time.Duration
				if candidate.QuotaWindow.ResetAt != nil {
					retryAfter = retryDelay(now, *candidate.QuotaWindow.ResetAt)
				}
				return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
			}
		}
		lease, err := s.acquirePinnedCapacity(ctx, value)
		if err != nil {
			return nil, err
		}
		lease.Billing = candidate.Billing
		lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
		return lease, nil
	}
	return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
}

func effectiveQuotaMode(candidate account.RoutingCandidate, fallback string) string {
	if candidate.QuotaWindow != nil && candidate.QuotaWindow.Mode == "weekly" {
		return "weekly"
	}
	return fallback
}

func (s *Selector) MarkSuccess(ctx context.Context, credential account.Credential) {
	s.markSuccess(ctx, credential, true)
}

func (s *Selector) markSuccess(ctx context.Context, credential account.Credential, quotaProbe bool) {
	now := time.Now().UTC()
	persist := credential.FailureCount > 0 || credential.CooldownUntil != nil || credential.LastError != ""
	s.mu.Lock()
	if last := s.lastSuccessAt[credential.ID]; last.IsZero() || now.Sub(last) >= successPersistInterval {
		persist = true
	}
	if persist {
		s.lastSuccessAt[credential.ID] = now
	}
	s.mu.Unlock()
	if persist {
		_ = s.accounts.UpdateHealth(ctx, credential.ID, 0, nil, "", true)
	}
	if quotaProbe {
		_ = s.accounts.ClearQuotaRecovery(ctx, credential.ID)
	}
	if quotaProbe || credential.FailureCount > 0 || credential.CooldownUntil != nil || credential.LastError != "" {
		s.invalidateCandidates(credential.Provider)
	}
}

func (s *Selector) MarkFreeQuotaExhausted(ctx context.Context, credential account.Credential, used, limit int64) {
	now := time.Now().UTC()
	nextProbeAt := now.Add(24 * time.Hour)
	_ = s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: credential.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted,
		ConfirmedUsed: used, ConfirmedLimit: limit, ExhaustedAt: &now,
		NextProbeAt: &nextProbeAt, LastConfirmedAt: &now, UpdatedAt: now,
	})
	_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	s.invalidateCandidates(credential.Provider)
}

func (s *Selector) MarkModelQuotaExhausted(ctx context.Context, credential account.Credential, upstreamModel string, retryAfter time.Duration) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		s.MarkFreeQuotaExhausted(ctx, credential, 0, 0)
		return
	}
	if retryAfter <= 0 {
		retryAfter = 24 * time.Hour
	}
	until := time.Now().UTC().Add(retryAfter)
	_ = s.accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: upstreamModel, Reason: "model_quota_depleted", CooldownUntil: until, UpdatedAt: time.Now().UTC(),
	})
	s.invalidateCandidates(credential.Provider)
}

// MarkPaidQuotaExhausted 使用已知真实账期将付费账号移出号池，到期后才允许 Billing 探测。
func (s *Selector) MarkPaidQuotaExhausted(ctx context.Context, credential account.Credential, billing *account.Billing) bool {
	if billing == nil || (billing.MonthlyLimit <= 0 && billing.OnDemandCap <= 0 && billing.OnDemandUsed <= 0 && billing.PrepaidBalance <= 0 && billing.CreditUsagePercent <= 0) {
		return false
	}
	periodEnd, ok := billing.PeriodEnd()
	if !ok {
		return false
	}
	now := time.Now().UTC()
	_ = s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: credential.ID, Kind: account.QuotaRecoveryKindPaid, Status: account.QuotaRecoveryStatusExhausted,
		ExhaustedAt: &now, NextProbeAt: &periodEnd, LastConfirmedAt: &now, UpdatedAt: now,
	})
	_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	s.invalidateCandidates(credential.Provider)
	return true
}

// MarkQuotaStateChanged 在 Billing 探测改变持久化额度状态后立即失效候选快照。
func (s *Selector) MarkQuotaStateChanged(provider account.Provider) { s.invalidateCandidates(provider) }

// ConsumeQuota 将成功请求的本地额度变化应用到候选快照，避免为单账号变化清空整个 Provider 缓存。
func (s *Selector) ConsumeQuota(provider account.Provider, accountID uint64, mode string, amount int) {
	if accountID == 0 || mode == "" || mode == "weekly" || amount <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, snapshot := range s.candidates {
		if key.provider != provider {
			continue
		}
		for index := range snapshot.values {
			candidate := &snapshot.values[index]
			if candidate.Credential.ID != accountID || candidate.QuotaWindow == nil || candidate.QuotaWindow.Mode != mode {
				continue
			}
			window := *candidate.QuotaWindow
			window.Remaining = max(0, window.Remaining-amount)
			window.UpdatedAt = time.Now().UTC()
			candidate.QuotaWindow = &window
		}
		s.candidates[key] = snapshot
	}
}

func (s *Selector) MarkFailure(ctx context.Context, credential account.Credential, status int, retryAfter time.Duration) {
	failureCount := credential.FailureCount + 1
	_, cooldownBase, cooldownMax, _ := s.routingConfig()
	cooldown := cooldownBase
	for i := 1; i < failureCount && cooldown < cooldownMax; i++ {
		cooldown *= 2
	}
	if cooldown > cooldownMax {
		cooldown = cooldownMax
	}
	if retryAfter > cooldown {
		cooldown = retryAfter
	}
	until := time.Now().UTC().Add(cooldown)
	_ = s.accounts.UpdateHealth(ctx, credential.ID, failureCount, &until, fmt.Sprintf("upstream status %d", status), false)
	s.invalidateCandidates(credential.Provider)
	if status == 401 || status == 402 || status == 403 || status == 429 {
		_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	}
}

func (s *Selector) loadCandidates(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string, now time.Time) ([]account.RoutingCandidate, error) {
	key := candidateCacheKey{provider: provider, upstreamModel: upstreamModel, quotaMode: quotaMode}
	s.mu.Lock()
	if snapshot, ok := s.candidates[key]; ok && now.Before(snapshot.expiresAt) {
		values := append([]account.RoutingCandidate(nil), snapshot.values...)
		s.mu.Unlock()
		return values, nil
	}
	s.mu.Unlock()
	loadKey := string(provider) + "\x00" + upstreamModel + "\x00" + quotaMode
	loaded, err, _ := s.candidateLoads.Do(loadKey, func() (any, error) {
		checkTime := time.Now().UTC()
		s.mu.Lock()
		if snapshot, ok := s.candidates[key]; ok && checkTime.Before(snapshot.expiresAt) {
			values := append([]account.RoutingCandidate(nil), snapshot.values...)
			s.mu.Unlock()
			return values, nil
		}
		s.mu.Unlock()
		values, err := s.accounts.ListRoutingCandidates(ctx, provider, upstreamModel, quotaMode)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.candidates[key] = candidateSnapshot{values: append([]account.RoutingCandidate(nil), values...), expiresAt: checkTime.Add(candidateCacheTTL)}
		s.mu.Unlock()
		return values, nil
	})
	if err != nil {
		return nil, err
	}
	return append([]account.RoutingCandidate(nil), loaded.([]account.RoutingCandidate)...), nil
}

func (s *Selector) invalidateCandidates(provider account.Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.candidates {
		if key.provider == provider {
			delete(s.candidates, key)
		}
	}
}

func (s *Selector) claimAccountSlot(ctx context.Context, value account.Credential) (*accountLease, error) {
	limit := value.MaxConcurrent
	if limit <= 0 {
		limit = account.DefaultMaxConcurrent
	}
	release, acquired, err := s.concurrency.Acquire(ctx, accountConcurrencyKey(value.ID), limit)
	if err != nil {
		return nil, fmt.Errorf("获取账号并发租约: %w", err)
	}
	if !acquired {
		return nil, nil
	}
	s.mu.Lock()
	s.lastSelectedAt[value.ID] = time.Now().UTC()
	s.mu.Unlock()
	return &accountLease{Credential: value, release: func() {
		release()
		s.announceLeaseReturn()
	}}, nil
}

func (s *Selector) acquirePinnedCapacity(ctx context.Context, value account.Credential) (*accountLease, error) {
	_, _, _, capacityWait := s.routingConfig()
	deadline := time.Now().Add(capacityWait)
	for {
		lease, err := s.claimAccountSlot(ctx, value)
		if err != nil || lease != nil {
			return lease, err
		}
		if capacityWait <= 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, deadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

func (s *Selector) leaseReturnNotice() <-chan struct{} {
	s.leaseWakeMu.Lock()
	defer s.leaseWakeMu.Unlock()
	if s.leaseWake == nil {
		s.leaseWake = make(chan struct{})
	}
	return s.leaseWake
}

func (s *Selector) announceLeaseReturn() {
	s.leaseWakeMu.Lock()
	if s.leaseWake != nil {
		close(s.leaseWake)
	}
	s.leaseWake = make(chan struct{})
	s.leaseWakeMu.Unlock()
}

// awaitLeaseRetry 在本实例归还租约时立即重试；短轮询用于感知其他实例释放的共享并发名额。
func (s *Selector) awaitLeaseRetry(ctx context.Context, deadline time.Time) (bool, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false, nil
	}
	notice := s.leaseReturnNotice()
	timer := time.NewTimer(min(remaining, 100*time.Millisecond))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-notice:
		return true, nil
	case <-timer.C:
		return time.Now().Before(deadline), nil
	}
}

func earlierFuture(current, candidate, now time.Time) time.Time {
	if candidate.IsZero() || !now.Before(candidate) {
		return current
	}
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func retryDelay(now, retryAt time.Time) time.Duration {
	if retryAt.IsZero() || !now.Before(retryAt) {
		return 0
	}
	return retryAt.Sub(now)
}

func (s *Selector) resolveTierOrder(provider account.Provider, upstreamModel string) []account.WebTier {
	if s.tierOrders == nil {
		return nil
	}
	return s.tierOrders.TierOrder(provider, upstreamModel)
}

func tierOrderRank(order []account.WebTier, tier account.WebTier) int {
	for index, value := range order {
		if value == tier {
			return index
		}
	}
	return len(order)
}
