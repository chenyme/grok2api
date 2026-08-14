package relational

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AuditRepository struct{ db *Database }

func NewAuditRepository(db *Database) *AuditRepository { return &AuditRepository{db: db} }

const (
	auditInsertBatchSize   = 20
	auditLookupBatchSize   = 500
	attemptInsertBatchSize = 40
	auditSuccessPredicate  = "status_code >= 200 AND status_code < 300 AND (error_code IS NULL OR error_code = '')"
	auditSuccessAggregate  = "COALESCE(SUM(CASE WHEN " + auditSuccessPredicate + " THEN 1 ELSE 0 END), 0)"
)

var errAuditBatchRequiresFallback = errors.New("audit batch requires idempotent fallback")

type preparedAudit struct {
	row      requestAuditModel
	attempts []requestAuditAttemptModel
}

func (r *AuditRepository) Create(ctx context.Context, value audit.Record) error {
	prepared, err := prepareAudits([]audit.Record{value})
	if err != nil {
		return err
	}
	row := prepared[0].row
	attempts := prepared[0].attempts
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return createAuditAndBill(tx, &row, attempts)
	})
}

func (r *AuditRepository) CreateBatch(ctx context.Context, values []audit.Record) error {
	if len(values) == 0 {
		return nil
	}
	prepared, err := prepareAudits(values)
	if err != nil {
		return err
	}
	err = r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return createPreparedAuditBatchFast(tx, prepared)
	})
	if !errors.Is(err, errAuditBatchRequiresFallback) {
		return err
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return createPreparedAuditBatchSafe(tx, prepared)
	})
}

func prepareAudits(values []audit.Record) ([]preparedAudit, error) {
	prepared := make([]preparedAudit, 0, len(values))
	for index, value := range values {
		row, attempts, err := toAuditModels(value)
		if err != nil {
			return nil, &repository.InvalidBatchRecordError{Index: index, Err: err}
		}
		candidate := preparedAudit{row: row, attempts: attempts}
		if err := validatePreparedAudit(candidate); err != nil {
			return nil, &repository.InvalidBatchRecordError{Index: index, Err: err}
		}
		prepared = append(prepared, candidate)
	}
	return prepared, nil
}

func validatePreparedAudit(value preparedAudit) error {
	row := value.row
	if length := utf8.RuneCountInString(row.EventID); length < 16 || length > 64 {
		return errors.New("event_id length must be between 16 and 64")
	}
	if length := utf8.RuneCountInString(row.RequestID); length < 1 || length > 64 {
		return errors.New("request_id length must be between 1 and 64")
	}
	if row.ClientKeyID == 0 {
		return errors.New("client_key_id must be positive")
	}
	if row.ModelRouteID == 0 {
		return errors.New("model_route_id must be positive")
	}
	if !auditStringAllowed(row.Provider, "grok_build", "grok_web", "grok_console") {
		return errors.New("provider is invalid")
	}
	if !auditStringAllowed(row.Operation, "responses", "compaction", "chat", "messages", "image", "image_edit", "video", "tts", "stt", "realtime", "voice") {
		return errors.New("operation is invalid")
	}
	if !auditStringAllowed(row.UsageSource, "upstream", "estimated", "none") {
		return errors.New("usage_source is invalid")
	}
	if row.AccountID != nil && *row.AccountID == 0 {
		return errors.New("account_id must be positive when present")
	}
	if row.EgressNodeID != nil && *row.EgressNodeID == 0 {
		return errors.New("egress_node_id must be positive when present")
	}
	if !auditStringAllowed(row.EgressScope, "", "grok_build", "grok_web", "grok_console", "grok_web_asset", "grok_console_asset") {
		return errors.New("egress_scope is invalid")
	}
	if !auditStringAllowed(row.EgressMode, "", "direct", "proxy") {
		return errors.New("egress_mode is invalid")
	}
	if row.StatusCode < 100 || row.StatusCode > 599 {
		return errors.New("status_code must be between 100 and 599")
	}
	attemptNumbers := make(map[int]struct{}, len(value.attempts))
	for index, attempt := range value.attempts {
		if err := validatePreparedAuditAttempt(attempt); err != nil {
			return fmt.Errorf("attempt %d: %w", index+1, err)
		}
		if _, exists := attemptNumbers[attempt.Number]; exists {
			return fmt.Errorf("attempt %d: number %d is duplicated", index+1, attempt.Number)
		}
		attemptNumbers[attempt.Number] = struct{}{}
	}
	return nil
}

