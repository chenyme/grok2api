package relational

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AuditRepository struct{ db *Database }

func NewAuditRepository(db *Database) *AuditRepository { return &AuditRepository{db: db} }

func (r *AuditRepository) Create(ctx context.Context, value audit.Record) error {
	row := toAuditModel(value)
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return createAuditAndBill(tx, row)
	})
}

func (r *AuditRepository) CreateBatch(ctx context.Context, values []audit.Record) error {
	if len(values) == 0 {
		return nil
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		insertedRows := make([]requestAuditModel, 0, len(values))
		for _, value := range values {
			row := toAuditModel(value)
			inserted, err := insertAudit(tx, row)
			if err != nil {
				return err
			}
			if inserted {
				insertedRows = append(insertedRows, row)
			}
		}
		if len(insertedRows) == 0 {
			return nil
		}
		eventIDs := make([]string, 0, len(insertedRows))
		for _, row := range insertedRows {
			eventIDs = append(eventIDs, row.EventID)
		}
		var reservations []billingReservationModel
		if err := tx.Where("event_id IN ?", eventIDs).Find(&reservations).Error; err != nil {
			return err
		}
		reservationByEvent := make(map[string]billingReservationModel, len(reservations))
		for _, reservation := range reservations {
			reservationByEvent[reservation.EventID] = reservation
		}
		for _, row := range insertedRows {
			reservation, hasReservation := reservationByEvent[row.EventID]
			if err := billInsertedAudit(tx, row, reservation, hasReservation); err != nil {
				return err
			}
		}
		return nil
	})
}

func toAuditModel(value audit.Record) requestAuditModel {
	provider := value.Provider
	if provider == "" {
		provider = "grok_build"
	}
	operation := value.Operation
	if operation == "" {
		operation = audit.OperationResponses
	}
	usageSource := value.UsageSource
	if usageSource == "" {
		usageSource = audit.UsageSourceUpstream
	}
	eventID := strings.TrimSpace(value.EventID)
	if eventID == "" {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%d", value.RequestID, value.ClientKeyID, value.ModelRouteID, value.CreatedAt.UnixNano())))
		eventID = fmt.Sprintf("evt_%x", digest[:18])
	}
	return requestAuditModel{
		EventID: truncate(eventID, 64), RequestID: truncate(value.RequestID, 64), ClientKeyID: value.ClientKeyID, ClientKeyName: truncate(value.ClientKeyName, 160),
		ModelRouteID: value.ModelRouteID, ModelPublicID: truncate(value.ModelPublicID, 255), ModelUpstreamModel: truncate(value.ModelUpstreamModel, 255),
		Provider: truncate(provider, 32), Operation: string(operation), UsageSource: string(usageSource),
		AccountID: value.AccountID, AccountName: truncate(value.AccountName, 160), StatusCode: value.StatusCode, Streaming: value.Streaming,
		MediaInputImages: nonNegative(value.MediaInputImages), MediaOutputImages: nonNegative(value.MediaOutputImages), MediaOutputSeconds: nonNegative(value.MediaOutputSeconds),
		InputTokens: nonNegative(value.InputTokens), CachedInputTokens: nonNegative(value.CachedInputTokens), OutputTokens: nonNegative(value.OutputTokens),
		ReasoningTokens: nonNegative(value.ReasoningTokens), TotalTokens: nonNegative(value.TotalTokens), CostInUSDTicks: nonNegative(value.CostInUSDTicks),
		EstimatedCostInUSDTicks: nonNegative(value.EstimatedCostInUSDTicks), PricingModel: truncate(value.PricingModel, 100), PricingVersion: truncate(value.PricingVersion, 20),
		NumSourcesUsed: nonNegative(value.NumSourcesUsed), NumServerSideToolsUsed: nonNegative(value.NumServerSideToolsUsed),
		ContextInputTokens: nonNegative(value.ContextInputTokens), ContextOutputTokens: nonNegative(value.ContextOutputTokens), DurationMS: nonNegative(value.DurationMS),
		ErrorCode: truncate(value.ErrorCode, 100), CreatedAt: value.CreatedAt,
	}
}

func createAuditAndBill(tx *gorm.DB, row requestAuditModel) error {
	inserted, err := insertAudit(tx, row)
	if err != nil || !inserted {
		return err
	}
	var reservation billingReservationModel
	reservationErr := tx.Where("event_id = ?", row.EventID).First(&reservation).Error
	if reservationErr != nil && !errors.Is(reservationErr, gorm.ErrRecordNotFound) {
		return reservationErr
	}
	return billInsertedAudit(tx, row, reservation, reservationErr == nil)
}

