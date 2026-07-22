package relational

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AccountRepository struct {
	db       *Database
	observer repository.InvalidationObserver
}

func NewAccountRepository(db *Database) *AccountRepository { return &AccountRepository{db: db} }

func (r *AccountRepository) SetInvalidationObserver(observer repository.InvalidationObserver) {
	r.observer = observer
}

func (r *AccountRepository) notifyInvalidation(ctx context.Context, event repository.InvalidationEvent) {
	if r.observer != nil {
		r.observer(ctx, event)
	}
}

type quotaBreakdownJSON struct {
	ProductCode  int     `json:"productCode"`
	UsagePercent float64 `json:"usagePercent"`
}

const (
	accountPaidPlanSignal       = `(LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_code), ' ', ''), '_', ''), '-', ''), '+', 'plus')) IN ('super', 'supergrok', 'supergrokpro', 'supergrokheavy', 'supergroklite', 'grokpro', 'xpremium', 'xpremiumplus', 'apikey') OR LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_name), ' ', ''), '_', ''), '-', ''), '+', 'plus')) IN ('super', 'supergrok', 'supergrokpro', 'supergrokheavy', 'supergroklite', 'grokpro', 'xpremium', 'xpremiumplus', 'apikey'))`
	accountFreePlanSignal       = `(LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_code), ' ', ''), '_', ''), '-', ''), '+', 'plus')) IN ('free', 'grokfree', 'freetier', 'basic', 'grokbasic', 'xbasic') OR LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_name), ' ', ''), '_', ''), '-', ''), '+', 'plus')) IN ('free', 'grokfree', 'freetier', 'basic', 'grokbasic', 'xbasic'))`
	accountPaidBillingSignals   = `(` + accountPaidPlanSignal + ` OR billing.monthly_limit > 0 OR billing.on_demand_cap > 0 OR billing.on_demand_used > 0 OR billing.prepaid_balance > 0)`
	accountPaidBillingPredicate = `EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = provider_accounts.id AND ` + accountPaidBillingSignals + `)`
	// 仅 grok_build 的管理员确认 Super entitlement；与 domain.IsBuildSuper 对齐。
	accountBuildSuperEntitledPredicate = `(provider_accounts.provider = 'grok_build' AND provider_accounts.build_super_entitled = TRUE)`
	accountBuildSuperPredicate         = `(` + accountPaidBillingPredicate + ` OR ` + accountBuildSuperEntitledPredicate + `)`
	accountInferredFreeBillingSignal   = `(TRIM(billing.plan_code) = '' AND TRIM(billing.plan_name) = '' AND billing.synced_at IS NOT NULL AND billing.monthly_limit = 0 AND billing.used = 0 AND billing.on_demand_cap = 0 AND billing.on_demand_used = 0 AND billing.prepaid_balance = 0 AND billing.credit_usage_percent = 0)`
	accountFreeBillingSignal           = `(` + accountFreePlanSignal + ` OR ` + accountInferredFreeBillingSignal + `)`
	accountFreeSignalPredicate         = `(provider_accounts.provider = 'grok_build' AND (LOWER(TRIM(provider_accounts.observed_model)) LIKE '%-build-free' OR EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = provider_accounts.id AND ` + accountFreeBillingSignal + `)))`
	accountRecoveryPredicate           = `EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status IN ('exhausted', 'probing'))`
	providerQuotaExhaustedPredicate    = `((provider_accounts.provider = 'grok_web' AND ((EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'weekly') AND NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'weekly' AND quota.remaining > 0)) OR (NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'weekly') AND EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id) AND NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.remaining > 0)))) OR (provider_accounts.provider = 'grok_console' AND EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id) AND NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.remaining > 0)))`
	accountTypeSortExpression          = `CASE WHEN provider_accounts.provider = 'grok_web' THEN COALESCE((SELECT profile.tier FROM web_account_profiles profile WHERE profile.account_id = provider_accounts.id), 'auto') WHEN ` + accountBuildSuperPredicate + ` THEN 'paid' WHEN ` + accountFreeSignalPredicate + ` THEN 'free' ELSE 'unknown' END`
	accountStatusSortExpression        = `CASE WHEN provider_accounts.enabled = FALSE THEN 4 WHEN provider_accounts.auth_status = 'reauthRequired' THEN 5 WHEN EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'probing') THEN 3 WHEN EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'exhausted') OR ` + providerQuotaExhaustedPredicate + ` THEN 2 WHEN provider_accounts.cooldown_until > CURRENT_TIMESTAMP THEN 1 ELSE 0 END`
	missingConsoleAccountPredicate     = `NOT EXISTS (SELECT 1 FROM provider_accounts AS console_account WHERE console_account.provider = ? AND console_account.source_key = ('console-' || provider_accounts.source_key))`
)

func (r *AccountRepository) List(ctx context.Context, input repository.AccountListQuery) ([]account.Credential, int64, error) {
	var total int64
	query := r.db.db.WithContext(ctx).Model(&accountModel{})
	if input.Filter.Provider != "" {
		query = query.Where("provider = ?", input.Filter.Provider)
	}
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(name) LIKE ? OR LOWER(email) LIKE ? OR LOWER(user_id) LIKE ? OR LOWER(team_id) LIKE ?", pattern, pattern, pattern, pattern)
	}
	switch input.Filter.QuotaType {
	case "free":
		// Super（Billing paid 或 BuildSuperEntitled）不得落入 free；与 IsKnownFreeBuild / QuotaView 一致。
		query = query.Where("NOT " + accountBuildSuperPredicate + " AND (EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.kind = 'free') OR " + accountFreeSignalPredicate + ")")
	case "paid":
		query = query.Where(accountBuildSuperPredicate)
	case "unknown":
		query = query.Where("NOT " + accountRecoveryPredicate + " AND NOT " + accountBuildSuperPredicate + " AND NOT " + accountFreeSignalPredicate)
	case "auto", "basic", "super", "heavy":
		query = query.Where("EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = provider_accounts.id AND profile.tier = ?)", input.Filter.QuotaType)
	}
	query = applyAccountStatusFilter(query, input.Filter.Status, input.Filter.Now)
	switch input.Filter.Egress {
	case "bound":
		query = query.Where("egress_node_id IS NOT NULL")
	case "unbound":
		query = query.Where("egress_node_id IS NULL")
	}
	if input.Filter.Refreshable != nil {
		if *input.Filter.Refreshable {
			query = query.Where("EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.encrypted_refresh <> '')")
		} else {
			query = query.Where("NOT EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.encrypted_refresh <> '')")
		}
	}
	if input.Filter.RestrictIDs {
		if len(input.Filter.AccountIDs) == 0 {
			query = query.Where("1 = 0")
		} else {
			query = query.Where("provider_accounts.id IN ?", input.Filter.AccountIDs)
		}
	}
	if len(input.Filter.ExcludeIDs) > 0 {
		query = query.Where("provider_accounts.id NOT IN ?", input.Filter.ExcludeIDs)
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []accountModel
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"name":      {expression: "LOWER(provider_accounts.name)"},
		"type":      {expression: accountTypeSortExpression},
		"status":    {expression: accountStatusSortExpression},
		"createdAt": {expression: "provider_accounts.created_at", defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "provider_accounts.created_at", defaultDirection: repository.SortDescending}, "provider_accounts.id")
	if err := query.Preload("Credential").Preload("WebProfile").Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAccountDomain(row))
	}
	if err := r.attachAccountLinks(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (r *AccountRepository) ListProviderAccountBatch(ctx context.Context, providerValue account.Provider, afterID uint64, limit int) ([]account.Credential, int64, error) {
	if limit < 1 {
		return []account.Credential{}, 0, nil
	}
	var total int64
	if afterID == 0 {
		if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("provider = ?", providerValue).Count(&total).Error; err != nil {
			return nil, 0, err
		}
	}
	var rows []accountModel
	if err := r.db.db.WithContext(ctx).
		Preload("Credential").Preload("WebProfile").
		Where("provider = ? AND id > ?", providerValue, afterID).
		Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAccountDomain(row))
	}
	if err := r.attachAccountLinks(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// CountProviderAccountsByIDs 只校验账号主表归属，不加载额度、关联或审计数据。
func (r *AccountRepository) CountProviderAccountsByIDs(ctx context.Context, providerValue account.Provider, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var count int64
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("provider = ? AND id IN ?", providerValue, ids).
		Count(&count).Error
	return count, err
}

func (r *AccountRepository) Summarize(ctx context.Context, now time.Time) ([]repository.AccountSummary, error) {
	var rows []repository.AccountSummary
	selectFields := `
		provider,
		COUNT(*) AS total,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND NOT ` + accountRecoveryPredicate + ` AND NOT ` + providerQuotaExhaustedPredicate + ` AND (cooldown_until IS NULL OR cooldown_until <= ?) THEN 1 ELSE 0 END) AS available,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND NOT ` + accountRecoveryPredicate + ` AND NOT ` + providerQuotaExhaustedPredicate + ` AND cooldown_until > ? THEN 1 ELSE 0 END) AS cooldown,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND (EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'exhausted') OR ` + providerQuotaExhaustedPredicate + `) THEN 1 ELSE 0 END) AS waiting_reset,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'probing') THEN 1 ELSE 0 END) AS probing,
		SUM(CASE WHEN enabled = ? THEN 1 ELSE 0 END) AS disabled,
		SUM(CASE WHEN enabled = ? AND auth_status = ? THEN 1 ELSE 0 END) AS reauth_required`
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).Select(
		selectFields,
		true, account.AuthStatusActive, now,
		true, account.AuthStatusActive, now,
		true, account.AuthStatusActive,
		true, account.AuthStatusActive,
		false,
		true, account.AuthStatusReauthRequired,
	).Group("provider").Scan(&rows).Error
	return rows, err
}