func validatePreparedAuditAttempt(value requestAuditAttemptModel) error {
	if value.Number <= 0 {
		return errors.New("number must be positive")
	}
	if !auditStringAllowed(value.Source, "upstream_http", "gateway_transport", "credential") {
		return errors.New("source is invalid")
	}
	if length := utf8.RuneCountInString(strings.TrimSpace(value.Stage)); length < 1 || length > 64 {
		return errors.New("stage length must be between 1 and 64")
	}
	if value.AccountID != nil && *value.AccountID == 0 {
		return errors.New("account_id must be positive when present")
	}
	if value.UpstreamStatusCode != nil && (*value.UpstreamStatusCode < 100 || *value.UpstreamStatusCode > 599) {
		return errors.New("upstream_status_code must be between 100 and 599 when present")
	}
	if utf8.RuneCountInString(value.ResponseHeadersJSON) > 32768 {
		return errors.New("response headers exceed the storage limit")
	}
	if len(value.ResponseBody) > 65536 {
		return errors.New("response body exceeds the storage limit")
	}
	if utf8.RuneCountInString(value.ErrorChainJSON) > 32768 {
		return errors.New("error chain exceeds the storage limit")
	}
	return nil
}

func auditStringAllowed(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func createPreparedAuditBatchFast(tx *gorm.DB, prepared []preparedAudit) error {
	rows := make([]requestAuditModel, len(prepared))
	for index := range prepared {
		rows[index] = prepared[index].row
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, auditInsertBatchSize)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}
	// A mixed new/duplicate result cannot be mapped safely across database RETURNING implementations.
	if result.RowsAffected != int64(len(rows)) {
		return errAuditBatchRequiresFallback
	}
	idsByEvent, err := loadInsertedAuditIDs(tx, prepared)
	if err != nil {
		return err
	}
	inserted := make([]preparedAudit, len(prepared))
	for index := range prepared {
		auditID := idsByEvent[prepared[index].row.EventID]
		if auditID == 0 {
			return errAuditBatchRequiresFallback
		}
		inserted[index] = prepared[index]
		inserted[index].row.ID = auditID
	}
	if err := insertPreparedAuditAttempts(tx, inserted); err != nil {
		return err
	}
	return settleInsertedAudits(tx, inserted)
}

func loadInsertedAuditIDs(tx *gorm.DB, prepared []preparedAudit) (map[string]uint64, error) {
	eventIDs := make([]string, len(prepared))
	for index := range prepared {
		eventIDs[index] = prepared[index].row.EventID
	}
	idsByEvent := make(map[string]uint64, len(eventIDs))
	for start := 0; start < len(eventIDs); start += auditLookupBatchSize {
		end := min(start+auditLookupBatchSize, len(eventIDs))
		var rows []struct {
			ID      uint64
			EventID string
		}
		if err := tx.Model(&requestAuditModel{}).Select("id", "event_id").Where("event_id IN ?", eventIDs[start:end]).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			idsByEvent[row.EventID] = row.ID
		}
	}
	return idsByEvent, nil
}

func createPreparedAuditBatchSafe(tx *gorm.DB, prepared []preparedAudit) error {
	inserted := make([]preparedAudit, 0, len(prepared))
	for index := range prepared {
		row := prepared[index].row
		row.ID = 0
		wasInserted, err := insertAudit(tx, &row)
		if err != nil {
			return err
		}
		if !wasInserted {
			continue
		}
		attempts := append([]requestAuditAttemptModel(nil), prepared[index].attempts...)
		if err := insertAuditAttempts(tx, row.ID, attempts); err != nil {
			return err
		}
		inserted = append(inserted, preparedAudit{row: row})
	}
	return settleInsertedAudits(tx, inserted)
}

func insertPreparedAuditAttempts(tx *gorm.DB, inserted []preparedAudit) error {
	attemptCount := 0
	for _, value := range inserted {
		attemptCount += len(value.attempts)
	}
	if attemptCount == 0 {
		return nil
	}
	attempts := make([]requestAuditAttemptModel, 0, attemptCount)
	for _, value := range inserted {
		for _, attempt := range value.attempts {
			attempt.AuditID = value.row.ID
			attempts = append(attempts, attempt)
		}
	}
	return tx.CreateInBatches(&attempts, attemptInsertBatchSize).Error
}