func insertAudit(tx *gorm.DB, row requestAuditModel) (bool, error) {
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	return result.RowsAffected == 1, result.Error
}

func billInsertedAudit(tx *gorm.DB, row requestAuditModel, reservation billingReservationModel, hasReservation bool) error {
	amount := row.CostInUSDTicks
	if amount <= 0 {
		amount = row.EstimatedCostInUSDTicks
	}
	if hasReservation {
		if err := settleReservedBilling(tx, reservation, amount); err != nil {
			return err
		}
		result := tx.Where("event_id = ?", row.EventID).Delete(&billingReservationModel{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrConflict
		}
		return nil
	}
	if amount <= 0 {
		return nil
	}
	result := tx.Model(&clientKeyModel{}).Where("id = ?", row.ClientKeyID).UpdateColumn(
		"billed_usage_usd_ticks",
		gorm.Expr("CASE WHEN billed_usage_usd_ticks > ? THEN ? ELSE billed_usage_usd_ticks + ? END", math.MaxInt64-amount, int64(math.MaxInt64), amount),
	)
	if result.Error != nil {
		return result.Error
	}
	return nil
}

func settleReservedBilling(tx *gorm.DB, reservation billingReservationModel, amount int64) error {
	if amount < 0 {
		amount = 0
	}
	result := tx.Model(&clientKeyModel{}).Where("id = ?", reservation.ClientKeyID).Updates(map[string]any{
		"reserved_usage_usd_ticks": gorm.Expr("CASE WHEN reserved_usage_usd_ticks <= ? THEN 0 ELSE reserved_usage_usd_ticks - ? END", reservation.Amount, reservation.Amount),
		"billed_usage_usd_ticks":   gorm.Expr("CASE WHEN billed_usage_usd_ticks > ? THEN ? ELSE billed_usage_usd_ticks + ? END", math.MaxInt64-amount, int64(math.MaxInt64), amount),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func (r *AuditRepository) SumTokensByAccountsSince(ctx context.Context, accountIDs []uint64, since time.Time) (map[uint64]int64, error) {
	result := make(map[uint64]int64, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []struct {
		AccountID   uint64
		TotalTokens int64
	}
	err := r.db.db.WithContext(ctx).
		Model(&requestAuditModel{}).
		Select("account_id, COALESCE(SUM(total_tokens), 0) AS total_tokens").
		Where("account_id IN ? AND created_at >= ? AND total_tokens > 0", accountIDs, since).
		Group("account_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = row.TotalTokens
	}
	return result, nil
}

func (r *AuditRepository) List(ctx context.Context, offset, limit int) ([]audit.Record, int64, error) {
	var total int64
	query := r.db.db.WithContext(ctx).Model(&requestAuditModel{})
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []requestAuditModel
	if err := query.Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]audit.Record, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAuditDomain(row))
	}
	return out, total, nil
}

// ListCursor 使用“排序值 + ID”复合游标读取审计，避免深分页和同值记录漏读。
func (r *AuditRepository) ListCursor(ctx context.Context, input repository.AuditCursorQuery) ([]audit.Record, bool, error) {
	query := r.db.db.WithContext(ctx).Model(&requestAuditModel{})
	query = applyAuditQuery(query, input.Search, input.Start, input.End, input.Filter)
	fields := map[string]sortSpec{
		"request":   {expression: "request_audits.request_id"},
		"model":     {expression: "LOWER(request_audits.model_public_id)"},
		"billing":   {expression: "CASE WHEN request_audits.cost_in_usd_ticks > 0 THEN request_audits.cost_in_usd_ticks ELSE request_audits.estimated_cost_in_usd_ticks END", defaultDirection: repository.SortDescending},
		"tokens":    {expression: "request_audits.total_tokens", defaultDirection: repository.SortDescending},
		"status":    {expression: "request_audits.status_code"},
		"mode":      {expression: "CASE WHEN request_audits.streaming = TRUE THEN 1 ELSE 0 END"},
		"duration":  {expression: "request_audits.duration_ms", defaultDirection: repository.SortDescending},
		"createdAt": {expression: "request_audits.created_at", defaultDirection: repository.SortDescending},
	}
	fallback := sortSpec{expression: "request_audits.created_at", defaultDirection: repository.SortDescending}
	spec, direction := stableSortSpec(input.Sort, fields, fallback)
	if input.Cursor != nil {
		comparison := ">"
		if direction == "DESC" {
			comparison = "<"
		}
		query = query.Where("("+spec.expression+" "+comparison+" ? OR ("+spec.expression+" = ? AND request_audits.id "+comparison+" ?))", input.Cursor.Value, input.Cursor.Value, input.Cursor.ID)
	}
	var rows []requestAuditModel
	query = applyStableSort(query, input.Sort, fields, fallback, "request_audits.id")
	if err := query.Limit(input.Limit + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > input.Limit
	if hasMore {
		rows = rows[:input.Limit]
	}
	out := make([]audit.Record, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAuditDomain(row))
	}
	return out, hasMore, nil
}

func (r *AuditRepository) Summarize(ctx context.Context, input repository.AuditSummaryQuery) (audit.Summary, error) {
	var aggregate struct {
		Requests                int64
		SuccessfulRequests      int64
		FailedRequests          int64
		InputTokens             int64
		CachedInputTokens       int64
		OutputTokens            int64
		ReasoningTokens         int64
		TotalTokens             int64
		DurationMS              int64
		EstimatedCostInUSDTicks int64
		PricedRequests          int64
		UnpricedRequests        int64
		PricedTokens            int64
		UnpricedTokens          int64
	}
	query := applyAuditQuery(r.db.db.WithContext(ctx).Model(&requestAuditModel{}), input.Search, input.Start, input.End, input.Filter)
	if err := query.Select(`
		COUNT(*) AS requests,
		COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0) AS successful_requests,
		COALESCE(SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END), 0) AS failed_requests,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens,
		COALESCE(SUM(total_tokens), 0) AS total_tokens,
		COALESCE(SUM(duration_ms), 0) AS duration_ms,
		COALESCE(SUM(estimated_cost_in_usd_ticks), 0) AS estimated_cost_in_usd_ticks,
		COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') <> '' THEN 1 ELSE 0 END), 0) AS priced_requests,
		COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') = '' THEN 1 ELSE 0 END), 0) AS unpriced_requests,
		COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') <> '' THEN total_tokens ELSE 0 END), 0) AS priced_tokens,
		COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') = '' THEN total_tokens ELSE 0 END), 0) AS unpriced_tokens`).Scan(&aggregate).Error; err != nil {
		return audit.Summary{}, err
	}
	result := audit.Summary{
		Requests: aggregate.Requests, SuccessfulRequests: aggregate.SuccessfulRequests, FailedRequests: aggregate.FailedRequests,
		InputTokens: aggregate.InputTokens, CachedInputTokens: aggregate.CachedInputTokens, OutputTokens: aggregate.OutputTokens,
		ReasoningTokens: aggregate.ReasoningTokens, TotalTokens: aggregate.TotalTokens, DurationMS: aggregate.DurationMS,
		EstimatedCostInUSDTicks: aggregate.EstimatedCostInUSDTicks, PricedRequests: aggregate.PricedRequests,
		UnpricedRequests: aggregate.UnpricedRequests, PricedTokens: aggregate.PricedTokens, UnpricedTokens: aggregate.UnpricedTokens,
	}
	return result, nil
}

func applyAuditQuery(query *gorm.DB, search string, start, end time.Time, filter repository.AuditListFilter) *gorm.DB {
	if value := strings.TrimSpace(search); value != "" {
		pattern := "%" + strings.ToLower(value) + "%"
		query = query.Where("LOWER(request_id) LIKE ? OR LOWER(model_public_id) LIKE ? OR LOWER(model_upstream_model) LIKE ?", pattern, pattern, pattern)
	}
	if !start.IsZero() {
		query = query.Where("created_at >= ?", start)
	}
	if !end.IsZero() {
		query = query.Where("created_at < ?", end)
	}
	if value := strings.TrimSpace(filter.Model); value != "" {
		query = query.Where("model_public_id = ? OR model_upstream_model = ?", value, value)
	}
	if value := strings.TrimSpace(filter.Key); value != "" {
		pattern := "%" + strings.ToLower(value) + "%"
		query = query.Where("LOWER(client_key_name) LIKE ? OR CAST(client_key_id AS TEXT) LIKE ?", pattern, pattern)
	}
	if value := strings.TrimSpace(filter.Account); value != "" {
		pattern := "%" + strings.ToLower(value) + "%"
		query = query.Where("LOWER(account_name) LIKE ? OR CAST(account_id AS TEXT) LIKE ?", pattern, pattern)
	}
	switch filter.Status {
	case "success", "2xx":
		query = query.Where("status_code >= 200 AND status_code < 300")
	case "clientError", "4xx":
		query = query.Where("status_code >= 400 AND status_code < 500")
	case "serverError", "5xx":
		query = query.Where("status_code >= 500 AND status_code < 600")
	}
	switch filter.Mode {
	case "stream":
		query = query.Where("streaming = ?", true)
	case "nonStream":
		query = query.Where("streaming = ?", false)
	}
	return query
}