// ListRoutingCandidates 批量加载账号、额度、恢复状态和目标模型能力，避免推理热路径按账号逐条查询。
func (r *AccountRepository) ListRoutingCandidates(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string) ([]account.RoutingCandidate, error) {
	values, err := r.ListEnabled(ctx, provider)
	if err != nil {
		return nil, err
	}
	bound := make(map[uint64]bool)
	if strings.TrimSpace(upstreamModel) != "" {
		var boundIDs []uint64
		if err := r.db.db.WithContext(ctx).
			Table("model_route_accounts AS binding").
			Select("binding.account_id").
			Joins("JOIN model_routes AS route ON route.id = binding.model_route_id").
			Where("route.provider = ? AND route.upstream_model = ?", provider, upstreamModel).
			Scan(&boundIDs).Error; err != nil {
			return nil, err
		}
		if len(boundIDs) > 0 {
			for _, id := range boundIDs {
				bound[id] = true
			}
			filtered := values[:0]
			for _, value := range values {
				if bound[value.ID] {
					filtered = append(filtered, value)
				}
			}
			values = filtered
		}
	}
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	billings, err := r.GetBillings(ctx, ids)
	if err != nil {
		return nil, err
	}
	recoveries, err := r.GetQuotaRecoveries(ctx, ids)
	if err != nil {
		return nil, err
	}
	quotaWindows := make(map[uint64]account.QuotaWindow, len(ids))
	if len(ids) > 0 && (provider == account.ProviderWeb || quotaMode != "") {
		var rows []quotaWindowModel
		modes := make([]string, 0, 2)
		if provider == account.ProviderWeb {
			modes = append(modes, "weekly")
		}
		if quotaMode != "" {
			modes = append(modes, quotaMode)
		}
		if err := r.db.db.WithContext(ctx).Where("account_id IN ? AND mode IN ?", ids, modes).Order("CASE WHEN mode = 'weekly' THEN 0 ELSE 1 END").Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			if _, exists := quotaWindows[row.AccountID]; !exists {
				quotaWindows[row.AccountID] = toQuotaWindowDomain(row)
			}
		}
	}
	known := make(map[uint64]bool, len(ids))
	supported := make(map[uint64]bool, len(ids))
	modelQuotaBlocks := make(map[uint64]account.ModelQuotaBlock, len(ids))
	if strings.TrimSpace(upstreamModel) != "" && len(ids) > 0 {
		var states []accountModelSyncStateModel
		if err := r.db.db.WithContext(ctx).Where("account_id IN ? AND last_success_at IS NOT NULL", ids).Find(&states).Error; err != nil {
			return nil, err
		}
		for _, state := range states {
			known[state.AccountID] = true
		}
		var capabilities []accountModelCapabilityModel
		if err := r.db.db.WithContext(ctx).Where("account_id IN ? AND upstream_model = ?", ids, upstreamModel).Find(&capabilities).Error; err != nil {
			return nil, err
		}
		for _, capability := range capabilities {
			supported[capability.AccountID] = true
		}
		var blockRows []accountModelQuotaBlockModel
		if err := r.db.db.WithContext(ctx).Where("account_id IN ? AND upstream_model = ? AND cooldown_until > ?", ids, upstreamModel, time.Now().UTC()).Find(&blockRows).Error; err != nil {
			return nil, err
		}
		for _, row := range blockRows {
			modelQuotaBlocks[row.AccountID] = account.ModelQuotaBlock{AccountID: row.AccountID, UpstreamModel: row.UpstreamModel, Reason: row.Reason, CooldownUntil: row.CooldownUntil.UTC(), UpdatedAt: row.UpdatedAt.UTC()}
		}
	}
	sharedSuperBuildModel := false
	if provider == account.ProviderBuild && len(bound) == 0 {
		for _, value := range values {
			if !supported[value.ID] {
				continue
			}
			var billing *account.Billing
			if snapshot, exists := billings[value.ID]; exists {
				billing = &snapshot
			}
			if account.IsBuildSuper(value, billing) {
				sharedSuperBuildModel = true
				break
			}
		}
	}
	result := make([]account.RoutingCandidate, 0, len(values))
	for _, value := range values {
		capabilityKnown, supportsModel := known[value.ID], supported[value.ID]
		if len(bound) > 0 {
			capabilityKnown, supportsModel = true, true
		} else if sharedSuperBuildModel {
			var billing *account.Billing
			if snapshot, exists := billings[value.ID]; exists {
				billing = &snapshot
			}
			if account.IsBuildSuper(value, billing) {
				capabilityKnown, supportsModel = true, true
			}
		}
		candidate := account.RoutingCandidate{Credential: value, ModelCapabilityKnown: capabilityKnown, SupportsModel: supportsModel}
		if billing, ok := billings[value.ID]; ok {
			candidate.Billing = &billing
		}
		if recovery, ok := recoveries[value.ID]; ok {
			candidate.QuotaRecovery = &recovery
		}
		if window, ok := quotaWindows[value.ID]; ok {
			candidate.QuotaWindow = &window
		}
		if block, ok := modelQuotaBlocks[value.ID]; ok {
			candidate.ModelQuotaBlock = &block
		}
		result = append(result, candidate)
	}
	return result, nil
}

func (r *AccountRepository) ListRoutingAccountBases(ctx context.Context, provider account.Provider, quotaMode string) ([]account.RoutingAccountBase, error) {
	values, err := r.ListEnabled(ctx, provider)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	billings, err := r.GetBillings(ctx, ids)
	if err != nil {
		return nil, err
	}
	recoveries, err := r.GetQuotaRecoveries(ctx, ids)
	if err != nil {
		return nil, err
	}
	quotaWindows := make(map[uint64]account.QuotaWindow, len(ids))
	if len(ids) > 0 && (provider == account.ProviderWeb || quotaMode != "") {
		modes := make([]string, 0, 2)
		if provider == account.ProviderWeb {
			modes = append(modes, "weekly")
		}
		if quotaMode != "" {
			modes = append(modes, quotaMode)
		}
		var rows []quotaWindowModel
		if err := r.db.db.WithContext(ctx).Where("account_id IN ? AND mode IN ?", ids, modes).Order("CASE WHEN mode = 'weekly' THEN 0 ELSE 1 END").Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			if _, exists := quotaWindows[row.AccountID]; !exists {
				quotaWindows[row.AccountID] = toQuotaWindowDomain(row)
			}
		}
	}
	result := make([]account.RoutingAccountBase, 0, len(values))
	for _, value := range values {
		base := account.RoutingAccountBase{Credential: value}
		if billing, ok := billings[value.ID]; ok {
			base.Billing = &billing
		}
		if recovery, ok := recoveries[value.ID]; ok {
			base.QuotaRecovery = &recovery
		}
		if window, ok := quotaWindows[value.ID]; ok {
			base.QuotaWindow = &window
		}
		result = append(result, base)
	}
	return result, nil
}

func (r *AccountRepository) ListRoutingAccountOverlays(ctx context.Context, provider account.Provider, upstreamModel string) (account.RoutingOverlaySnapshot, error) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return account.RoutingOverlaySnapshot{}, nil
	}
	var boundIDs []uint64
	if err := r.db.db.WithContext(ctx).
		Table("model_route_accounts AS binding").
		Select("binding.account_id").
		Joins("JOIN model_routes AS route ON route.id = binding.model_route_id").
		Where("route.provider = ? AND route.upstream_model = ?", provider, upstreamModel).
		Scan(&boundIDs).Error; err != nil {
		return account.RoutingOverlaySnapshot{}, err
	}
	values := make(map[uint64]account.RoutingAccountOverlay)
	for _, id := range boundIDs {
		values[id] = account.RoutingAccountOverlay{AccountID: id, Bound: true, ModelCapabilityKnown: true, SupportsModel: true}
	}
	var states []accountModelSyncStateModel
	if err := r.db.db.WithContext(ctx).
		Table("account_model_sync_states AS state").
		Select("state.*").
		Joins("JOIN provider_accounts AS account ON account.id = state.account_id").
		Where("account.provider = ? AND account.enabled = TRUE AND state.last_success_at IS NOT NULL", provider).
		Find(&states).Error; err != nil {
		return account.RoutingOverlaySnapshot{}, err
	}
	for _, state := range states {
		overlay := values[state.AccountID]
		overlay.AccountID = state.AccountID
		overlay.ModelCapabilityKnown = true
		values[state.AccountID] = overlay
	}
	var capabilities []accountModelCapabilityModel
	if err := r.db.db.WithContext(ctx).
		Table("account_model_capabilities AS capability").
		Select("capability.*").
		Joins("JOIN provider_accounts AS account ON account.id = capability.account_id").
		Where("account.provider = ? AND account.enabled = TRUE AND capability.upstream_model = ?", provider, upstreamModel).
		Find(&capabilities).Error; err != nil {
		return account.RoutingOverlaySnapshot{}, err
	}
	for _, capability := range capabilities {
		overlay := values[capability.AccountID]
		overlay.AccountID = capability.AccountID
		overlay.SupportsModel = true
		values[capability.AccountID] = overlay
	}
	var blockRows []accountModelQuotaBlockModel
	if err := r.db.db.WithContext(ctx).
		Table("account_model_quota_blocks AS block").
		Select("block.*").
		Joins("JOIN provider_accounts AS account ON account.id = block.account_id").
		Where("account.provider = ? AND account.enabled = TRUE AND block.upstream_model = ? AND block.cooldown_until > ?", provider, upstreamModel, time.Now().UTC()).
		Find(&blockRows).Error; err != nil {
		return account.RoutingOverlaySnapshot{}, err
	}
	for _, row := range blockRows {
		overlay := values[row.AccountID]
		overlay.AccountID = row.AccountID
		overlay.ModelQuotaBlock = &account.ModelQuotaBlock{AccountID: row.AccountID, UpstreamModel: row.UpstreamModel, Reason: row.Reason, CooldownUntil: row.CooldownUntil.UTC(), UpdatedAt: row.UpdatedAt.UTC()}
		values[row.AccountID] = overlay
	}
	result := account.RoutingOverlaySnapshot{HasBindings: len(boundIDs) > 0, Values: make([]account.RoutingAccountOverlay, 0, len(values))}
	for _, value := range values {
		result.Values = append(result.Values, value)
	}
	return result, nil
}

