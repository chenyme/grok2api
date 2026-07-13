package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrQueueFull     = errors.New("审计写入队列已满")
	ErrInvalidCursor = errors.New("审计游标无效")
	ErrInvalidFilter = errors.New("审计筛选条件无效")
	ErrInvalidPeriod = errors.New("审计时间范围无效")
)

type Period string

const (
	Period24Hours Period = "24h"
	Period7Days   Period = "7d"
	Period30Days  Period = "30d"
	Period90Days  Period = "90d"
)

const (
	auditEnqueueWait   = 25 * time.Millisecond
	auditWriteTimeout  = 2 * time.Second
	auditWriteAttempts = 3
	auditSummaryTTL    = 10 * time.Second
)

// Service 提供请求元数据审计查询，以及有界异步批量写入。
type Service struct {
	audits        repository.AuditRepository
	logger        *slog.Logger
	queue         chan auditdomain.Record
	batchSize     atomic.Int64
	flushInterval atomic.Int64
	configChanged chan struct{}
	startOnce     sync.Once
	stopOnce      sync.Once
	stop          chan struct{}
	done          chan struct{}
	stopped       atomic.Bool
	dropped       atomic.Uint64
	now           func() time.Time
	summaryCache  *resultcache.Cache[string, SummaryResult]
}

func NewService(audits repository.AuditRepository, logger *slog.Logger, bufferSize, batchSize int, flushInterval time.Duration) *Service {
	service := &Service{audits: audits, logger: logger, queue: make(chan auditdomain.Record, bufferSize), configChanged: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), now: time.Now, summaryCache: resultcache.New[string, SummaryResult](64, auditSummaryTTL)}
	service.UpdateConfig(batchSize, flushInterval)
	return service
}

func (s *Service) UpdateConfig(batchSize int, flushInterval time.Duration) {
	s.batchSize.Store(int64(batchSize))
	s.flushInterval.Store(int64(flushInterval))
	select {
	case s.configChanged <- struct{}{}:
	default:
	}
}

// Start 启动单个审计写入协程，将请求热路径与关系型数据库批量写入解耦。
func (s *Service) Start() {
	s.startOnce.Do(func() {
		go s.runSupervised()
	})
}

// Record 将审计写入有界队列；突发满载时短暂等待，持续拥塞才降级丢弃审计。
func (s *Service) Record(value auditdomain.Record) bool {
	if s.stopped.Load() {
		return false
	}
	select {
	case s.queue <- value:
		return true
	default:
	}
	timer := time.NewTimer(auditEnqueueWait)
	defer timer.Stop()
	select {
	case s.queue <- value:
		return true
	case <-s.stop:
		return false
	case <-timer.C:
		dropped := s.dropped.Add(1)
		if dropped == 1 || dropped%1000 == 0 {
			s.logger.Warn("audit_queue_full", "dropped", dropped)
		}
		return false
	}
}

// Create 优先同步持久化审计和计费；数据库瞬时不可用时进入有界重试队列。
func (s *Service) Create(ctx context.Context, value auditdomain.Record) error {
	lastErr := s.createDurable(ctx, value)
	if lastErr == nil {
		return nil
	}
	if s.Record(value) {
		s.logger.Warn("audit_sync_write_deferred", "event_id", value.EventID, "error", lastErr)
		return nil
	}
	return errors.Join(lastErr, ErrQueueFull)
}

// CreateDurable 只有在审计及计费已提交到数据库后才返回成功。
func (s *Service) CreateDurable(ctx context.Context, value auditdomain.Record) error {
	return s.createDurable(ctx, value)
}

