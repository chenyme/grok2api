package relational

import (
	"context"
	"fmt"
	"strings"
	"time"

	dashboarddomain "github.com/chenyme/grok2api/backend/internal/domain/dashboard"
	"gorm.io/gorm"
)

type DashboardRepository struct{ db *Database }

func NewDashboardRepository(db *Database) *DashboardRepository { return &DashboardRepository{db: db} }

// Snapshot 在同一数据库事务内读取资源计数和指定区间的审计聚合。
func (r *DashboardRepository) Snapshot(ctx context.Context, bucketBoundaries []time.Time, snapshotAt time.Time) (dashboarddomain.Aggregate, error) {
	if len(bucketBoundaries) < 2 {
		return dashboarddomain.Aggregate{}, fmt.Errorf("Dashboard 聚合范围无效")
	}
	for index := 1; index < len(bucketBoundaries); index++ {
		if !bucketBoundaries[index-1].Before(bucketBoundaries[index]) {
			return dashboarddomain.Aggregate{}, fmt.Errorf("Dashboard 时间桶无效")
		}
	}
	start := bucketBoundaries[0]
	end := bucketBoundaries[len(bucketBoundaries)-1]
	bucketExpression, bucketArgs := dashboardBucketExpression(bucketBoundaries)
	result := dashboarddomain.Aggregate{}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var accounts struct {
			Total  int64
			Active int64
		}
		if err := tx.Model(&accountModel{}).
			Select("COUNT(*) AS total, COALESCE(SUM(CASE WHEN enabled = ? AND auth_status = ? AND (cooldown_until IS NULL OR cooldown_until <= ?) AND NOT EXISTS (SELECT 1 FROM account_quota_recovery WHERE account_quota_recovery.account_id = provider_accounts.id AND account_quota_recovery.status IN ?) THEN 1 ELSE 0 END), 0) AS active", true, "active", snapshotAt, []string{"exhausted", "probing"}).
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

		if err := tx.Model(&requestAuditModel{}).Count(&result.Resources.AllTimeRequests).Error; err != nil {
			return err
		}
		result.Resources.ActiveAccounts = accounts.Active
		result.Resources.TotalAccounts = accounts.Total
		result.Resources.EnabledModels = models.Enabled
		result.Resources.TotalModels = models.Total
		result.Resources.ActiveClientKeys = clientKeys.Active
		result.Resources.TotalClientKeys = clientKeys.Total

		if err := tx.Model(&requestAuditModel{}).
			Select("COUNT(*) AS requests, COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0) AS successful_requests, COALESCE(SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END), 0) AS failed_requests, COALESCE(SUM(input_tokens), 0) AS input_tokens, COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens, COALESCE(SUM(output_tokens), 0) AS output_tokens, COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens, COALESCE(SUM(total_tokens), 0) AS tokens, COALESCE(SUM(CASE WHEN cost_in_usd_ticks > 0 THEN cost_in_usd_ticks ELSE estimated_cost_in_usd_ticks END), 0) AS billed_cost_usd_ticks").
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
			Order("requests DESC, tokens DESC, model ASC").
			Limit(10).
			Scan(&topModels).Error; err != nil {
			return err
		}
		result.TopModels = make([]dashboarddomain.ModelUsage, 0, len(topModels))
		topModelNames := make([]string, 0, len(topModels))
		for _, item := range topModels {
			result.TopModels = append(result.TopModels, dashboarddomain.ModelUsage{Model: item.Model, Requests: item.Requests, InputTokens: item.InputTokens, CachedInputTokens: item.CachedInputTokens, OutputTokens: item.OutputTokens, ReasoningTokens: item.ReasoningTokens, Tokens: item.Tokens, BilledCostUSDTicks: item.BilledCostUSDTicks})
			topModelNames = append(topModelNames, item.Model)
		}
		if len(topModelNames) > 0 {
			var modelBuckets []struct {
				BucketIndex        int `gorm:"column:bucket_index"`
				Model              string
				Tokens             int64
				BilledCostUSDTicks int64
			}
			if err := tx.Model(&requestAuditModel{}).
				Select(bucketExpression+" AS bucket_index, "+modelExpression+" AS model, COALESCE(SUM(total_tokens), 0) AS tokens, COALESCE(SUM(CASE WHEN cost_in_usd_ticks > 0 THEN cost_in_usd_ticks ELSE estimated_cost_in_usd_ticks END), 0) AS billed_cost_usd_ticks", bucketArgs...).
				Where("created_at >= ? AND created_at < ?", start, end).
				Where(modelExpression+" IN ?", topModelNames).
				Group("bucket_index, " + modelExpression).
				Order("bucket_index ASC, tokens DESC, model ASC").
				Scan(&modelBuckets).Error; err != nil {
				return err
			}
			result.ModelBuckets = make([]dashboarddomain.ModelBucket, 0, len(modelBuckets))
			for _, item := range modelBuckets {
				result.ModelBuckets = append(result.ModelBuckets, dashboarddomain.ModelBucket{Index: item.BucketIndex, Model: item.Model, Tokens: item.Tokens, BilledCostUSDTicks: item.BilledCostUSDTicks})
			}
		}
		return nil
	})
	return result, err
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