func (r *AccountRepository) ListEnabled(ctx context.Context, provider account.Provider) ([]account.Credential, error) {
	var rows []accountModel
	err := r.db.db.WithContext(ctx).Preload("Credential").Preload("WebProfile").Where("provider = ? AND enabled = ? AND auth_status = ?", provider, true, account.AuthStatusActive).Order("priority DESC, id ASC").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAccountDomain(row))
	}
	if err := r.attachRoutingEgressIdentities(ctx, provider, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *AccountRepository) ListEnabledAccountIDs(ctx context.Context, provider account.Provider, refreshableOnly bool) ([]uint64, error) {
	query := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive)
	if refreshableOnly {
		query = query.
			Joins("JOIN account_credentials AS credential ON credential.account_id = account.id").
			Where("credential.encrypted_refresh <> ''")
	}
	var ids []uint64
	err := query.Order("account.priority DESC, account.id ASC").Scan(&ids).Error
	return ids, err
}

func (r *AccountRepository) FilterMissingBuildConversionIDs(ctx context.Context, ids []uint64) ([]uint64, error) {
	if len(ids) == 0 {
		return []uint64{}, nil
	}
	var linkedIDs []uint64
	if err := r.db.db.WithContext(ctx).Model(&accountProviderLinkModel{}).
		Where("web_account_id IN ?", ids).Pluck("web_account_id", &linkedIDs).Error; err != nil {
		return nil, err
	}
	linked := make(map[uint64]struct{}, len(linkedIDs))
	for _, id := range linkedIDs {
		linked[id] = struct{}{}
	}
	values := make([]uint64, 0, len(ids)-len(linked))
	for _, id := range ids {
		if _, exists := linked[id]; !exists {
			values = append(values, id)
		}
	}
	return values, nil
}

func (r *AccountRepository) ListUnlinkedWebAccountIDs(ctx context.Context, afterID uint64, limit int) ([]uint64, int64, error) {
	if limit < 1 {
		return []uint64{}, 0, nil
	}
	query := func() *gorm.DB {
		return r.db.db.WithContext(ctx).
			Table("provider_accounts AS account").
			Joins("LEFT JOIN account_provider_links AS link ON link.web_account_id = account.id").
			Where("account.provider = ? AND link.web_account_id IS NULL", account.ProviderWeb)
	}
	var total int64
	if afterID == 0 {
		if err := query().Count(&total).Error; err != nil {
			return nil, 0, err
		}
	}
	var ids []uint64
	err := query().
		Select("account.id").
		Where("account.id > ?", afterID).
		Order("account.id ASC").
		Limit(limit).
		Scan(&ids).Error
	return ids, total, err
}

func (r *AccountRepository) ListMissingConsoleSyncAccounts(ctx context.Context, ids []uint64) ([]account.Credential, error) {
	if len(ids) == 0 {
		return []account.Credential{}, nil
	}
	var existing int64
	if err := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("id IN ? AND provider = ?", ids, account.ProviderWeb).Count(&existing).Error; err != nil {
		return nil, err
	}
	if existing != int64(len(ids)) {
		return nil, repository.ErrNotFound
	}
	var rows []accountModel
	if err := r.db.db.WithContext(ctx).
		Preload("Credential").Preload("WebProfile").
		Where("id IN ? AND provider = ?", ids, account.ProviderWeb).
		Where(missingConsoleAccountPredicate, account.ProviderConsole).
		Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		values = append(values, toAccountDomain(row))
	}
	return values, nil
}

func (r *AccountRepository) ListMissingConsoleSyncBatch(ctx context.Context, afterID uint64, limit int) ([]account.Credential, int64, int64, error) {
	if limit < 1 {
		return []account.Credential{}, 0, 0, nil
	}
	query := func() *gorm.DB {
		return r.db.db.WithContext(ctx).Model(&accountModel{}).
			Where("provider = ?", account.ProviderWeb).
			Where(missingConsoleAccountPredicate, account.ProviderConsole)
	}
	var total, skipped int64
	if afterID == 0 {
		if err := query().Count(&total).Error; err != nil {
			return nil, 0, 0, err
		}
		var all int64
		if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("provider = ?", account.ProviderWeb).Count(&all).Error; err != nil {
			return nil, 0, 0, err
		}
		skipped = max(0, all-total)
	}
	var rows []accountModel
	if err := query().Preload("Credential").Preload("WebProfile").
		Where("id > ?", afterID).Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, 0, err
	}
	values := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		values = append(values, toAccountDomain(row))
	}
	return values, total, skipped, nil
}

func (r *AccountRepository) HasActive(ctx context.Context, provider account.Provider) (bool, error) {
	var row struct{ ID uint64 }
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).Select("id").Where("provider = ? AND enabled = ? AND auth_status = ?", provider, true, account.AuthStatusActive).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return row.ID > 0, err
}

func (r *AccountRepository) Get(ctx context.Context, id uint64) (account.Credential, error) {
	var row accountModel
	if err := r.db.db.WithContext(ctx).Preload("Credential").Preload("WebProfile").First(&row, id).Error; err != nil {
		return account.Credential{}, mapError(err)
	}
	value := toAccountDomain(row)
	values := []account.Credential{value}
	if err := r.attachAccountLinks(ctx, values); err != nil {
		return account.Credential{}, err
	}
	return values[0], nil
}

func (r *AccountRepository) LinkWebToBuild(ctx context.Context, webAccountID, buildAccountID uint64) error {
	if webAccountID == 0 || buildAccountID == 0 || webAccountID == buildAccountID {
		return repository.ErrConflict
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var webAccount, buildAccount accountModel
		if err := tx.Select("id", "provider").First(&webAccount, webAccountID).Error; err != nil {
			return err
		}
		if err := tx.Select("id", "provider").First(&buildAccount, buildAccountID).Error; err != nil {
			return err
		}
		if webAccount.Provider != string(account.ProviderWeb) || buildAccount.Provider != string(account.ProviderBuild) {
			return repository.ErrConflict
		}
		var existing accountProviderLinkModel
		err := tx.Where("web_account_id = ? OR build_account_id = ?", webAccountID, buildAccountID).First(&existing).Error
		if err == nil {
			if existing.WebAccountID == webAccountID && existing.BuildAccountID == buildAccountID {
				return nil
			}
			return repository.ErrConflict
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(&accountProviderLinkModel{WebAccountID: webAccountID, BuildAccountID: buildAccountID, CreatedAt: time.Now().UTC()}).Error
	})
	err = mapError(err)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged})
	}
	return err
}

func (r *AccountRepository) attachAccountLinks(ctx context.Context, values []account.Credential) error {
	if len(values) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(values))
	positions := make(map[uint64]int, len(values))
	for index := range values {
		ids = append(ids, values[index].ID)
		positions[values[index].ID] = index
	}
	var buildRows []struct {
		WebAccountID            uint64
		BuildAccountID          uint64
		WebName                 string
		BuildName               string
		WebEmail                string
		BuildEmail              string
		WebUserID               string
		BuildUserID             string
		WebSourceKey            string
		EgressIdentity          string
		WebNSFWEnabledAt        *time.Time
		WebTermsAcceptedAt      *time.Time
		WebTermsAcceptedVersion int
	}
	err := r.db.db.WithContext(ctx).Table("account_provider_links AS link").
		Select("link.web_account_id, link.build_account_id, web.name AS web_name, build.name AS build_name, web.email AS web_email, build.email AS build_email, web.user_id AS web_user_id, build.user_id AS build_user_id, web.source_key AS web_source_key, profile.egress_identity, profile.nsfw_enabled_at AS web_nsfw_enabled_at, profile.terms_accepted_at AS web_terms_accepted_at, profile.terms_accepted_version AS web_terms_accepted_version").
		Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
		Joins("JOIN provider_accounts AS build ON build.id = link.build_account_id").
		Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
		Where("link.web_account_id IN ? OR link.build_account_id IN ?", ids, ids).
		Scan(&buildRows).Error
	if err != nil {
		return err
	}
	for _, row := range buildRows {
		egressIdentity := linkedWebEgressIdentity(row.EgressIdentity, row.WebSourceKey)
		if index, ok := positions[row.WebAccountID]; ok {
			values[index].LinkedAccountID = row.BuildAccountID
			values[index].LinkedAccountName = row.BuildName
			values[index].LinkedProvider = account.ProviderBuild
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.BuildAccountID, Provider: account.ProviderBuild, Name: row.BuildName, Email: row.BuildEmail, UserID: row.BuildUserID})
			if values[index].EgressIdentity == "" {
				values[index].EgressIdentity = egressIdentity
			}
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
		if index, ok := positions[row.BuildAccountID]; ok {
			values[index].LinkedAccountID = row.WebAccountID
			values[index].LinkedAccountName = row.WebName
			values[index].LinkedProvider = account.ProviderWeb
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.WebAccountID, Provider: account.ProviderWeb, Name: row.WebName, Email: row.WebEmail, UserID: row.WebUserID})
			values[index].EgressIdentity = egressIdentity
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
	}
	var consoleRows []struct {
		WebAccountID            uint64
		ConsoleAccountID        uint64
		WebName                 string
		ConsoleName             string
		WebEmail                string
		ConsoleEmail            string
		WebUserID               string
		ConsoleUserID           string
		WebSourceKey            string
		EgressIdentity          string
		WebNSFWEnabledAt        *time.Time
		WebTermsAcceptedAt      *time.Time
		WebTermsAcceptedVersion int
	}
	if err := r.db.db.WithContext(ctx).Table("web_console_account_links AS link").
		Select("link.web_account_id, link.console_account_id, web.name AS web_name, console.name AS console_name, web.email AS web_email, console.email AS console_email, web.user_id AS web_user_id, console.user_id AS console_user_id, web.source_key AS web_source_key, profile.egress_identity, profile.nsfw_enabled_at AS web_nsfw_enabled_at, profile.terms_accepted_at AS web_terms_accepted_at, profile.terms_accepted_version AS web_terms_accepted_version").
		Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
		Joins("JOIN provider_accounts AS console ON console.id = link.console_account_id").
		Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
		Where("link.web_account_id IN ? OR link.console_account_id IN ?", ids, ids).
		Scan(&consoleRows).Error; err != nil {
		return err
	}
	for _, row := range consoleRows {
		egressIdentity := linkedWebEgressIdentity(row.EgressIdentity, row.WebSourceKey)
		if index, ok := positions[row.WebAccountID]; ok {
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.ConsoleAccountID, Provider: account.ProviderConsole, Name: row.ConsoleName, Email: row.ConsoleEmail, UserID: row.ConsoleUserID})
			if values[index].EgressIdentity == "" {
				values[index].EgressIdentity = egressIdentity
			}
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
		if index, ok := positions[row.ConsoleAccountID]; ok {
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.WebAccountID, Provider: account.ProviderWeb, Name: row.WebName, Email: row.WebEmail, UserID: row.WebUserID})
			values[index].EgressIdentity = egressIdentity
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
	}
	return nil
}