func (s *Service) createDurable(ctx context.Context, value auditdomain.Record) error {
	var lastErr error
	for attempt := 1; attempt <= auditWriteAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		persistCtx, cancel := context.WithTimeout(ctx, auditWriteTimeout)
		lastErr = s.audits.Create(persistCtx, value)
		cancel()
		if lastErr == nil {
			return nil
		}
		if attempt < auditWriteAttempts {
			timer := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return lastErr
}

// Close 停止接收新审计并尽力排空队列。
func (s *Service) Close(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.stopped.Store(true)
		close(s.stop)
	})
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) List(ctx context.Context, page, pageSize int) ([]auditdomain.Record, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return s.audits.List(ctx, (page-1)*pageSize, pageSize)
}

// CursorResult 表示按递减 ID 游标读取的一页审计记录。
type CursorResult struct {
	Items      []auditdomain.Record
	NextCursor string
	HasMore    bool
}

type ListFilter struct {
	Model   string
	Status  string
	Mode    string
	Key     string
	Account string
	Sort    repository.SortQuery
}

type auditCursorPayload struct {
	Version   int                      `json:"v"`
	Field     string                   `json:"field"`
	Direction repository.SortDirection `json:"direction"`
	ID        uint64                   `json:"id"`
	Value     string                   `json:"value"`
}

// ListCursor 使用复合游标读取审计，适合持续增长且支持多字段排序的大数据列表。
func (s *Service) ListCursor(ctx context.Context, rawCursor string, pageSize int, search, rawPeriod string, filter ListFilter) (CursorResult, error) {
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 100 {
		pageSize = 100
	}
	if filter.Sort.Field == "" && filter.Sort.Direction == "" {
		filter.Sort = repository.SortQuery{Field: "createdAt", Direction: repository.SortDescending}
	}
	if !validAuditFilter(filter.Status, "", "success", "clientError", "serverError", "2xx", "4xx", "5xx") || !validAuditFilter(filter.Mode, "", "stream", "nonStream") || !repository.IsValidSort(filter.Sort, "request", "model", "billing", "tokens", "status", "mode", "duration", "createdAt") {
		return CursorResult{}, ErrInvalidFilter
	}
	cursor, err := decodeAuditCursor(rawCursor, filter.Sort)
	if err != nil {
		return CursorResult{}, err
	}
	_, start, end, err := s.resolvePeriod(rawPeriod)
	if err != nil {
		return CursorResult{}, err
	}
	items, hasMore, err := s.audits.ListCursor(ctx, repository.AuditCursorQuery{Cursor: cursor, Limit: pageSize, Search: search, Start: start, End: end, Sort: filter.Sort, Filter: repository.AuditListFilter{
		Model: filter.Model, Status: filter.Status, Mode: filter.Mode, Key: filter.Key, Account: filter.Account,
	}})
	if err != nil {
		return CursorResult{}, err
	}
	result := CursorResult{Items: items, HasMore: hasMore}
	if hasMore && len(items) > 0 {
		result.NextCursor, err = encodeAuditCursor(items[len(items)-1], filter.Sort)
		if err != nil {
			return CursorResult{}, err
		}
	}
	return result, nil
}

func decodeAuditCursor(raw string, sort repository.SortQuery) (*repository.SortCursor, error) {
	if raw == "" {
		return nil, nil
	}
	encoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var payload auditCursorPayload
	if json.Unmarshal(encoded, &payload) != nil || payload.Version != 1 || payload.ID == 0 || payload.Field != sort.Field || payload.Direction != sort.Direction {
		return nil, ErrInvalidCursor
	}
	value, err := parseAuditCursorValue(payload.Field, payload.Value)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	return &repository.SortCursor{ID: payload.ID, Value: value}, nil
}