func settleInsertedAudits(tx *gorm.DB, inserted []preparedAudit) error {
	if len(inserted) == 0 {
		return nil
	}
	keySet := make(map[uint64]struct{}, len(inserted))
	for _, value := range inserted {
		keySet[value.row.ClientKeyID] = struct{}{}
	}
	keyIDs := make([]uint64, 0, len(keySet))
	for keyID := range keySet {
		keyIDs = append(keyIDs, keyID)
	}
	sort.Slice(keyIDs, func(i, j int) bool { return keyIDs[i] < keyIDs[j] })
	// Lock every referenced key before reading reservations so zero-cost settlements cannot race reservation creation.
	missingKeys := make(map[uint64]struct{})
	for _, keyID := range keyIDs {
		if err := lockClientKey(tx, keyID); err != nil {
			if !errors.Is(err, repository.ErrNotFound) {
				return err
			}
			missingKeys[keyID] = struct{}{}
		}
	}

	eventIDs := make([]string, 0, len(inserted))
	for _, value := range inserted {
		eventIDs = append(eventIDs, value.row.EventID)
	}
	var reservations []billingReservationModel
	if err := tx.Where("event_id IN ?", eventIDs).Find(&reservations).Error; err != nil {
		return err
	}
	reservationByEvent := make(map[string]billingReservationModel, len(reservations))
	for _, reservation := range reservations {
		if _, missing := missingKeys[reservation.ClientKeyID]; missing {
			return repository.ErrNotFound
		}
		reservationByEvent[reservation.EventID] = reservation
	}

	sort.Slice(inserted, func(i, j int) bool {
		if inserted[i].row.ClientKeyID == inserted[j].row.ClientKeyID {
			return inserted[i].row.EventID < inserted[j].row.EventID
		}
		return inserted[i].row.ClientKeyID < inserted[j].row.ClientKeyID
	})
	settledEventIDs := make([]string, 0, len(reservations))
	for _, value := range inserted {
		reservation, hasReservation := reservationByEvent[value.row.EventID]
		settled, err := applyInsertedAuditBilling(tx, value.row, reservation, hasReservation)
		if err != nil {
			return err
		}
		if settled {
			settledEventIDs = append(settledEventIDs, value.row.EventID)
		}
	}
	return deleteBillingReservations(tx, settledEventIDs)
}

func toAuditModels(value audit.Record) (requestAuditModel, []requestAuditAttemptModel, error) {
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
	row := requestAuditModel{
		EventID: truncate(eventID, 64), RequestID: truncate(value.RequestID, 64), ClientKeyID: value.ClientKeyID, ClientKeyName: truncate(value.ClientKeyName, 160),
		ModelRouteID: value.ModelRouteID, ModelPublicID: truncate(value.ModelPublicID, 255), ModelUpstreamModel: truncate(value.ModelUpstreamModel, 255),
		Provider: truncate(provider, 32), Operation: string(operation), UsageSource: string(usageSource),
		AccountID: value.AccountID, AccountName: truncate(value.AccountName, 160),
		EgressNodeID: value.EgressNodeID, EgressNodeName: truncate(value.EgressNodeName, 160), EgressScope: truncate(value.EgressScope, 32), EgressMode: string(value.EgressMode),
		StatusCode: value.StatusCode, Streaming: value.Streaming,
		MediaInputImages: nonNegative(value.MediaInputImages), MediaOutputImages: nonNegative(value.MediaOutputImages), MediaOutputSeconds: nonNegative(value.MediaOutputSeconds),
		InputTokens: nonNegative(value.InputTokens), CachedInputTokens: nonNegative(value.CachedInputTokens), OutputTokens: nonNegative(value.OutputTokens),
		ReasoningTokens: nonNegative(value.ReasoningTokens), TotalTokens: nonNegative(value.TotalTokens), CostInUSDTicks: nonNegative(value.CostInUSDTicks),
		EstimatedCostInUSDTicks: nonNegative(value.EstimatedCostInUSDTicks), PricingModel: truncate(value.PricingModel, 100), PricingVersion: truncate(value.PricingVersion, 20),
		NumSourcesUsed: nonNegative(value.NumSourcesUsed), NumServerSideToolsUsed: nonNegative(value.NumServerSideToolsUsed),
		ContextInputTokens: nonNegative(value.ContextInputTokens), ContextOutputTokens: nonNegative(value.ContextOutputTokens), FirstTokenMS: normalizedFirstToken(value), DurationMS: nonNegative(value.DurationMS),
		ErrorCode: truncate(value.ErrorCode, 100), AttemptCount: len(value.Attempts), CreatedAt: value.CreatedAt,
	}
	attempts := make([]requestAuditAttemptModel, 0, len(value.Attempts))
	for _, attempt := range value.Attempts {
		responseHeaders, err := json.Marshal(attempt.ResponseHeaders)
		if err != nil {
			return requestAuditModel{}, nil, fmt.Errorf("序列化审计响应头: %w", err)
		}
		if attempt.ResponseHeaders == nil {
			responseHeaders = []byte("{}")
		}
		errorChain, err := json.Marshal(attempt.ErrorChain)
		if err != nil {
			return requestAuditModel{}, nil, fmt.Errorf("序列化审计错误链: %w", err)
		}
		if attempt.ErrorChain == nil {
			errorChain = []byte("[]")
		}
		attempts = append(attempts, requestAuditAttemptModel{
			Number:                attempt.Number,
			Source:                string(attempt.Source),
			Stage:                 attempt.Stage,
			AccountID:             attempt.AccountID,
			AccountName:           truncate(attempt.AccountName, 160),
			Method:                truncate(attempt.Method, 16),
			RequestPath:           truncate(attempt.RequestPath, 2048),
			UpstreamURL:           truncate(attempt.UpstreamURL, 4096),
			StartedAt:             attempt.StartedAt,
			DurationMS:            nonNegative(attempt.DurationMS),
			UpstreamStatusCode:    attempt.UpstreamStatusCode,
			UpstreamStatus:        truncate(attempt.UpstreamStatus, 128),
			ResponseHeadersJSON:   string(responseHeaders),
			ResponseBody:          truncateBytes(attempt.ResponseBody, 65536),
			ResponseBodyTruncated: attempt.ResponseBodyTruncated || len(attempt.ResponseBody) > 65536,
			TransportError:        truncate(attempt.TransportError, 2048),
			ErrorChainJSON:        string(errorChain),
		})
	}
	return row, attempts, nil
}