func currentWebTermsAcceptedAt(value *time.Time, version int) *time.Time {
	if version < account.CurrentWebTermsVersion {
		return nil
	}
	return value
}

// attachRoutingEgressIdentities 只补充推理路由需要的稳定出口身份。
// 管理端展示所需的账号名称和 linkedAccounts 仍由 attachAccountLinks 加载，
// 避免路由候选缓存刷新时额外查询两类完整关系。
func (r *AccountRepository) attachRoutingEgressIdentities(ctx context.Context, provider account.Provider, values []account.Credential) error {
	if len(values) == 0 || provider == account.ProviderWeb {
		return nil
	}
	ids := make([]uint64, 0, len(values))
	positions := make(map[uint64]int, len(values))
	for index := range values {
		ids = append(ids, values[index].ID)
		positions[values[index].ID] = index
	}
	type identityRow struct {
		AccountID      uint64
		WebSourceKey   string
		EgressIdentity string
	}
	var rows []identityRow
	query := r.db.db.WithContext(ctx)
	switch provider {
	case account.ProviderBuild:
		query = query.Table("account_provider_links AS link").
			Select("link.build_account_id AS account_id, web.source_key AS web_source_key, profile.egress_identity").
			Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
			Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
			Where("link.build_account_id IN ?", ids)
	case account.ProviderConsole:
		query = query.Table("web_console_account_links AS link").
			Select("link.console_account_id AS account_id, web.source_key AS web_source_key, profile.egress_identity").
			Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
			Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
			Where("link.console_account_id IN ?", ids)
	default:
		return nil
	}
	if err := query.Scan(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if index, ok := positions[row.AccountID]; ok {
			values[index].EgressIdentity = linkedWebEgressIdentity(row.EgressIdentity, row.WebSourceKey)
		}
	}
	return nil
}

func linkedWebEgressIdentity(stored, sourceKey string) string {
	if value := strings.TrimSpace(stored); value != "" {
		return value
	}
	value, _ := egressIdentityFromWebSourceKey(sourceKey)
	return value
}

func (r *AccountRepository) UpsertByIdentity(ctx context.Context, value account.Credential) (account.Credential, bool, error) {
	var result repository.AccountUpsertResult
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = upsertAccountByIdentity(tx, value)
		return err
	})
	if err != nil {
		return account.Credential{}, false, mapError(err)
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: value.Provider, AccountID: result.ID})
	stored, err := r.Get(ctx, result.ID)
	return stored, result.Created, err
}

func (r *AccountRepository) UpsertManyByIdentity(ctx context.Context, values []account.Credential) ([]repository.AccountUpsertResult, error) {
	if len(values) == 0 {
		return []repository.AccountUpsertResult{}, nil
	}
	results := make([]repository.AccountUpsertResult, len(values))
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		identityKeys := make([]string, 0, len(values))
		sourceKeysByProvider := make(map[account.Provider][]string)
		for _, value := range values {
			identityKeys = append(identityKeys, fromAccountDomain(value).IdentityKey)
			if strings.TrimSpace(value.SourceKey) != "" {
				sourceKeysByProvider[value.Provider] = append(sourceKeysByProvider[value.Provider], value.SourceKey)
			}
		}
		var existingRows []accountModel
		if err := tx.Where("identity_key IN ?", identityKeys).Find(&existingRows).Error; err != nil {
			return err
		}
		existingByIdentity := make(map[string]accountModel, len(values))
		for _, row := range existingRows {
			existingByIdentity[row.IdentityKey] = row
		}
		existingBySource := make(map[string]accountModel, len(values))
		for providerValue, sourceKeys := range sourceKeysByProvider {
			var sourceRows []accountModel
			if err := tx.Where("provider = ? AND source_key IN ?", providerValue, sourceKeys).Find(&sourceRows).Error; err != nil {
				return err
			}
			for _, row := range sourceRows {
				key := providerSourceLookupKey(row.Provider, row.SourceKey)
				if existing, duplicate := existingBySource[key]; duplicate && existing.ID != row.ID {
					return fmt.Errorf("Provider %s 的来源凭据匹配多个账号", row.Provider)
				}
				existingBySource[key] = row
			}
		}
		for index, value := range values {
			identityKey := fromAccountDomain(value).IdentityKey
			existing, foundByIdentity := existingByIdentity[identityKey]
			bySource, foundBySource := existingBySource[providerSourceLookupKey(string(value.Provider), value.SourceKey)]
			if foundByIdentity && foundBySource && existing.ID != bySource.ID {
				return fmt.Errorf("账号身份与来源凭据指向不同账号")
			}
			if !foundByIdentity && foundBySource {
				existing = bySource
			}
			var current *accountModel
			if foundByIdentity || foundBySource {
				current = &existing
			}
			result, stored, err := upsertKnownAccountByIdentity(tx, value, current)
			if err != nil {
				return err
			}
			results[index] = result
			existingByIdentity[stored.IdentityKey] = stored
			existingBySource[providerSourceLookupKey(stored.Provider, stored.SourceKey)] = stored
		}
		return nil
	})
	if err != nil {
		return nil, mapError(err)
	}
	providers := make(map[account.Provider]struct{})
	for _, value := range values {
		providers[value.Provider] = struct{}{}
	}
	for providerValue := range providers {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: providerValue})
	}
	return results, nil
}

func upsertAccountByIdentity(tx *gorm.DB, value account.Credential) (repository.AccountUpsertResult, error) {
	row := fromAccountDomain(value)
	var byIdentity accountModel
	identityErr := tx.Where("identity_key = ?", row.IdentityKey).First(&byIdentity).Error
	if identityErr != nil && !errors.Is(identityErr, gorm.ErrRecordNotFound) {
		return repository.AccountUpsertResult{}, identityErr
	}
	var sourceRows []accountModel
	if strings.TrimSpace(row.SourceKey) != "" {
		if err := tx.Where("provider = ? AND source_key = ?", row.Provider, row.SourceKey).Limit(2).Find(&sourceRows).Error; err != nil {
			return repository.AccountUpsertResult{}, err
		}
		if len(sourceRows) > 1 {
			return repository.AccountUpsertResult{}, fmt.Errorf("Provider %s 的来源凭据匹配多个账号", row.Provider)
		}
	}
	if identityErr == nil && len(sourceRows) == 1 && byIdentity.ID != sourceRows[0].ID {
		return repository.AccountUpsertResult{}, fmt.Errorf("账号身份与来源凭据指向不同账号")
	}
	if identityErr == nil {
		result, _, err := upsertKnownAccountByIdentity(tx, value, &byIdentity)
		return result, err
	}
	if len(sourceRows) == 1 {
		result, _, err := upsertKnownAccountByIdentity(tx, value, &sourceRows[0])
		return result, err
	}
	result, _, err := upsertKnownAccountByIdentity(tx, value, nil)
	return result, err
}

func providerSourceLookupKey(providerValue, sourceKey string) string {
	return providerValue + "\x00" + sourceKey
}

func upsertKnownAccountByIdentity(tx *gorm.DB, value account.Credential, existing *accountModel) (repository.AccountUpsertResult, accountModel, error) {
	row := fromAccountDomain(value)
	if existing != nil {
		if value.EncryptedCloudflareCookie == "" {
			var storedCredential accountCredentialModel
			if err := tx.Where("account_id = ?", existing.ID).First(&storedCredential).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return repository.AccountUpsertResult{}, accountModel{}, err
			}
			value.EncryptedCloudflareCookie = storedCredential.EncryptedCloudflareCookie
		}
		row.ID = existing.ID
		row.CreatedAt = existing.CreatedAt
		row.Enabled = existing.Enabled
		row.Priority = existing.Priority
		row.MaxConcurrent = existing.MaxConcurrent
		row.MinimumRemaining = existing.MinimumRemaining
		row.FailureCount = existing.FailureCount
		row.CooldownUntil = existing.CooldownUntil
		row.LastError = existing.LastError
		row.LastUsedAt = existing.LastUsedAt
		row.ObservedModel = existing.ObservedModel
		row.ObservedModelAt = existing.ObservedModelAt
		// 账号级 Build 路由、XAI 回退记录与 Super entitlement 在 upsert/转换/刷新路径中保留。
		row.BuildAPIFallback = existing.BuildAPIFallback
		row.BuildRouteMode = existing.BuildRouteMode
		row.BuildSuperEntitled = existing.BuildSuperEntitled
		row.EgressNodeID = existing.EgressNodeID
		row.EgressAssignmentMode = existing.EgressAssignmentMode
		row.EgressAssignedAt = existing.EgressAssignedAt
		// reauth_marked_at 与 Update 路径一致：保持 reauth 时永不被普通 upsert 改写。
		applyReauthMarkedAtTransition(&row, *existing)
		if err := tx.Save(&row).Error; err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		if err := saveAccountRelations(tx, value, row.ID); err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		return repository.AccountUpsertResult{ID: row.ID}, row, nil
	}
	if row.AuthStatus == "" {
		row.AuthStatus = string(account.AuthStatusActive)
	}
	if row.AuthStatus == string(account.AuthStatusReauthRequired) && row.ReauthMarkedAt == nil {
		now := time.Now().UTC()
		row.ReauthMarkedAt = &now
	}
	if row.AuthStatus != string(account.AuthStatusReauthRequired) {
		row.ReauthMarkedAt = nil
	}
	if row.Priority == 0 {
		row.Priority = account.DefaultPriority
	}
	if row.MaxConcurrent == 0 {
		row.MaxConcurrent = account.DefaultMaxConcurrent
	}
	row.Enabled = true
	if err := tx.Create(&row).Error; err != nil {
		return repository.AccountUpsertResult{}, accountModel{}, err
	}
	if err := saveAccountRelations(tx, value, row.ID); err != nil {
		return repository.AccountUpsertResult{}, accountModel{}, err
	}
	return repository.AccountUpsertResult{ID: row.ID, Created: true}, row, nil
}