func encodeAuditCursor(value auditdomain.Record, sort repository.SortQuery) (string, error) {
	payload := auditCursorPayload{Version: 1, Field: sort.Field, Direction: sort.Direction, ID: value.ID, Value: formatAuditCursorValue(value, sort.Field)}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func parseAuditCursorValue(field, value string) (any, error) {
	switch field {
	case "request", "model":
		return value, nil
	case "billing", "tokens", "status", "mode", "duration":
		return strconv.ParseInt(value, 10, 64)
	case "createdAt":
		return time.Parse(time.RFC3339Nano, value)
	default:
		return nil, ErrInvalidCursor
	}
}

func formatAuditCursorValue(value auditdomain.Record, field string) string {
	switch field {
	case "request":
		return value.RequestID
	case "model":
		return strings.ToLower(value.ModelPublicID)
	case "billing":
		amount := value.CostInUSDTicks
		if amount == 0 {
			amount = value.EstimatedCostInUSDTicks
		}
		return strconv.FormatInt(amount, 10)
	case "tokens":
		return strconv.FormatInt(value.TotalTokens, 10)
	case "status":
		return strconv.Itoa(value.StatusCode)
	case "mode":
		if value.Streaming {
			return "1"
		}
		return "0"
	case "duration":
		return strconv.FormatInt(value.DurationMS, 10)
	default:
		return value.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
}

type SummaryUsage struct {
	Requests                int64
	SuccessfulRequests      int64
	FailedRequests          int64
	InputTokens             int64
	CachedInputTokens       int64
	OutputTokens            int64
	ReasoningTokens         int64
	TotalTokens             int64
	AverageDurationMS       float64
	SuccessRate             float64
	EstimatedCostInUSDTicks int64
	PricedRequests          int64
	UnpricedRequests        int64
	PricedTokens            int64
	UnpricedTokens          int64
}

type SummaryResult struct {
	Period      Period
	GeneratedAt time.Time
	Start       time.Time
	End         time.Time
	Usage       SummaryUsage
}

func (s *Service) Summary(ctx context.Context, search, rawPeriod string, filter ListFilter) (SummaryResult, error) {
	return s.summary(ctx, search, rawPeriod, filter, true)
}

// SummaryFresh 绕过短缓存，供管理员显式刷新时读取最新汇总。
func (s *Service) SummaryFresh(ctx context.Context, search, rawPeriod string, filter ListFilter) (SummaryResult, error) {
	return s.summary(ctx, search, rawPeriod, filter, false)
}

func (s *Service) summary(ctx context.Context, search, rawPeriod string, filter ListFilter, useCache bool) (SummaryResult, error) {
	if !validAuditFilter(filter.Status, "", "success", "clientError", "serverError", "2xx", "4xx", "5xx") || !validAuditFilter(filter.Mode, "", "stream", "nonStream") {
		return SummaryResult{}, ErrInvalidFilter
	}
	period, start, end, err := s.resolvePeriod(rawPeriod)
	if err != nil {
		return SummaryResult{}, err
	}
	if !useCache {
		return s.loadSummary(ctx, search, filter, period, start, end)
	}
	cacheKey := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s", period, search, filter.Model, filter.Status, filter.Mode, filter.Key, filter.Account)
	return s.summaryCache.Load(cacheKey, end, func() (SummaryResult, error) {
		return s.loadSummary(ctx, search, filter, period, start, end)
	})
}

func (s *Service) loadSummary(ctx context.Context, search string, filter ListFilter, period Period, start, end time.Time) (SummaryResult, error) {
	aggregate, err := s.audits.Summarize(ctx, repository.AuditSummaryQuery{Search: search, Start: start, End: end, Filter: repository.AuditListFilter{
		Model: filter.Model, Status: filter.Status, Mode: filter.Mode, Key: filter.Key, Account: filter.Account,
	}})
	if err != nil {
		return SummaryResult{}, err
	}
	usage := SummaryUsage{
		Requests: aggregate.Requests, SuccessfulRequests: aggregate.SuccessfulRequests, FailedRequests: aggregate.FailedRequests,
		InputTokens: aggregate.InputTokens, CachedInputTokens: aggregate.CachedInputTokens, OutputTokens: aggregate.OutputTokens,
		ReasoningTokens: aggregate.ReasoningTokens, TotalTokens: aggregate.TotalTokens,
		EstimatedCostInUSDTicks: aggregate.EstimatedCostInUSDTicks, PricedRequests: aggregate.PricedRequests,
		UnpricedRequests: aggregate.UnpricedRequests, PricedTokens: aggregate.PricedTokens, UnpricedTokens: aggregate.UnpricedTokens,
	}
	if aggregate.Requests > 0 {
		usage.SuccessRate = float64(aggregate.SuccessfulRequests) / float64(aggregate.Requests) * 100
		usage.AverageDurationMS = float64(aggregate.DurationMS) / float64(aggregate.Requests)
	}
	return SummaryResult{Period: period, GeneratedAt: end, Start: start, End: end, Usage: usage}, nil
}

func (s *Service) resolvePeriod(value string) (Period, time.Time, time.Time, error) {
	period, duration, err := parsePeriod(value)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	end := s.now().UTC()
	return period, end.Add(-duration), end, nil
}

func parsePeriod(value string) (Period, time.Duration, error) {
	if value == "" {
		value = string(Period24Hours)
	}
	switch Period(value) {
	case Period24Hours:
		return Period24Hours, 24 * time.Hour, nil
	case Period7Days:
		return Period7Days, 7 * 24 * time.Hour, nil
	case Period30Days:
		return Period30Days, 30 * 24 * time.Hour, nil
	case Period90Days:
		return Period90Days, 90 * 24 * time.Hour, nil
	default:
		return "", 0, ErrInvalidPeriod
	}
}

func validAuditFilter(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func (s *Service) runSupervised() {
	defer close(s.done)
	backoff := 100 * time.Millisecond
	for {
		err := batch.Do(context.Background(), func(context.Context) error {
			s.run()
			return nil
		})
		if err == nil {
			return
		}
		var panicErr *batch.PanicError
		if errors.As(err, &panicErr) {
			s.logger.Error("audit_worker_restarting", "backoff", backoff, "error", panicErr, "stack", string(panicErr.Stack))
		} else {
			s.logger.Error("audit_worker_restarting", "backoff", backoff, "error", err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-s.stop:
			timer.Stop()
			_ = batch.Do(context.Background(), func(context.Context) error {
				s.run()
				return nil
			})
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (s *Service) run() {
	ticker := time.NewTicker(time.Duration(s.flushInterval.Load()))
	defer ticker.Stop()
	batch := make([]auditdomain.Record, 0, int(s.batchSize.Load()))
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.persistBatch(batch)
		batch = batch[:0]
	}
	for {
		select {
		case value := <-s.queue:
			batch = append(batch, value)
			if len(batch) >= int(s.batchSize.Load()) {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.configChanged:
			ticker.Reset(time.Duration(s.flushInterval.Load()))
			if len(batch) >= int(s.batchSize.Load()) {
				flush()
			}
		case <-s.stop:
			for {
				select {
				case value := <-s.queue:
					batch = append(batch, value)
					if len(batch) >= int(s.batchSize.Load()) {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *Service) persistBatch(records []auditdomain.Record) {
	var lastErr error
	for attempt := 1; attempt <= auditWriteAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
		lastErr = batch.Do(ctx, func(workCtx context.Context) error { return s.audits.CreateBatch(workCtx, records) })
		cancel()
		if lastErr == nil {
			return
		}
		if attempt < auditWriteAttempts {
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
	}
	dropped := s.dropped.Add(uint64(len(records)))
	var panicErr *batch.PanicError
	if errors.As(lastErr, &panicErr) {
		s.logger.Error("audit_batch_write_failed", "count", len(records), "attempts", auditWriteAttempts, "dropped", dropped, "error", panicErr, "stack", string(panicErr.Stack))
	} else {
		s.logger.Error("audit_batch_write_failed", "count", len(records), "attempts", auditWriteAttempts, "dropped", dropped, "error", lastErr)
	}
}
