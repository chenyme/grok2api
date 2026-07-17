package relational

import (
	"context"
	"fmt"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	dashboarddomain "github.com/chenyme/grok2api/backend/internal/domain/dashboard"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type DashboardRepository struct{ db *Database }

func NewDashboardRepository(db *Database) *DashboardRepository { return &DashboardRepository{db: db} }

const dashboardUsageAggregateSelect = "COUNT(*) AS requests, COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0) AS successful_requests, COALESCE(SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END), 0) AS failed_requests, COALESCE(SUM(input_tokens), 0) AS input_tokens, COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens, COALESCE(SUM(output_tokens), 0) AS output_tokens, COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens, COALESCE(SUM(total_tokens), 0) AS tokens, COALESCE(SUM(CASE WHEN cost_in_usd_ticks > 0 THEN cost_in_usd_ticks ELSE estimated_cost_in_usd_ticks END), 0) AS billed_cost_usd_ticks"

const dashboardTopModelsLimit = 10

// Snapshot 在同一数据库事务内读取资源计数和指定区间的审计聚合。
func (r *DashboardRepository) Snapshot(ctx context.Context, window repository.DashboardSnapshotWindow, snapshotAt time.Time) (dashboarddomain.Aggregate, error) {
	if err := validateDashboardBoundaries(window.BucketBoundaries); err != nil {
		return dashboarddomain.Aggregate{}, err
	}
	if err := validateDashboardBoundaries(window.ActivityBoundaries); err != nil {
		return dashboarddomain.Aggregate{}, err
	}
	bucketBoundaries := window.BucketBoundaries
	start := bucketBoundaries[0]
	end := bucketBoundaries[len(bucketBoundaries)-1]
	bucketExpression, bucketArgs := dashboardBucketExpression(bucketBoundaries)
	activityExpression, activityArgs := dashboardBucketExpression(window.ActivityBoundaries)
	result := dashboarddomain.Aggregate{}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var accounts struct {
			Total           int64
			Active          int64
			BuildAccounts   int64
			WebAccounts     int64
			ConsoleAccounts int64
		}
		if err := tx.Model(&accountModel{}).
			Select("COUNT(*) AS total, COALESCE(SUM(CASE WHEN enabled = ? AND auth_status = ? AND (cooldown_until IS NULL OR cooldown_until <= ?) AND NOT EXISTS (SELECT 1 FROM account_quota_recovery WHERE account_quota_recovery.account_id = provider_accounts.id AND account_quota_recovery.status IN ?) THEN 1 ELSE 0 END), 0) AS active, COALESCE(SUM(CASE WHEN provider = 'grok_build' THEN 1 ELSE 0 END), 0) AS build_accounts, COALESCE(SUM(CASE WHEN provider = 'grok_web' THEN 1 ELSE 0 END), 0) AS web_accounts, COALESCE(SUM(CASE WHEN provider = 'grok_console' THEN 1 ELSE 0 END), 0) AS console_accounts", true, "active", snapshotAt, []string{"exhausted", "probing"}).
			Scan(&accounts).Error; err != nil {
			return err
		}

		var models struct {
			Total   int64
			Enabled int64
		}
		if err := tx.Model(&modelRouteModel{}).
			Select("COUNT(*) AS total, COALESCE(SUM(CASE WHEN enabled = ? AND "+availableRoutePredicate+" THEN 1 ELSE 0 END), 0) AS enabled", true, true, "active").
			Scan(&models).Error; err != nil {
			return err
		}

		var clientKeys struct {
			Total  int64
			Active int64
		}
		if err := tx.Model(&clientKeyModel{}).
			Select("COUNT(*) AS total, COALESCE(SUM(CASE WHEN enabled = ? AND (expires_at IS NULL OR expires_at > ?) THEN 1 ELSE 0 END), 0) AS active", true, snapshotAt).
			Scan(&clientKeys).Error; err != nil {
			return err
		}

		result.Resources.ActiveAccounts = accounts.Active
		result.Resources.TotalAccounts = accounts.Total
		result.Resources.BuildAccounts = accounts.BuildAccounts
		result.Resources.WebAccounts = accounts.WebAccounts
		result.Resources.ConsoleAccounts = accounts.ConsoleAccounts
		result.Resources.EnabledModels = models.Enabled
		result.Resources.TotalModels = models.Total
		result.Resources.ActiveClientKeys = clientKeys.Active
		result.Resources.TotalClientKeys = clientKeys.Total

		if err := tx.Model(&requestAuditModel{}).
			Select(dashboardUsageAggregateSelect).
			Where("created_at >= ? AND created_at < ?", start, end).
			Scan(&result.Usage).Error; err != nil {
			return err
		}
		var buckets []struct {
			BucketIndex        int `gorm:"column:bucket_index"`
			Requests           int64
			InputTokens        int64
			CachedInputTokens  int64
			OutputTokens       int64
			ReasoningTokens    int64
			Tokens             int64
			BilledCostUSDTicks int64
		}
		if err := tx.Model(&requestAuditModel{}).
			Select(bucketExpression+" AS bucket_index, COUNT(*) AS requests, COALESCE(SUM(input_tokens), 0) AS input_tokens, COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens, COALESCE(SUM(output_tokens), 0) AS output_tokens, COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens, COALESCE(SUM(total_tokens), 0) AS tokens, COALESCE(SUM(CASE WHEN cost_in_usd_ticks > 0 THEN cost_in_usd_ticks ELSE estimated_cost_in_usd_ticks END), 0) AS billed_cost_usd_ticks", bucketArgs...).
			Where("created_at >= ? AND created_at < ?", start, end).
			Group("bucket_index").
			Order("bucket_index ASC").
			Scan(&buckets).Error; err != nil {
			return err
		}
		result.Buckets = make([]dashboarddomain.Bucket, 0, len(buckets))
		for _, bucket := range buckets {
			result.Buckets = append(result.Buckets, dashboarddomain.Bucket{Index: bucket.BucketIndex, Requests: bucket.Requests, InputTokens: bucket.InputTokens, CachedInputTokens: bucket.CachedInputTokens, OutputTokens: bucket.OutputTokens, ReasoningTokens: bucket.ReasoningTokens, Tokens: bucket.Tokens, BilledCostUSDTicks: bucket.BilledCostUSDTicks})
		}

		var activityBuckets []struct {
			BucketIndex int `gorm:"column:bucket_index"`
			Requests    int64
		}
		if err := tx.Model(&requestAuditModel{}).
			Select(activityExpression+" AS bucket_index, COUNT(*) AS requests", activityArgs...).
			Where("created_at >= ? AND created_at < ?", window.ActivityBoundaries[0], window.ActivityBoundaries[len(window.ActivityBoundaries)-1]).
			Group("bucket_index").
			Order("bucket_index ASC").
			Scan(&activityBuckets).Error; err != nil {
			return err
		}
		result.ActivityBuckets = make([]dashboarddomain.ActivityBucket, 0, len(activityBuckets))
		for _, bucket := range activityBuckets {
			result.ActivityBuckets = append(result.ActivityBuckets, dashboarddomain.ActivityBucket{Index: bucket.BucketIndex, Requests: bucket.Requests})
		}

		var providers []struct {
			Provider           string
			Requests           int64
			SuccessfulRequests int64
			Tokens             int64
		}
		if err := tx.Model(&requestAuditModel{}).
			Select("provider, COUNT(*) AS requests, COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0) AS successful_requests, COALESCE(SUM(total_tokens), 0) AS tokens").
			Where("created_at >= ? AND created_at < ?", start, end).
			Group("provider").
			Order("requests DESC, provider ASC").
			Scan(&providers).Error; err != nil {
			return err
		}
		result.Providers = make([]dashboarddomain.ProviderUsage, 0, len(providers))
		for _, item := range providers {
			result.Providers = append(result.Providers, dashboarddomain.ProviderUsage{Provider: item.Provider, Requests: item.Requests, SuccessfulRequests: item.SuccessfulRequests, Tokens: item.Tokens})
		}

		modelExpression := "CASE WHEN TRIM(model_public_id) <> '' THEN model_public_id WHEN TRIM(model_upstream_model) <> '' THEN model_upstream_model ELSE 'unknown' END"
		var topModels []struct {
			Model              string
			Requests           int64
			InputTokens        int64
			CachedInputTokens  int64
			OutputTokens       int64
			ReasoningTokens    int64
			Tokens             int64
			BilledCostUSDTicks int64
		}
		if err := tx.Model(&requestAuditModel{}).
			Select(modelExpression+" AS model, COUNT(*) AS requests, COALESCE(SUM(input_tokens), 0) AS input_tokens, COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens, COALESCE(SUM(output_tokens), 0) AS output_tokens, COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens, COALESCE(SUM(total_tokens), 0) AS tokens, COALESCE(SUM(CASE WHEN cost_in_usd_ticks > 0 THEN cost_in_usd_ticks ELSE estimated_cost_in_usd_ticks END), 0) AS billed_cost_usd_ticks").
			Where("created_at >= ? AND created_at < ?", start, end).
			Group(modelExpression).
			Order("billed_cost_usd_ticks DESC, requests DESC, tokens DESC, model ASC").
			Limit(dashboardTopModelsLimit).
			Scan(&topModels).Error; err != nil {
			return err
		}
		result.TopModels = make([]dashboarddomain.ModelUsage, 0, dashboardTopModelsLimit)
		listedModels := make(map[string]struct{}, dashboardTopModelsLimit)
		for _, item := range topModels {
			result.TopModels = append(result.TopModels, dashboarddomain.ModelUsage{Model: item.Model, Requests: item.Requests, InputTokens: item.InputTokens, CachedInputTokens: item.CachedInputTokens, OutputTokens: item.OutputTokens, ReasoningTokens: item.ReasoningTokens, Tokens: item.Tokens, BilledCostUSDTicks: item.BilledCostUSDTicks})
			listedModels[item.Model] = struct{}{}
		}
		if len(result.TopModels) < dashboardTopModelsLimit {
			var enabledModels []struct {
				PublicID string
				Provider string
			}
			if err := tx.Model(&modelRouteModel{}).
				Select("public_id, provider").
				Where("enabled = ?", true).
				Order("public_id ASC").
				Limit(dashboardTopModelsLimit * len(accountdomain.Providers())).
				Scan(&enabledModels).Error; err != nil {
				return err
			}
			for _, route := range enabledModels {
				publicID := modeldomain.ExternalPublicID(accountdomain.Provider(route.Provider), route.PublicID)
				if publicID == "" {
					continue
				}
				if _, exists := listedModels[publicID]; exists {
					continue
				}
				result.TopModels = append(result.TopModels, dashboarddomain.ModelUsage{Model: publicID})
				listedModels[publicID] = struct{}{}
				if len(result.TopModels) == dashboardTopModelsLimit {
					break
				}
			}
		}
		return nil
	})
	return result, err
}

func validateDashboardBoundaries(boundaries []time.Time) error {
	if len(boundaries) < 2 {
		return fmt.Errorf("Dashboard 聚合范围无效")
	}
	for index := 1; index < len(boundaries); index++ {
		if !boundaries[index-1].Before(boundaries[index]) {
			return fmt.Errorf("Dashboard 时间桶无效")
		}
	}
	return nil
}

func dashboardBucketExpression(boundaries []time.Time) (string, []any) {
	var expression strings.Builder
	expression.WriteString("CASE")
	args := make([]any, 0)
	for index := 0; index < len(boundaries)-1; index++ {
		expression.WriteString(" WHEN created_at >= ? AND created_at < ? THEN ?")
		args = append(args, boundaries[index], boundaries[index+1], index)
	}
	expression.WriteString(" ELSE -1 END")
	return expression.String(), args
}