func (r *AccountRepository) Update(ctx context.Context, value account.Credential) (account.Credential, error) {
	var row accountModel
	var storedProvider account.Provider
	if err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing accountModel
		if err := tx.Select("id", "identity_key", "created_at", "provider", "auth_status", "reauth_marked_at").First(&existing, value.ID).Error; err != nil {
			return err
		}
		storedProvider = account.Provider(existing.Provider)
		value.Provider = storedProvider
		row = fromAccountDomain(value)
		row.ID = existing.ID
		// 身份同步补充的 user_id/email 不得让普通编辑重写持久化身份键。
		row.IdentityKey = existing.IdentityKey
		row.CreatedAt = existing.CreatedAt
		applyReauthMarkedAtTransition(&row, existing)
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		return saveAccountRelations(tx, value, row.ID)
	}); err != nil {
		return account.Credential{}, mapError(err)
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: storedProvider, AccountID: row.ID})
	return r.Get(ctx, row.ID)
}

// applyReauthMarkedAtTransition 仅在状态切入 reauthRequired 时打锚点；保持 reauth 时保留原锚点；离开 reauth 时清空。
func applyReauthMarkedAtTransition(row *accountModel, existing accountModel) {
	if row.AuthStatus == string(account.AuthStatusReauthRequired) {
		if existing.AuthStatus == string(account.AuthStatusReauthRequired) && existing.ReauthMarkedAt != nil {
			row.ReauthMarkedAt = existing.ReauthMarkedAt
			return
		}
		if row.ReauthMarkedAt == nil {
			now := time.Now().UTC()
			row.ReauthMarkedAt = &now
		}
		return
	}
	row.ReauthMarkedAt = nil
}

func saveAccountRelations(tx *gorm.DB, value account.Credential, accountID uint64) error {
	value.ID = accountID
	credential := fromAccountCredentialDomain(value)
	if err := tx.Save(&credential).Error; err != nil {
		return err
	}
	if profile := fromWebProfileDomain(value); profile != nil {
		updates := []string{"tier", "synced_at"}
		if profile.NSFWEnabledAt != nil {
			updates = append(updates, "nsfw_enabled_at")
		}
		if profile.TermsAcceptedAt != nil {
			updates = append(updates, "terms_accepted_at")
		}
		if profile.TermsAcceptedVersion > 0 {
			updates = append(updates, "terms_accepted_version")
		}
		if profile.BirthDateSetAt != nil {
			updates = append(updates, "birth_date_set_at")
		}
		if strings.TrimSpace(profile.EgressIdentity) != "" {
			updates = append(updates, "egress_identity")
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}},
			DoUpdates: clause.AssignmentColumns(updates),
		}).Create(profile).Error
	}
	return tx.Where("account_id = ?", accountID).Delete(&webAccountProfileModel{}).Error
}

// MarkWebNSFWEnabled 幂等保存首次成功开启时间；重复执行不会覆盖已有标记。
func (r *AccountRepository) MarkWebNSFWEnabled(ctx context.Context, id uint64, enabledAt time.Time) error {
	if id == 0 || enabledAt.IsZero() {
		return fmt.Errorf("Web NSFW 标记参数无效")
	}
	err := r.markWebProfileTimestamp(ctx, id, "nsfw_enabled_at", enabledAt)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderWeb, AccountID: id})
	}
	return err
}

// MarkWebTermsAccepted 幂等保存已完整接受的产品协议版本。
// 协议升级时会同步更新完成时间；相同或更高版本不会被覆盖。
func (r *AccountRepository) MarkWebTermsAccepted(ctx context.Context, id uint64, version int, acceptedAt time.Time) error {
	if id == 0 || version <= 0 || acceptedAt.IsZero() {
		return fmt.Errorf("Web 服务协议标记参数无效")
	}
	acceptedAt = acceptedAt.UTC()
	err := mapError(r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var accountRow accountModel
		if err := tx.Select("id", "provider").First(&accountRow, id).Error; err != nil {
			return err
		}
		if account.Provider(accountRow.Provider) != account.ProviderWeb {
			return fmt.Errorf("仅 Grok Web 账号支持资料状态标记")
		}
		profile := webAccountProfileModel{
			AccountID: id, Tier: string(account.WebTierAuto),
			TermsAcceptedAt: &acceptedAt, TermsAcceptedVersion: version,
		}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&profile)
		if created.Error != nil || created.RowsAffected > 0 {
			return created.Error
		}
		return tx.Model(&webAccountProfileModel{}).
			Where("account_id = ? AND (terms_accepted_version < ? OR terms_accepted_at IS NULL)", id, version).
			Updates(map[string]any{"terms_accepted_at": acceptedAt, "terms_accepted_version": version}).Error
	}))
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderWeb, AccountID: id})
	}
	return err
}

// MarkWebBirthDateSet 幂等保存首次成功设置或确认已有生日的时间。
func (r *AccountRepository) MarkWebBirthDateSet(ctx context.Context, id uint64, setAt time.Time) error {
	if id == 0 || setAt.IsZero() {
		return fmt.Errorf("Web 生日标记参数无效")
	}
	err := r.markWebProfileTimestamp(ctx, id, "birth_date_set_at", setAt)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderWeb, AccountID: id})
	}
	return err
}

func (r *AccountRepository) markWebProfileTimestamp(ctx context.Context, id uint64, column string, value time.Time) error {
	value = value.UTC()
	return mapError(r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var accountRow accountModel
		if err := tx.Select("id", "provider").First(&accountRow, id).Error; err != nil {
			return err
		}
		if account.Provider(accountRow.Provider) != account.ProviderWeb {
			return fmt.Errorf("仅 Grok Web 账号支持资料状态标记")
		}
		profile := webAccountProfileModel{AccountID: id, Tier: string(account.WebTierAuto)}
		switch column {
		case "nsfw_enabled_at":
			profile.NSFWEnabledAt = &value
		case "birth_date_set_at":
			profile.BirthDateSetAt = &value
		default:
			return fmt.Errorf("Web 资料状态字段无效")
		}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&profile)
		if created.Error != nil || created.RowsAffected > 0 {
			return created.Error
		}
		switch column {
		case "nsfw_enabled_at":
			return tx.Model(&webAccountProfileModel{}).
				Where("account_id = ? AND nsfw_enabled_at IS NULL", id).
				Update("nsfw_enabled_at", value).Error
		case "birth_date_set_at":
			return tx.Model(&webAccountProfileModel{}).
				Where("account_id = ? AND birth_date_set_at IS NULL", id).
				Update("birth_date_set_at", value).Error
		default:
			return fmt.Errorf("Web 资料状态字段无效")
		}
	}))
}

func (r *AccountRepository) UpdateMany(ctx context.Context, ids []uint64, updates repository.AccountUpdates) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	values := make(map[string]any, 4)
	if updates.Enabled != nil {
		values["enabled"] = *updates.Enabled
	}
	if updates.Priority != nil {
		values["priority"] = *updates.Priority
	}
	if updates.MaxConcurrent != nil {
		values["max_concurrent"] = *updates.MaxConcurrent
	}
	if updates.MinimumRemaining != nil {
		values["minimum_remaining"] = *updates.MinimumRemaining
	}
	if len(values) == 0 {
		return 0, nil
	}
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("id IN ?", ids).Updates(values)
	if result.Error == nil && result.RowsAffected > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return result.RowsAffected, result.Error
}

// UpdateEgressBindings assigns one egress node to multiple accounts of one
// provider. A nil node clears the binding and restores normal pool selection.
func (r *AccountRepository) UpdateEgressBindings(ctx context.Context, providerValue account.Provider, ids []uint64, nodeID *uint64, mode account.EgressAssignmentMode, assignedAt time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	values := map[string]any{
		"egress_node_id": nodeID,
	}
	if nodeID == nil {
		values["egress_assignment_mode"] = ""
		values["egress_assigned_at"] = nil
	} else {
		values["egress_assignment_mode"] = string(mode)
		values["egress_assigned_at"] = assignedAt.UTC()
	}
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("provider = ? AND id IN ?", providerValue, ids).
		Updates(values)
	return result.RowsAffected, mapError(result.Error)
}

// ListEgressAssignments returns all accounts for one provider with their
// binding metadata. It deliberately includes disabled accounts so capacity
// reporting reflects every account that reserves a proxy slot.
func (r *AccountRepository) ListEgressAssignments(ctx context.Context, providerValue account.Provider) ([]account.Credential, error) {
	var rows []accountModel
	if err := r.db.db.WithContext(ctx).Preload("Credential").Preload("WebProfile").
		Where("provider = ?", providerValue).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	values := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		values = append(values, toAccountDomain(row))
	}
	return values, nil
}

func (r *AccountRepository) ListEgressBindingProviders(ctx context.Context, nodeID uint64) ([]account.Provider, error) {
	if nodeID == 0 {
		return []account.Provider{}, nil
	}
	return r.listEgressBindingProviders(r.db.db.WithContext(ctx).Model(&accountModel{}).Where("egress_node_id = ?", nodeID))
}

func (r *AccountRepository) ListEgressSourceBindingProviders(ctx context.Context, sourceID uint64) ([]account.Provider, error) {
	if sourceID == 0 {
		return []account.Provider{}, nil
	}
	query := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Joins("JOIN egress_nodes ON egress_nodes.id = provider_accounts.egress_node_id").
		Where("egress_nodes.source_id = ?", sourceID)
	return r.listEgressBindingProviders(query)
}

func (r *AccountRepository) listEgressBindingProviders(query *gorm.DB) ([]account.Provider, error) {
	var raw []string
	if err := query.Distinct("provider_accounts.provider").Order("provider_accounts.provider ASC").Pluck("provider_accounts.provider", &raw).Error; err != nil {
		return nil, mapError(err)
	}
	result := make([]account.Provider, 0, len(raw))
	for _, value := range raw {
		provider := account.Provider(value)
		if provider.IsValid() {
			result = append(result, provider)
		}
	}
	return result, nil
}