func truncateBytes(value []byte, limit int) []byte {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func createAuditAndBill(tx *gorm.DB, row *requestAuditModel, attempts []requestAuditAttemptModel) error {
	inserted, err := insertAudit(tx, row)
	if err != nil || !inserted {
		return err
	}
	if err := insertAuditAttempts(tx, row.ID, attempts); err != nil {
		return err
	}
	var reservation billingReservationModel
	reservationErr := tx.Where("event_id = ?", row.EventID).First(&reservation).Error
	if reservationErr != nil && !errors.Is(reservationErr, gorm.ErrRecordNotFound) {
		return reservationErr
	}
	if reservationErr == nil || auditBillingAmount(*row) > 0 {
		if err := lockClientKey(tx, row.ClientKeyID); err != nil {
			if reservationErr == nil || !errors.Is(err, repository.ErrNotFound) {
				return err
			}
			return billInsertedAudit(tx, *row, billingReservationModel{}, false)
		}
		reservation = billingReservationModel{}
		reservationErr = tx.Where("event_id = ?", row.EventID).First(&reservation).Error
		if reservationErr != nil && !errors.Is(reservationErr, gorm.ErrRecordNotFound) {
			return reservationErr
		}
	}
	return billInsertedAudit(tx, *row, reservation, reservationErr == nil)
}

func insertAudit(tx *gorm.DB, row *requestAuditModel) (bool, error) {
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	return result.RowsAffected == 1, result.Error
}

func insertAuditAttempts(tx *gorm.DB, auditID uint64, attempts []requestAuditAttemptModel) error {
	if len(attempts) == 0 {
		return nil
	}
	for index := range attempts {
		attempts[index].AuditID = auditID
	}
	return tx.Create(&attempts).Error
}

func billInsertedAudit(tx *gorm.DB, row requestAuditModel, reservation billingReservationModel, hasReservation bool) error {
	settled, err := applyInsertedAuditBilling(tx, row, reservation, hasReservation)
	if err != nil {
		return err
	}
	if !settled {
		return nil
	}
	return deleteBillingReservations(tx, []string{row.EventID})
}

func applyInsertedAuditBilling(tx *gorm.DB, row requestAuditModel, reservation billingReservationModel, hasReservation bool) (bool, error) {
	amount := auditBillingAmount(row)
	if hasReservation {
		if err := settleReservedBilling(tx, reservation, amount); err != nil {
			return false, err
		}
		return true, nil
	}
	if amount <= 0 {
		return false, nil
	}
	result := tx.Model(&clientKeyModel{}).Where("id = ?", row.ClientKeyID).UpdateColumn(
		"billed_usage_usd_ticks",
		gorm.Expr("CASE WHEN billed_usage_usd_ticks > ? THEN ? ELSE billed_usage_usd_ticks + ? END", math.MaxInt64-amount, int64(math.MaxInt64), amount),
	)
	if result.Error != nil {
		return false, result.Error
	}
	return false, nil
}

func deleteBillingReservations(tx *gorm.DB, eventIDs []string) error {
	if len(eventIDs) == 0 {
		return nil
	}
	result := tx.Where("event_id IN ?", eventIDs).Delete(&billingReservationModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != int64(len(eventIDs)) {
		return repository.ErrConflict
	}
	return nil
}

func auditBillingAmount(row requestAuditModel) int64 {
	if row.CostInUSDTicks > 0 {
		return row.CostInUSDTicks
	}
	return row.EstimatedCostInUSDTicks
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

func normalizedFirstToken(value audit.Record) *int64 {
	if value.FirstTokenMS == nil || !value.Streaming || value.StatusCode < 200 || value.StatusCode >= 300 || value.ErrorCode != "" {
		return nil
	}
	normalized := nonNegative(*value.FirstTokenMS)
	if normalized > nonNegative(value.DurationMS) {
		return nil
	}
	return &normalized
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

func (r *AuditRepository) Get(ctx context.Context, id uint64) (audit.Record, error) {
	var row requestAuditModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return audit.Record{}, mapError(err)
	}
	var attemptRows []requestAuditAttemptModel
	if err := r.db.db.WithContext(ctx).Where("audit_id = ?", id).Order("number ASC").Find(&attemptRows).Error; err != nil {
		return audit.Record{}, err
	}
	value := toAuditDomain(row)
	value.Attempts = make([]audit.Attempt, 0, len(attemptRows))
	for _, attemptRow := range attemptRows {
		attempt, err := toAuditAttemptDomain(attemptRow)
		if err != nil {
			return audit.Record{}, err
		}
		value.Attempts = append(value.Attempts, attempt)
	}
	return value, nil
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
		` + auditSuccessAggregate + ` AS successful_requests,
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
		Requests: aggregate.Requests, SuccessfulRequests: aggregate.SuccessfulRequests, FailedRequests: aggregate.Requests - aggregate.SuccessfulRequests,
		InputTokens: aggregate.InputTokens, CachedInputTokens: aggregate.CachedInputTokens, OutputTokens: aggregate.OutputTokens,
		ReasoningTokens: aggregate.ReasoningTokens, TotalTokens: aggregate.TotalTokens, DurationMS: aggregate.DurationMS,
		EstimatedCostInUSDTicks: aggregate.EstimatedCostInUSDTicks, PricedRequests: aggregate.PricedRequests,
		UnpricedRequests: aggregate.UnpricedRequests, PricedTokens: aggregate.PricedTokens, UnpricedTokens: aggregate.UnpricedTokens,
	}
	return result, nil
}

type degradeTotalsRow struct {
	Hits         int64   `gorm:"column:hits"`
	Accounts     int64   `gorm:"column:accounts"`
	StillEnabled int64   `gorm:"column:still_enabled"`
	Disabled     int64   `gorm:"column:disabled"`
	Deleted      int64   `gorm:"column:deleted"`
	Hard         int64   `gorm:"column:hard"`
	Soft         int64   `gorm:"column:soft"`
	Burst        int64   `gorm:"column:burst"`
	MaxTPS       float64 `gorm:"column:max_tps"`
}

type degradeAccountRow struct {
	ID                 uint64           `gorm:"column:id"`
	Name               string           `gorm:"column:name"`
	Email              string           `gorm:"column:email"`
	Hits               int64            `gorm:"column:hits"`
	MaxTPS             float64          `gorm:"column:max_tps"`
	Burst              int64            `gorm:"column:burst"`
	Soft               int64            `gorm:"column:soft"`
	Hard               int64            `gorm:"column:hard"`
	Last               degradeTimestamp `gorm:"column:last_seen"`
	Enabled            bool             `gorm:"column:enabled"`
	Found              bool             `gorm:"column:found"`
	BuildBotFlagSource int              `gorm:"column:build_bot_flag_source"`
}

type degradeTimestamp time.Time

func (value *degradeTimestamp) Scan(input any) error {
	parsed, err := parseDegradeTimestamp(input)
	if err != nil {
		return err
	}
	*value = degradeTimestamp(parsed)
	return nil
}

func parseDegradeTimestamp(input any) (time.Time, error) {
	if parsed, ok := input.(time.Time); ok {
		return parsed, nil
	}
	var raw string
	switch value := input.(type) {
	case string:
		raw = value
	case []byte:
		raw = string(value)
	case nil:
		return time.Time{}, nil
	default:
		return time.Time{}, fmt.Errorf("unsupported degrade timestamp type %T", input)
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid degrade timestamp")
}

type degradeNodeRow struct {
	Name     string  `gorm:"column:name"`
	Hits     int64   `gorm:"column:hits"`
	Accounts int64   `gorm:"column:accounts"`
	MaxTPS   float64 `gorm:"column:max_tps"`
}

type degradeBucketRow struct {
	Index  int   `gorm:"column:bucket_index"`
	Count  int64 `gorm:"column:count"`
	Severe int64 `gorm:"column:severe"`
}

type degradeAccountNodeRow struct {
	AccountID uint64 `gorm:"column:account_id"`
	Name      string `gorm:"column:name"`
}

type degradeEventRow struct {
	ID             uint64    `gorm:"column:id"`
	RequestID      string    `gorm:"column:request_id"`
	AccountID      *uint64   `gorm:"column:account_id"`
	AccountName    string    `gorm:"column:account_name"`
	EgressNodeID   *uint64   `gorm:"column:egress_node_id"`
	EgressNodeName string    `gorm:"column:egress_node_name"`
	OutputTokens   int64     `gorm:"column:output_tokens"`
	TPS            float64   `gorm:"column:tps"`
	Class          string    `gorm:"column:degrade_class"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	Model          string    `gorm:"column:model_upstream_model"`
}

func (r *AuditRepository) SummarizeDegrade(ctx context.Context, input repository.DegradeSummaryQuery) (repository.DegradeSummaryResult, error) {
	if input.AccountLimit <= 0 || input.AccountLimit > 100 {
		input.AccountLimit = 50
	}
	if input.AccountOffset < 0 {
		input.AccountOffset = 0
	}
	if input.RecentLimit <= 0 || input.RecentLimit > 200 {
		input.RecentLimit = 80
	}
	var result repository.DegradeSummaryResult
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		classified := r.degradeClassifiedQuery(tx, input)
		var totals degradeTotalsRow
		if err := tx.Table("(?) AS d", classified).
			Select(`COUNT(*) AS hits,
				COUNT(DISTINCT d.account_id) AS accounts,
				COUNT(DISTINCT CASE WHEN p.id IS NOT NULL AND p.enabled = TRUE THEN d.account_id END) AS still_enabled,
				COUNT(DISTINCT CASE WHEN p.id IS NOT NULL AND p.enabled = FALSE THEN d.account_id END) AS disabled,
				COUNT(DISTINCT CASE WHEN p.id IS NULL THEN d.account_id END) AS deleted,
				COALESCE(SUM(CASE WHEN d.degrade_class = 'hard_tps' THEN 1 ELSE 0 END), 0) AS hard,
				COALESCE(SUM(CASE WHEN d.degrade_class = 'soft_tps' THEN 1 ELSE 0 END), 0) AS soft,
				COALESCE(SUM(CASE WHEN d.degrade_class = 'buffered_burst' THEN 1 ELSE 0 END), 0) AS burst,
				COALESCE(MAX(d.tps), 0) AS max_tps`).
			Joins("LEFT JOIN provider_accounts p ON p.id = d.account_id").
			Scan(&totals).Error; err != nil {
			return err
		}
		result.Totals = repository.DegradeTotals{
			Hits: totals.Hits, Accounts: totals.Accounts, StillEnabled: totals.StillEnabled,
			Disabled: totals.Disabled, Deleted: totals.Deleted, Hard: totals.Hard, Soft: totals.Soft,
			Burst: totals.Burst, MaxTPS: totals.MaxTPS,
		}

		accountQuery := r.degradeAccountAggregate(tx, classified, input)
		if err := tx.Table("(?) AS filtered_accounts", accountQuery).Count(&result.AccountTotal).Error; err != nil {
			return err
		}
		accountOffset := input.AccountOffset
		if result.AccountTotal > 0 && int64(accountOffset) >= result.AccountTotal {
			accountOffset = int((result.AccountTotal-1)/int64(input.AccountLimit)) * input.AccountLimit
		}
		result.AccountOffset = accountOffset
		var accountRows []degradeAccountRow
		if err := accountQuery.Order("hits DESC, max_tps DESC, id ASC").
			Offset(accountOffset).Limit(input.AccountLimit).Scan(&accountRows).Error; err != nil {
			return err
		}
		accountIDs := make([]uint64, 0, len(accountRows))
		accountsByID := make(map[uint64]*repository.DegradeAccount, len(accountRows))
		result.Accounts = make([]repository.DegradeAccount, 0, len(accountRows))
		for _, row := range accountRows {
			result.Accounts = append(result.Accounts, repository.DegradeAccount{
				ID: row.ID, Name: row.Name, Email: row.Email, Hits: row.Hits, MaxTPS: row.MaxTPS,
				Burst: row.Burst, Soft: row.Soft, Hard: row.Hard, Last: time.Time(row.Last), Enabled: row.Enabled,
				Found: row.Found, BuildBotFlagSource: row.BuildBotFlagSource,
			})
			accountIDs = append(accountIDs, row.ID)
			accountsByID[row.ID] = &result.Accounts[len(result.Accounts)-1]
		}
		if len(accountIDs) > 0 {
			var nodeRows []degradeAccountNodeRow
			if err := tx.Table("(?) AS d", classified).
				Select("d.account_id, COALESCE(NULLIF(d.egress_node_name, ''), '?') AS name").
				Where("d.account_id IN ?", accountIDs).
				Group("d.account_id, COALESCE(NULLIF(d.egress_node_name, ''), '?')").
				Order("d.account_id ASC, name ASC").Scan(&nodeRows).Error; err != nil {
				return err
			}
			for _, row := range nodeRows {
				if account := accountsByID[row.AccountID]; account != nil {
					account.Nodes = append(account.Nodes, row.Name)
				}
			}
		}

		var nodeRows []degradeNodeRow
		if err := tx.Table("(?) AS d", classified).
			Select("COALESCE(NULLIF(d.egress_node_name, ''), '?') AS name, COUNT(*) AS hits, COUNT(DISTINCT d.account_id) AS accounts, COALESCE(MAX(d.tps), 0) AS max_tps").
			Group("COALESCE(NULLIF(d.egress_node_name, ''), '?')").Order("hits DESC, name ASC").Scan(&nodeRows).Error; err != nil {
			return err
		}
		result.Nodes = make([]repository.DegradeNode, 0, len(nodeRows))
		for _, row := range nodeRows {
			result.Nodes = append(result.Nodes, repository.DegradeNode{Name: row.Name, Hits: row.Hits, Accounts: row.Accounts, MaxTPS: row.MaxTPS})
		}

		if len(input.Buckets) > 0 {
			caseSQL, caseArgs := degradeBucketCase(input.Buckets)
			bucketed := tx.Table("(?) AS d", classified).Select(caseSQL+" AS bucket_index, d.degrade_class", caseArgs...)
			var bucketRows []degradeBucketRow
			if err := tx.Table("(?) AS b", bucketed).
				Select("b.bucket_index, COUNT(*) AS count, COALESCE(SUM(CASE WHEN b.degrade_class IN ('hard_tps', 'buffered_burst') THEN 1 ELSE 0 END), 0) AS severe").
				Where("b.bucket_index >= 0").Group("b.bucket_index").Order("b.bucket_index ASC").Scan(&bucketRows).Error; err != nil {
				return err
			}
			result.Buckets = make([]repository.DegradeBucket, 0, len(bucketRows))
			for _, row := range bucketRows {
				result.Buckets = append(result.Buckets, repository.DegradeBucket{Index: row.Index, Count: row.Count, Severe: row.Severe})
			}
		}

		var eventRows []degradeEventRow
		if err := tx.Table("(?) AS d", classified).
			Select("d.id, d.request_id, d.account_id, d.account_name, d.egress_node_id, d.egress_node_name, d.output_tokens, d.tps, d.degrade_class, d.created_at, d.model_upstream_model").
			Order("d.created_at DESC, d.id DESC").Limit(input.RecentLimit).Scan(&eventRows).Error; err != nil {
			return err
		}
		result.Events = make([]repository.DegradeEvent, 0, len(eventRows))
		for _, row := range eventRows {
			result.Events = append(result.Events, repository.DegradeEvent{
				ID: row.ID, RequestID: row.RequestID, AccountID: row.AccountID, AccountName: row.AccountName,
				EgressNodeID: row.EgressNodeID, EgressNodeName: row.EgressNodeName, OutputTokens: row.OutputTokens,
				TPS: row.TPS, Class: row.Class, CreatedAt: row.CreatedAt, Model: row.Model,
			})
		}
		return nil
	})
	return result, err
}

func (r *AuditRepository) degradeClassifiedQuery(tx *gorm.DB, input repository.DegradeSummaryQuery) *gorm.DB {
	castType := "REAL"
	if r.db.dialect == "postgres" {
		castType = "DOUBLE PRECISION"
	}
	generationExpression := "(a.duration_ms - a.first_token_ms)"
	// NULLIF keeps PostgreSQL safe even if its planner evaluates the throughput
	// expression before the duration guard in the WHERE clause.
	tpsExpression := fmt.Sprintf("(CAST(a.output_tokens AS %s) * 1000.0 / NULLIF(%s, 0))", castType, generationExpression)
	classExpression := fmt.Sprintf("CASE WHEN ? AND %s < ? THEN ? WHEN %s >= ? THEN ? ELSE ? END", generationExpression, tpsExpression)
	query := tx.Table("request_audits AS a").
		Select("a.id, a.request_id, a.account_id, a.account_name, a.egress_node_id, a.egress_node_name, a.output_tokens, a.created_at, a.model_upstream_model, "+tpsExpression+" AS tps, "+classExpression+" AS degrade_class",
			input.FailClosed, input.MinGenerationMS, audit.DegradeClassBurst, input.HardTPS, audit.DegradeClassHard, audit.DegradeClassSoft).
		Where("a.provider = ?", "grok_build").Where("a.streaming = ?", true).
		Where(auditSuccessPredicate).Where("a.output_tokens >= ?", input.MinOutputTokens).
		Where("a.first_token_ms IS NOT NULL").Where("a.duration_ms > a.first_token_ms").
		Where("a.request_id NOT LIKE ?", "quality_%").Where(tpsExpression+" >= ?", input.SoftTPS)
	if !input.Start.IsZero() {
		query = query.Where("a.created_at >= ?", input.Start)
	}
	if !input.End.IsZero() {
		query = query.Where("a.created_at < ?", input.End)
	}
	return query
}

func (r *AuditRepository) degradeAccountAggregate(tx *gorm.DB, classified *gorm.DB, input repository.DegradeSummaryQuery) *gorm.DB {
	query := tx.Table("(?) AS d", classified).
		Select(`d.account_id AS id,
			COALESCE(NULLIF(p.name, ''), MAX(d.account_name), '') AS name,
			COALESCE(p.email, '') AS email,
			COUNT(*) AS hits,
			COALESCE(MAX(d.tps), 0) AS max_tps,
			COALESCE(SUM(CASE WHEN d.degrade_class = 'buffered_burst' THEN 1 ELSE 0 END), 0) AS burst,
			COALESCE(SUM(CASE WHEN d.degrade_class = 'soft_tps' THEN 1 ELSE 0 END), 0) AS soft,
			COALESCE(SUM(CASE WHEN d.degrade_class = 'hard_tps' THEN 1 ELSE 0 END), 0) AS hard,
			MAX(d.created_at) AS last_seen,
			COALESCE(p.enabled, FALSE) AS enabled,
			CASE WHEN p.id IS NULL THEN FALSE ELSE TRUE END AS found,
			COALESCE(c.build_bot_flag_source, 0) AS build_bot_flag_source`).
		Joins("LEFT JOIN provider_accounts p ON p.id = d.account_id").
		Joins("LEFT JOIN account_credentials c ON c.account_id = d.account_id").
		Where("d.account_id IS NOT NULL").
		Group("d.account_id, p.id, p.name, p.email, p.enabled, c.build_bot_flag_source")
	if search := strings.TrimSpace(input.AccountSearch); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where(
			"LOWER(COALESCE(p.email, '')) LIKE ? OR LOWER(COALESCE(p.name, '')) LIKE ? OR LOWER(COALESCE(d.account_name, '')) LIKE ? OR CAST(d.account_id AS TEXT) LIKE ?",
			pattern, pattern, pattern, pattern,
		)
	}
	switch input.AccountStatus {
	case "enabled":
		query = query.Where("p.id IS NOT NULL AND p.enabled = TRUE")
	case "disabled":
		query = query.Where("p.id IS NOT NULL AND p.enabled = FALSE")
	case "deleted":
		query = query.Where("p.id IS NULL")
	}
	if input.MinHits > 1 {
		query = query.Having("COUNT(*) >= ?", input.MinHits)
	}
	if input.AccountClass != "" {
		query = query.Having("SUM(CASE WHEN d.degrade_class = ? THEN 1 ELSE 0 END) > 0", input.AccountClass)
	}
	return query
}

func degradeBucketCase(buckets []repository.DegradeBucketRange) (string, []any) {
	var builder strings.Builder
	args := make([]any, 0, len(buckets)*2)
	builder.WriteString("CASE")
	for index, bucket := range buckets {
		builder.WriteString(" WHEN d.created_at >= ? AND d.created_at < ? THEN ")
		builder.WriteString(strconv.Itoa(index))
		args = append(args, bucket.Start, bucket.End)
	}
	builder.WriteString(" ELSE -1 END")
	return builder.String(), args
}

func applyAuditQuery(query *gorm.DB, search string, start, end time.Time, filter repository.AuditListFilter) *gorm.DB {
	if value := strings.TrimSpace(search); value != "" {
		pattern := "%" + strings.ToLower(value) + "%"
		query = query.Where("LOWER(request_id) LIKE ? OR LOWER(model_public_id) LIKE ? OR LOWER(model_upstream_model) LIKE ? OR LOWER(egress_node_name) LIKE ?", pattern, pattern, pattern, pattern)
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
		query = query.Where(auditSuccessPredicate)
	case "clientError", "4xx":
		query = query.Where("status_code >= 400 AND status_code < 500")
	case "serverError", "5xx":
		query = query.Where("status_code >= 500 AND status_code < 600")
	case "other":
		// 保留真实 HTTP 状态：2xx 响应头之后的流失败通过 error_code 识别；
		// 同时覆盖不属于 2xx/4xx/5xx 的状态段。status_code < 100 兼容
		// 曾运行过早期实现并写入 0 的开发数据库，但新记录仍只允许 100..599。
		query = query.Where("(status_code >= 200 AND status_code < 300 AND error_code IS NOT NULL AND error_code <> '') OR status_code < 200 OR (status_code >= 300 AND status_code < 400) OR status_code >= 600")
	}
	switch filter.Mode {
	case "stream":
		query = query.Where("streaming = ?", true)
	case "nonStream":
		query = query.Where("streaming = ?", false)
	}
	return query
}