func (r *AccountRepository) Delete(ctx context.Context, id uint64) error {
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var lockedID uint64
		if err := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Pluck("id", &lockedID).Error; err != nil {
			return err
		}
		if lockedID == 0 {
			return repository.ErrNotFound
		}
		if err := rejectAccountsWithMediaJobs(tx, []uint64{id}); err != nil {
			return err
		}
		return mapError(tx.Delete(&accountModel{}, id).Error)
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return err
}

func (r *AccountRepository) DeleteMany(ctx context.Context, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var lockedIDs []uint64
		if err := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Pluck("id", &lockedIDs).Error; err != nil {
			return err
		}
		if err := rejectAccountsWithMediaJobs(tx, lockedIDs); err != nil {
			return err
		}
		result := tx.Where("id IN ?", lockedIDs).Delete(&accountModel{})
		deleted = result.RowsAffected
		return result.Error
	})
	if err == nil && deleted > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return deleted, err
}

func (r *AccountRepository) ListAutoCleanReauthCandidates(ctx context.Context, markedBefore time.Time, includeDisabled bool, afterID uint64, limit int) ([]uint64, error) {
	if limit < 1 {
		limit = 100
	}
	query := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Select("id").
		Where("auth_status = ? AND reauth_marked_at IS NOT NULL AND reauth_marked_at < ?", account.AuthStatusReauthRequired, markedBefore.UTC()).
		Where("NOT EXISTS (SELECT 1 FROM media_jobs job WHERE job.account_id = provider_accounts.id AND job.status IN ?)", []string{string(media.StatusQueued), string(media.StatusInProgress)})
	if afterID > 0 {
		query = query.Where("id > ?", afterID)
	}
	if !includeDisabled {
		query = query.Where("enabled = ?", true)
	}
	var candidates []uint64
	err := query.Order("id ASC").Limit(limit).Pluck("id", &candidates).Error
	return candidates, err
}

func (r *AccountRepository) DeleteAutoCleanReauthCandidates(ctx context.Context, markedBefore time.Time, includeDisabled bool, candidateIDs []uint64) ([]uint64, error) {
	if len(candidateIDs) == 0 {
		return []uint64{}, nil
	}
	deletedIDs := make([]uint64, 0, len(candidateIDs))
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		deletable, err := excludeAccountsWithActiveMediaJobs(tx, candidateIDs)
		if err != nil {
			return err
		}
		if len(deletable) == 0 {
			return nil
		}

		var lockedIDs []uint64
		lockQuery := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id IN ? AND auth_status = ? AND reauth_marked_at IS NOT NULL AND reauth_marked_at < ?", deletable, account.AuthStatusReauthRequired, markedBefore.UTC())
		if !includeDisabled {
			lockQuery = lockQuery.Where("enabled = ?", true)
		}
		if err := lockQuery.Pluck("id", &lockedIDs).Error; err != nil {
			return err
		}
		// lock 后再过滤活动视频任务，避免 list 与 delete 之间的 TOCTOU。
		lockedIDs, err = excludeAccountsWithActiveMediaJobs(tx, lockedIDs)
		if err != nil {
			return err
		}
		if len(lockedIDs) == 0 {
			return nil
		}
		deletion := tx.Where("id IN ? AND auth_status = ? AND reauth_marked_at IS NOT NULL AND reauth_marked_at < ?", lockedIDs, account.AuthStatusReauthRequired, markedBefore.UTC())
		if !includeDisabled {
			deletion = deletion.Where("enabled = ?", true)
		}
		result := deletion.Delete(&accountModel{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == int64(len(lockedIDs)) {
			deletedIDs = append(deletedIDs, lockedIDs...)
			return nil
		}
		var remaining []uint64
		if err := tx.Model(&accountModel{}).Where("id IN ?", lockedIDs).Pluck("id", &remaining).Error; err != nil {
			return err
		}
		remainingSet := make(map[uint64]struct{}, len(remaining))
		for _, id := range remaining {
			remainingSet[id] = struct{}{}
		}
		for _, id := range lockedIDs {
			if _, exists := remainingSet[id]; !exists {
				deletedIDs = append(deletedIDs, id)
			}
		}
		return nil
	})
	if err == nil && len(deletedIDs) > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return deletedIDs, err
}

// excludeAccountsWithActiveMediaJobs 返回无 queued/in_progress 视频任务的账号 ID（顺序保持输入顺序）。
func excludeAccountsWithActiveMediaJobs(db *gorm.DB, ids []uint64) ([]uint64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var blocked []uint64
	if err := db.Model(&mediaJobModel{}).
		Distinct("account_id").
		Where("account_id IN ? AND status IN ?", ids, []string{string(media.StatusQueued), string(media.StatusInProgress)}).
		Pluck("account_id", &blocked).Error; err != nil {
		return nil, err
	}
	if len(blocked) == 0 {
		out := make([]uint64, len(ids))
		copy(out, ids)
		return out, nil
	}
	blockedSet := make(map[uint64]struct{}, len(blocked))
	for _, id := range blocked {
		blockedSet[id] = struct{}{}
	}
	out := make([]uint64, 0, len(ids)-len(blocked))
	for _, id := range ids {
		if _, skip := blockedSet[id]; skip {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// rejectAccountsWithMediaJobs 仅保护仍需账号继续执行的活动视频任务。
// completed/failed 已保存账号名称等快照，删除账号后由外键 SET NULL 保留历史。
func rejectAccountsWithMediaJobs(db *gorm.DB, ids []uint64) error {
	var count int64
	if err := db.Model(&mediaJobModel{}).
		Where("account_id IN ? AND status IN ?", ids, []string{string(media.StatusQueued), string(media.StatusInProgress)}).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: 账号仍关联 %d 条排队中或进行中的视频任务，请等待任务结束后重试", repository.ErrConflict, count)
	}
	return nil
}

func (r *AccountRepository) DeleteAccountStatusBatch(ctx context.Context, providerValue account.Provider, status string, now time.Time, limit int) ([]uint64, int, error) {
	if limit < 1 {
		return []uint64{}, 0, nil
	}
	if status != "disabled" && status != "reauthRequired" && status != "cooldown" {
		return nil, 0, fmt.Errorf("不支持清理账号状态 %q", status)
	}
	deletedIDs := make([]uint64, 0, limit)
	candidateCount := 0
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidates []uint64
		selection := applyAccountStatusFilter(tx.Model(&accountModel{}).Where("provider = ?", providerValue), status, now)
		if err := selection.Order("id ASC").Limit(limit).Pluck("id", &candidates).Error; err != nil || len(candidates) == 0 {
			return err
		}
		candidateCount = len(candidates)
		if err := rejectAccountsWithMediaJobs(tx, candidates); err != nil {
			return err
		}
		deletion := applyAccountStatusFilter(tx.Where("id IN ?", candidates), status, now).Delete(&accountModel{})
		if deletion.Error != nil {
			return deletion.Error
		}
		if deletion.RowsAffected == int64(len(candidates)) {
			deletedIDs = append(deletedIDs, candidates...)
			return nil
		}
		var remaining []uint64
		if err := tx.Model(&accountModel{}).Where("id IN ?", candidates).Pluck("id", &remaining).Error; err != nil {
			return err
		}
		remainingSet := make(map[uint64]struct{}, len(remaining))
		for _, id := range remaining {
			remainingSet[id] = struct{}{}
		}
		for _, id := range candidates {
			if _, exists := remainingSet[id]; !exists {
				deletedIDs = append(deletedIDs, id)
			}
		}
		return nil
	})
	if err == nil && len(deletedIDs) > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: providerValue})
	}
	return deletedIDs, candidateCount, err
}

func applyAccountStatusFilter(query *gorm.DB, status string, now time.Time) *gorm.DB {
	switch status {
	case "active":
		return query.Where("enabled = ? AND auth_status = ? AND NOT "+accountRecoveryPredicate+" AND NOT "+providerQuotaExhaustedPredicate+" AND (cooldown_until IS NULL OR cooldown_until <= ?)", true, account.AuthStatusActive, now)
	case "disabled":
		return query.Where("enabled = ?", false)
	case "reauthRequired":
		return query.Where("enabled = ? AND auth_status = ?", true, account.AuthStatusReauthRequired)
	case "cooldown":
		return query.Where("enabled = ? AND auth_status = ? AND NOT "+accountRecoveryPredicate+" AND cooldown_until > ?", true, account.AuthStatusActive, now)
	case "waitingReset":
		return query.Where("enabled = ? AND auth_status = ? AND (EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'exhausted') OR "+providerQuotaExhaustedPredicate+")", true, account.AuthStatusActive)
	case "probing":
		return query.Where("enabled = ? AND auth_status = ? AND EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'probing')", true, account.AuthStatusActive)
	default:
		return query
	}
}

func (r *AccountRepository) UpdateTokens(ctx context.Context, id uint64, accessToken, refreshToken string, expiresAt time.Time) (account.Credential, error) {
	now := time.Now().UTC()
	refreshDueAt := account.CredentialRefreshDueAt(id, expiresAt)
	updates := map[string]any{
		"encrypted_primary": accessToken, "expires_at": expiresAt, "refresh_due_at": refreshDueAt,
		"last_refresh_at": now, "refresh_failures": 0, "last_refresh_error": "", "refresh_permanent": false, "updated_at": now,
	}
	if refreshToken != "" {
		updates["encrypted_refresh"] = refreshToken
	}
	if err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&accountCredentialModel{}).Where("account_id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Model(&accountModel{}).Where("id = ?", id).Updates(map[string]any{"auth_status": string(account.AuthStatusActive), "last_error": "", "reauth_marked_at": nil}).Error
	}); err != nil {
		return account.Credential{}, err
	}
	stored, err := r.Get(ctx, id)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged, Provider: stored.Provider, AccountID: id})
	} else {
		// The database write already committed; retain a broad fallback if the read-back fails.
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged, AccountID: id})
	}
	return stored, err
}

// BackfillCredentialRefreshSchedules 为升级前凭据分批补齐调度时间，不解密 Token，也不发起 OAuth 请求。
func (r *AccountRepository) BackfillCredentialRefreshSchedules(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	var rows []struct {
		AccountID        uint64
		ExpiresAt        *time.Time
		EncryptedPrimary string
	}
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.account_id, credential.expires_at, credential.encrypted_primary").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", account.ProviderBuild, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> '' AND credential.refresh_due_at IS NULL", account.AuthTypeOAuth).
		Where("credential.expires_at IS NOT NULL OR credential.encrypted_primary = ''").
		Order("credential.account_id ASC").Limit(limit).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	err = r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			dueAt := now
			if row.EncryptedPrimary != "" && row.ExpiresAt != nil && !row.ExpiresAt.IsZero() {
				dueAt = account.CredentialRefreshDueAt(row.AccountID, *row.ExpiresAt)
			}
			if err := tx.Model(&accountCredentialModel{}).Where("account_id = ? AND refresh_due_at IS NULL", row.AccountID).Update("refresh_due_at", dueAt).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return len(rows), err
}

// ListCriticalCredentialRefreshIDs 只返回重启后必须优先恢复的凭据，避免启动时刷新整个账号池。
func (r *AccountRepository) ListCriticalCredentialRefreshIDs(ctx context.Context, now, expiresBefore time.Time, limit int) ([]uint64, error) {
	if limit < 1 {
		return []uint64{}, nil
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", account.ProviderBuild, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> ''", account.AuthTypeOAuth).
		Where("credential.encrypted_primary = '' OR credential.expires_at <= ? OR (credential.refresh_failures > 0 AND credential.refresh_due_at IS NOT NULL AND credential.refresh_due_at <= ?)", expiresBefore.UTC(), now.UTC()).
		Order(gorm.Expr("CASE WHEN credential.encrypted_primary = '' THEN 0 WHEN credential.expires_at <= ? THEN 1 ELSE 2 END, credential.expires_at ASC, credential.account_id ASC", now.UTC())).
		Limit(limit).
		Scan(&ids).Error
	return ids, err
}

func (r *AccountRepository) ListDueCredentialRefreshIDs(ctx context.Context, now time.Time, limit int) ([]uint64, error) {
	if limit < 1 {
		return []uint64{}, nil
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", account.ProviderBuild, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> '' AND credential.refresh_due_at IS NOT NULL AND credential.refresh_due_at <= ?", account.AuthTypeOAuth, now).
		Order("credential.refresh_due_at ASC, credential.account_id ASC").Limit(limit).Scan(&ids).Error
	return ids, err
}

func (r *AccountRepository) NextCredentialRefreshDueAt(ctx context.Context) (*time.Time, error) {
	var rows []struct{ RefreshDueAt time.Time }
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.refresh_due_at").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", account.ProviderBuild, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> '' AND credential.refresh_due_at IS NOT NULL", account.AuthTypeOAuth).
		Order("credential.refresh_due_at ASC, credential.account_id ASC").Limit(1).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	value := rows[0].RefreshDueAt.UTC()
	return &value, nil
}

func (r *AccountRepository) UpdateCredentialRefreshFailure(ctx context.Context, id uint64, failureCount int, retryAt time.Time, errorCode string, permanent bool) error {
	err := r.db.db.WithContext(ctx).Model(&accountCredentialModel{}).Where("account_id = ?", id).Updates(map[string]any{
		"refresh_due_at": retryAt.UTC(), "refresh_failures": max(0, failureCount),
		"last_refresh_error": truncate(errorCode, 100), "refresh_permanent": permanent, "updated_at": time.Now().UTC(),
	}).Error
	if err == nil && permanent {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged, AccountID: id})
	}
	return err
}

func (r *AccountRepository) UpdateObservedModel(ctx context.Context, id uint64, model string, observedAt time.Time) error {
	_, err := r.UpdateObservedModelIfNewer(ctx, id, model, observedAt)
	return err
}

func (r *AccountRepository) UpdateObservedModelIfNewer(ctx context.Context, id uint64, model string, observedAt time.Time) (bool, error) {
	model = truncate(model, 255)
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("id = ? AND (observed_model_at IS NULL OR observed_model_at <= ?) AND (COALESCE(observed_model, '') <> ? OR observed_model_at <= ?)", id, observedAt, model, observedAt.Add(-30*time.Minute)).
		Updates(map[string]any{"observed_model": model, "observed_model_at": observedAt})
	if result.Error == nil && result.RowsAffected > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, AccountID: id})
	}
	return result.RowsAffected > 0, result.Error
}

// MarkBuildAPIFallback idempotently updates the XAI inference fallback marker for Grok Build accounts.
func (r *AccountRepository) MarkBuildAPIFallback(ctx context.Context, id uint64, enabled bool) error {
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("id = ? AND provider = ?", id, account.ProviderBuild).
		Update("build_api_fallback", enabled)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return repository.ErrNotFound
		}
		return fmt.Errorf("仅 grok_build 账号支持 Build API 降级标记")
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild, AccountID: id})
	return nil
}

func (r *AccountRepository) UpdateHealth(ctx context.Context, id uint64, failureCount int, cooldownUntil *time.Time, lastError string, success bool) error {
	updates := map[string]any{"failure_count": failureCount, "cooldown_until": cooldownUntil, "last_error": truncate(lastError, 512)}
	if success {
		now := time.Now().UTC()
		updates["last_used_at"] = &now
	}
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("id = ?", id).Updates(updates).Error
	if err == nil && !success {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, AccountID: id})
	}
	return err
}

func (r *AccountRepository) UpsertModelQuotaBlock(ctx context.Context, value account.ModelQuotaBlock) error {
	value.UpstreamModel = strings.TrimSpace(value.UpstreamModel)
	value.Reason = strings.TrimSpace(value.Reason)
	if value.AccountID == 0 || value.UpstreamModel == "" || value.Reason == "" || value.CooldownUntil.IsZero() {
		return repository.ErrConflict
	}
	now := time.Now().UTC()
	row := accountModelQuotaBlockModel{
		AccountID: value.AccountID, UpstreamModel: truncate(value.UpstreamModel, 255), Reason: truncate(value.Reason, 100),
		CooldownUntil: value.CooldownUntil.UTC(), UpdatedAt: now,
	}
	err := r.db.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "account_id"}, {Name: "upstream_model"}},
		DoUpdates: clause.Assignments(map[string]any{
			"reason":         gorm.Expr("CASE WHEN cooldown_until > ? THEN reason ELSE ? END", row.CooldownUntil, row.Reason),
			"cooldown_until": gorm.Expr("CASE WHEN cooldown_until > ? THEN cooldown_until ELSE ? END", row.CooldownUntil, row.CooldownUntil), "updated_at": now,
		}),
	}).Create(&row).Error
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountModelQuotaChanged, AccountID: value.AccountID, UpstreamModel: value.UpstreamModel})
	}
	return err
}

func (r *AccountRepository) PruneExpiredModelQuotaBlocks(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []accountModelQuotaBlockModel
	if err := r.db.db.WithContext(ctx).Select("account_id", "upstream_model").Where("cooldown_until <= ?", now.UTC()).Order("cooldown_until ASC").Limit(limit).Find(&rows).Error; err != nil || len(rows) == 0 {
		return 0, err
	}
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			result := tx.Where("account_id = ? AND upstream_model = ? AND cooldown_until <= ?", row.AccountID, row.UpstreamModel, now.UTC()).Delete(&accountModelQuotaBlockModel{})
			if result.Error != nil {
				return result.Error
			}
			deleted += result.RowsAffected
		}
		return nil
	})
	if err == nil && deleted > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountModelQuotaChanged})
	}
	return deleted, err
}

func (r *AccountRepository) SaveBilling(ctx context.Context, value account.Billing) error {
	history, err := json.Marshal(value.History)
	if err != nil {
		return err
	}
	row := billingModel{AccountID: value.AccountID, PlanCode: truncate(value.PlanCode, 100), PlanName: truncate(value.PlanName, 160), MonthlyLimit: value.MonthlyLimit, Used: value.Used, OnDemandCap: value.OnDemandCap, OnDemandUsed: value.OnDemandUsed, PrepaidBalance: value.PrepaidBalance, CreditUsagePercent: value.CreditUsagePercent, IsUnifiedBillingUser: value.IsUnifiedBillingUser, OnDemandEnabled: value.OnDemandEnabled, TopUpMethod: truncate(value.TopUpMethod, 100), UsagePeriodType: truncate(value.UsagePeriodType, 100), UsagePeriodStart: truncate(value.UsagePeriodStart, 64), UsagePeriodEnd: truncate(value.UsagePeriodEnd, 64), BillingPeriodStart: truncate(value.BillingPeriodStart, 64), BillingPeriodEnd: truncate(value.BillingPeriodEnd, 64), HistoryJSON: string(history), SyncedAt: value.SyncedAt}
	err = r.db.db.WithContext(ctx).Save(&row).Error
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountBillingChanged, AccountID: value.AccountID})
	}
	return err
}

func (r *AccountRepository) GetBilling(ctx context.Context, accountID uint64) (account.Billing, error) {
	var row billingModel
	if err := r.db.db.WithContext(ctx).First(&row, "account_id = ?", accountID).Error; err != nil {
		return account.Billing{}, mapError(err)
	}
	return toBillingDomain(row), nil
}

func (r *AccountRepository) GetBillings(ctx context.Context, accountIDs []uint64) (map[uint64]account.Billing, error) {
	result := make(map[uint64]account.Billing, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []billingModel
	if err := r.db.db.WithContext(ctx).Where("account_id IN ?", accountIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = toBillingDomain(row)
	}
	return result, nil
}

func (r *AccountRepository) GetQuotaRecovery(ctx context.Context, accountID uint64) (account.QuotaRecovery, error) {
	var row quotaRecoveryModel
	if err := r.db.db.WithContext(ctx).First(&row, "account_id = ?", accountID).Error; err != nil {
		return account.QuotaRecovery{}, mapError(err)
	}
	return account.QuotaRecovery{
		AccountID: row.AccountID, Kind: account.QuotaRecoveryKind(row.Kind), Status: account.QuotaRecoveryStatus(row.Status), ConfirmedUsed: row.ConfirmedUsed,
		ConfirmedLimit: row.ConfirmedLimit, ExhaustedAt: row.ExhaustedAt, NextProbeAt: row.NextProbeAt,
		LastConfirmedAt: row.LastConfirmedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *AccountRepository) GetQuotaRecoveries(ctx context.Context, accountIDs []uint64) (map[uint64]account.QuotaRecovery, error) {
	result := make(map[uint64]account.QuotaRecovery, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []quotaRecoveryModel
	if err := r.db.db.WithContext(ctx).Where("account_id IN ?", accountIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = account.QuotaRecovery{
			AccountID: row.AccountID, Kind: account.QuotaRecoveryKind(row.Kind), Status: account.QuotaRecoveryStatus(row.Status), ConfirmedUsed: row.ConfirmedUsed,
			ConfirmedLimit: row.ConfirmedLimit, ExhaustedAt: row.ExhaustedAt, NextProbeAt: row.NextProbeAt,
			LastConfirmedAt: row.LastConfirmedAt, UpdatedAt: row.UpdatedAt,
		}
	}
	return result, nil
}

func (r *AccountRepository) SaveQuotaRecovery(ctx context.Context, value account.QuotaRecovery) error {
	row := quotaRecoveryModel{
		AccountID: value.AccountID, Kind: string(value.Kind), Status: string(value.Status), ConfirmedUsed: value.ConfirmedUsed,
		ConfirmedLimit: value.ConfirmedLimit, ExhaustedAt: value.ExhaustedAt, NextProbeAt: value.NextProbeAt,
		LastConfirmedAt: value.LastConfirmedAt, UpdatedAt: value.UpdatedAt,
	}
	err := r.db.db.WithContext(ctx).Save(&row).Error
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountRecoveryChanged, AccountID: value.AccountID})
	}
	return err
}

func (r *AccountRepository) ClaimQuotaProbe(ctx context.Context, accountID uint64, now, leaseUntil time.Time) (bool, error) {
	result := r.db.db.WithContext(ctx).Model(&quotaRecoveryModel{}).
		Where("account_id = ? AND status IN ? AND next_probe_at IS NOT NULL AND next_probe_at <= ?", accountID, []string{string(account.QuotaRecoveryStatusExhausted), string(account.QuotaRecoveryStatusProbing)}, now).
		Updates(map[string]any{"status": string(account.QuotaRecoveryStatusProbing), "next_probe_at": leaseUntil, "updated_at": now})
	return result.RowsAffected == 1, result.Error
}

func (r *AccountRepository) ClearQuotaRecovery(ctx context.Context, accountID uint64) error {
	err := r.db.db.WithContext(ctx).Delete(&quotaRecoveryModel{}, "account_id = ?", accountID).Error
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountRecoveryChanged, AccountID: accountID})
	}
	return err
}

func (r *AccountRepository) HasQuotaWindows(ctx context.Context, accountID uint64) (bool, error) {
	var count int64
	err := r.db.db.WithContext(ctx).Model(&quotaWindowModel{}).Where("account_id = ? AND synced_at IS NOT NULL", accountID).Count(&count).Error
	return count > 0, err
}

func (r *AccountRepository) GetQuotaWindows(ctx context.Context, accountIDs []uint64) (map[uint64][]account.QuotaWindow, error) {
	result := make(map[uint64][]account.QuotaWindow, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []quotaWindowModel
	if err := r.db.db.WithContext(ctx).Where("account_id IN ?", accountIDs).Order("account_id ASC, mode ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = append(result[row.AccountID], toQuotaWindowDomain(row))
	}
	return result, nil
}

func (r *AccountRepository) SaveQuotaWindows(ctx context.Context, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow) error {
	err := r.saveQuotaWindows(ctx, accountID, tier, syncedAt, values, false)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: accountID})
	}
	return err
}

func (r *AccountRepository) ReplaceQuotaWindows(ctx context.Context, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow) error {
	err := r.saveQuotaWindows(ctx, accountID, tier, syncedAt, values, true)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: accountID})
	}
	return err
}

func (r *AccountRepository) saveQuotaWindows(ctx context.Context, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow, replace bool) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tier != "" {
			profile := webAccountProfileModel{AccountID: accountID, Tier: string(tier), SyncedAt: &syncedAt}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoUpdates: clause.AssignmentColumns([]string{"tier", "synced_at"})}).Create(&profile).Error; err != nil {
				return err
			}
		}
		if replace {
			if err := tx.Where("account_id = ?", accountID).Delete(&quotaWindowModel{}).Error; err != nil {
				return err
			}
		}
		for _, value := range values {
			serializedBreakdown := make([]quotaBreakdownJSON, 0, len(value.Breakdown))
			for _, item := range value.Breakdown {
				serializedBreakdown = append(serializedBreakdown, quotaBreakdownJSON{ProductCode: item.ProductCode, UsagePercent: item.UsagePercent})
			}
			breakdownJSON, err := json.Marshal(serializedBreakdown)
			if err != nil {
				return err
			}
			row := quotaWindowModel{
				AccountID: accountID, Mode: truncate(strings.TrimSpace(value.Mode), 64), Remaining: max(0, value.Remaining), Total: max(0, value.Total),
				UsagePercent: min(100, max(0, value.UsagePercent)), BreakdownJSON: string(breakdownJSON),
				WindowSeconds: max(0, value.WindowSeconds), ResetAt: value.ResetAt, SyncedAt: value.SyncedAt, Source: string(value.Source), UpdatedAt: syncedAt,
			}
			if row.Source == "" {
				row.Source = string(account.QuotaSourceUpstream)
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "account_id"}, {Name: "mode"}},
				DoUpdates: clause.AssignmentColumns([]string{"remaining", "total", "usage_percent", "breakdown_json", "window_seconds", "reset_at", "synced_at", "source", "updated_at"}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *AccountRepository) DecrementQuotaWindow(ctx context.Context, accountID uint64, mode string, now time.Time) (bool, error) {
	result := r.db.db.WithContext(ctx).Model(&quotaWindowModel{}).
		Where("account_id = ? AND mode = ? AND remaining > 0", accountID, mode).
		Updates(map[string]any{"remaining": gorm.Expr("remaining - 1"), "updated_at": now})
	return result.RowsAffected == 1, result.Error
}

func (r *AccountRepository) DecrementQuotaWindowBy(ctx context.Context, accountID uint64, mode string, amount int, now time.Time) (bool, error) {
	if amount <= 0 {
		amount = 1
	}
	result := r.db.db.WithContext(ctx).Model(&quotaWindowModel{}).
		Where("account_id = ? AND mode = ? AND remaining > 0", accountID, mode).
		Updates(map[string]any{
			"remaining":  gorm.Expr("CASE WHEN remaining <= ? THEN 0 ELSE remaining - ? END", amount, amount),
			"updated_at": now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *AccountRepository) ExhaustQuotaWindow(ctx context.Context, accountID uint64, mode string, resetAt *time.Time, now time.Time) error {
	err := r.db.db.WithContext(ctx).Model(&quotaWindowModel{}).Where("account_id = ? AND mode = ?", accountID, mode).
		Updates(map[string]any{"remaining": 0, "reset_at": resetAt, "updated_at": now}).Error
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: accountID})
	}
	return err
}

func (r *AccountRepository) ListDueQuotaWindows(ctx context.Context, now time.Time, limit int) ([]account.QuotaWindow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []quotaWindowModel
	if err := r.db.db.WithContext(ctx).Where("remaining = 0 AND reset_at IS NOT NULL AND reset_at <= ?", now).Order("reset_at ASC, account_id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]account.QuotaWindow, 0, len(rows))
	for _, row := range rows {
		values = append(values, toQuotaWindowDomain(row))
	}
	return values, nil
}

func (r *AccountRepository) ListQuotaRecoveryWindows(ctx context.Context, limit int) ([]account.QuotaWindow, error) {
	if limit <= 0 || limit > 100000 {
		limit = 100000
	}
	var rows []quotaWindowModel
	if err := r.db.db.WithContext(ctx).Where("remaining = 0 AND reset_at IS NOT NULL").Order("reset_at ASC, account_id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]account.QuotaWindow, 0, len(rows))
	for _, row := range rows {
		values = append(values, toQuotaWindowDomain(row))
	}
	return values, nil
}

// ListStaleWebQuotaAccountIDs 返回缺失或长期未同步额度的 Web 账号，供重启后的低优先级追赶任务使用。
func (r *AccountRepository) ListStaleWebQuotaAccountIDs(ctx context.Context, before time.Time, limit int) ([]uint64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Joins("LEFT JOIN account_quota_windows AS quota ON quota.account_id = account.id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", account.ProviderWeb, true, account.AuthStatusActive).
		Group("account.id").
		Having("MAX(quota.synced_at) IS NULL OR MAX(quota.synced_at) < ?", before.UTC()).
		Order("MIN(quota.synced_at) ASC, account.id ASC").
		Limit(limit).
		Scan(&ids).Error
	return ids, err
}

func toQuotaWindowDomain(row quotaWindowModel) account.QuotaWindow {
	var serializedBreakdown []quotaBreakdownJSON
	_ = json.Unmarshal([]byte(row.BreakdownJSON), &serializedBreakdown)
	breakdown := make([]account.QuotaBreakdown, 0, len(serializedBreakdown))
	for _, item := range serializedBreakdown {
		breakdown = append(breakdown, account.QuotaBreakdown{ProductCode: item.ProductCode, UsagePercent: item.UsagePercent})
	}
	return account.QuotaWindow{
		AccountID: row.AccountID, Mode: row.Mode, Remaining: row.Remaining, Total: row.Total,
		UsagePercent: row.UsagePercent, Breakdown: breakdown, WindowSeconds: row.WindowSeconds,
		ResetAt: row.ResetAt, SyncedAt: row.SyncedAt, Source: account.QuotaSource(row.Source), UpdatedAt: row.UpdatedAt,
	}
}
