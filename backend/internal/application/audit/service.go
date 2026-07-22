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
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrQueueFull         = errors.New("审计写入队列已满")
	ErrWriterUnavailable = errors.New("audit writer is not running")
	ErrInvalidCursor     = errors.New("审计游标无效")
	ErrInvalidFilter     = errors.New("审计筛选条件无效")
	ErrInvalidPeriod     = errors.New("审计时间范围无效")
	ErrLedgerUnavailable = errors.New("billing ledger is not ready")
)

type LedgerMode string

const (
	LedgerModeObserve LedgerMode = "observe"
	LedgerModeEnforce LedgerMode = "enforce"
)

type LedgerConfig struct {
	Mode                      LedgerMode
	FailureThreshold          int
	UnhealthyGrace            time.Duration
	QueueHighWatermarkPercent int
}

type LedgerSnapshot struct {
	Mode                LedgerMode
	Ready               bool
	Irrecoverable       bool
	QueueDepth          int
	QueueCapacity       int
	ConsecutiveFailures int
	Dropped             uint64
	LastSuccessAt       time.Time
	LastFailureAt       time.Time
	UnhealthySince      time.Time
}

type Period string

const (
	Period24Hours Period = "24h"
	Period7Days   Period = "7d"
	Period30Days  Period = "30d"
	Period90Days  Period = "90d"
)

const (
	auditEnqueueWait        = 25 * time.Millisecond
	auditWriteTimeout       = 2 * time.Second
	auditWriteAttempts      = 3
	auditWriteRetryBase     = 250 * time.Millisecond
	auditWriteRetryMax      = 5 * time.Second
	auditDefaultCommitDelay = 5 * time.Millisecond
	auditSummaryTTL         = 10 * time.Second
)

type auditWriteRequest struct {
	record auditdomain.Record
	ack    chan error
}

// Service 提供请求元数据审计查询，以及有界异步批量写入。
type Service struct {
	audits               repository.AuditRepository
	logger               *slog.Logger
	queue                chan auditWriteRequest
	batchSize            atomic.Int64
	flushInterval        atomic.Int64
	commitDelay          atomic.Int64
	configChanged        chan struct{}
	lifecycleMu          sync.RWMutex
	queueSpace           chan struct{}
	queueWaiters         atomic.Int64
	startOnce            sync.Once
	stopOnce             sync.Once
	stop                 chan struct{}
	done                 chan struct{}
	stopped              atomic.Bool
	started              atomic.Bool
	dropped              atomic.Uint64
	now                  func() time.Time
	summaryCache         *resultcache.Cache[string, SummaryResult]
	ledgerMu             sync.Mutex
	ledgerConfig         LedgerConfig
	ledgerFailures       int
	ledgerUnhealthySince time.Time
	ledgerLastSuccess    time.Time
	ledgerLastFailure    time.Time
	ledgerLastDrop       time.Time
	ledgerQueueHighSince time.Time
	ledgerLastWarning    time.Time
	observerMu           sync.RWMutex
	commitObserver       func([]string)
	dropObserver         func([]string)
}

func NewService(audits repository.AuditRepository, logger *slog.Logger, bufferSize, batchSize int, flushInterval time.Duration) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	service := &Service{
		audits: audits, logger: logger, queue: make(chan auditWriteRequest, bufferSize),
		configChanged: make(chan struct{}, 1), queueSpace: make(chan struct{}, bufferSize), stop: make(chan struct{}), done: make(chan struct{}),
		now: time.Now, summaryCache: resultcache.New[string, SummaryResult](64, auditSummaryTTL),
		ledgerConfig: defaultLedgerConfig(),
	}
	service.UpdateConfig(batchSize, flushInterval)
	return service
}

func defaultLedgerConfig() LedgerConfig {
	return LedgerConfig{
		Mode:                      LedgerModeEnforce,
		FailureThreshold:          1,
		UnhealthyGrace:            10 * time.Second,
		QueueHighWatermarkPercent: 90,
	}
}

// UpdateLedgerConfig changes readiness policy without altering the audit writer.
func (s *Service) UpdateLedgerConfig(value LedgerConfig) {
	defaults := defaultLedgerConfig()
	if value.Mode != LedgerModeObserve && value.Mode != LedgerModeEnforce {
		value.Mode = defaults.Mode
	}
	if value.FailureThreshold <= 0 {
		value.FailureThreshold = defaults.FailureThreshold
	}
	if value.UnhealthyGrace <= 0 {
		value.UnhealthyGrace = defaults.UnhealthyGrace
	}
	if value.QueueHighWatermarkPercent < 1 || value.QueueHighWatermarkPercent > 100 {
		value.QueueHighWatermarkPercent = defaults.QueueHighWatermarkPercent
	}
	s.ledgerMu.Lock()
	s.ledgerConfig = value
	s.ledgerMu.Unlock()
}

// SetCommitObserver registers a lightweight callback invoked after the audit and billing transaction commits.
func (s *Service) SetCommitObserver(observer func([]string)) {
	s.observerMu.Lock()
	s.commitObserver = observer
	s.observerMu.Unlock()
}

// SetDropObserver registers a callback for records that can no longer enter
// the writer. It must not release the durable billing reservation itself.
func (s *Service) SetDropObserver(observer func([]string)) {
	s.observerMu.Lock()
	s.dropObserver = observer
	s.observerMu.Unlock()
}

// LedgerSnapshot returns a bounded, identity-free view of the durable ledger state.
func (s *Service) LedgerSnapshot() LedgerSnapshot {
	now := s.now().UTC()
	queueDepth := len(s.queue)
	queueCapacity := cap(s.queue)

	s.ledgerMu.Lock()
	s.updateQueuePressureLocked(now, queueDepth, queueCapacity)
	dropped := s.dropped.Load()
	ready := dropped == 0 && s.ledgerReadyLocked(now)
	snapshot := LedgerSnapshot{
		Mode:                s.ledgerConfig.Mode,
		Ready:               ready,
		Irrecoverable:       dropped > 0,
		QueueDepth:          queueDepth,
		QueueCapacity:       queueCapacity,
		ConsecutiveFailures: s.ledgerFailures,
		Dropped:             dropped,
		LastSuccessAt:       s.ledgerLastSuccess,
		LastFailureAt:       s.ledgerLastFailure,
		UnhealthySince:      s.ledgerUnhealthySince,
	}
	s.ledgerMu.Unlock()

	perfmetrics.Default.SetGauge("audit_queue_depth", perfmetrics.Labels{Subsystem: "audit", Stage: "queue"}, int64(queueDepth))
	perfmetrics.Default.SetGauge("audit_queue_capacity", perfmetrics.Labels{Subsystem: "audit", Stage: "queue"}, int64(queueCapacity))
	return snapshot
}

// CheckLedgerReady always blocks after confirmed data loss. Observe mode only
// permits traffic while a recoverable writer or queue degradation is active.
func (s *Service) CheckLedgerReady() error {
	snapshot := s.LedgerSnapshot()
	if snapshot.Ready && !snapshot.Irrecoverable {
		return nil
	}
	if !snapshot.Irrecoverable && snapshot.Mode != LedgerModeEnforce {
		return nil
	}
	return ErrLedgerUnavailable
}

func (s *Service) UpdateConfig(batchSize int, flushInterval time.Duration) {
	s.UpdateWriterConfig(batchSize, flushInterval, time.Duration(s.commitDelay.Load()))
}

// UpdateWriterConfig hot-reloads batching limits without replacing the active queue.
func (s *Service) UpdateWriterConfig(batchSize int, flushInterval, commitDelay time.Duration) {
	if commitDelay <= 0 {
		commitDelay = auditDefaultCommitDelay
	}
	s.batchSize.Store(int64(batchSize))
	s.flushInterval.Store(int64(flushInterval))
	s.commitDelay.Store(int64(commitDelay))
	select {
	case s.configChanged <- struct{}{}:
	default:
	}
}

// Start 启动单个审计写入协程，将请求热路径与关系型数据库批量写入解耦。
func (s *Service) Start() {
	s.startOnce.Do(func() {
		s.started.Store(true)
		go s.runSupervised()
	})
}

// Record 将审计写入有界队列；突发满载时短暂等待，持续拥塞才降级丢弃审计。
func (s *Service) Record(value auditdomain.Record) bool {
	return s.enqueueBestEffort(context.Background(), auditWriteRequest{record: value}) == nil
}

// Create returns success only after the audit and billing transaction commits.
func (s *Service) Create(ctx context.Context, value auditdomain.Record) error {
	return s.createAcknowledged(ctx, value)
}

// CreateDurable returns success only after the audit and billing transaction commits.
func (s *Service) CreateDurable(ctx context.Context, value auditdomain.Record) error {
	return s.createAcknowledged(ctx, value)
}

func (s *Service) createAcknowledged(ctx context.Context, value auditdomain.Record) error {
	startedAt := time.Now()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.started.Load() || s.stopped.Load() {
		return ErrWriterUnavailable
	}
	request := auditWriteRequest{record: value, ack: make(chan error, 1)}
	if err := s.enqueueAcknowledged(ctx, request); err != nil {
		return err
	}
	select {
	case err := <-request.ack:
		outcome := "success"
		if err != nil {
			outcome = "failed"
		}
		labels := perfmetrics.Labels{Subsystem: "audit", Operation: string(value.Operation), Stage: "ack", Outcome: outcome}
		perfmetrics.Default.Inc("audit_records_total", labels)
		perfmetrics.Default.ObserveDuration("audit_ack_duration_us", labels, time.Since(startedAt))
		return err
	case <-ctx.Done():
		labels := perfmetrics.Labels{Subsystem: "audit", Operation: string(value.Operation), Stage: "ack", Outcome: "timeout"}
		perfmetrics.Default.Inc("audit_records_total", labels)
		perfmetrics.Default.ObserveDuration("audit_ack_duration_us", labels, time.Since(startedAt))
		return ctx.Err()
	}
}

func (s *Service) tryEnqueue(request auditWriteRequest) error {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	if s.stopped.Load() {
		return ErrWriterUnavailable
	}
	select {
	case s.queue <- request:
		return nil
	default:
		return ErrQueueFull
	}
}

func (s *Service) enqueueBestEffort(ctx context.Context, request auditWriteRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.tryEnqueue(request); !errors.Is(err, ErrQueueFull) {
		return err
	}
	s.queueWaiters.Add(1)
	defer s.queueWaiters.Add(-1)
	timer := time.NewTimer(auditEnqueueWait)
	defer timer.Stop()
	for {
		if err := s.tryEnqueue(request); !errors.Is(err, ErrQueueFull) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stop:
			return ErrWriterUnavailable
		case <-timer.C:
			s.recordEnqueueDrop(request, "queue_full")
			return ErrQueueFull
		case <-s.queueSpace:
		}
	}
}

func (s *Service) enqueueAcknowledged(ctx context.Context, request auditWriteRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.tryEnqueue(request); !errors.Is(err, ErrQueueFull) {
		return err
	}
	s.queueWaiters.Add(1)
	defer s.queueWaiters.Add(-1)
	for {
		if err := s.tryEnqueue(request); !errors.Is(err, ErrQueueFull) {
			return err
		}
		select {
		case <-ctx.Done():
			s.recordEnqueueDrop(request, "context_done")
			return ctx.Err()
		case <-s.stop:
			s.recordEnqueueDrop(request, "writer_stopping")
			return ErrWriterUnavailable
		case <-s.queueSpace:
		}
	}
}

func (s *Service) notifyQueueSpace() {
	if s.queueWaiters.Load() <= 0 {
		return
	}
	select {
	case s.queueSpace <- struct{}{}:
	default:
	}
}

func (s *Service) recordEnqueueDrop(request auditWriteRequest, reason string) {
	dropped := s.dropped.Add(1)
	s.recordLedgerDrop()
	perfmetrics.Default.Inc("audit_records_total", perfmetrics.Labels{Subsystem: "audit", Operation: string(request.record.Operation), Stage: "enqueue", Outcome: "dropped"})
	if dropped == 1 || dropped%1000 == 0 {
		s.logger.Warn("audit_queue_full", "reason", reason, "dropped", dropped)
	}
	s.notifyDropped(request.record.EventID)
}

// Close 停止接收新审计并尽力排空队列。
func (s *Service) Close(ctx context.Context) error {
	if !s.started.Load() {
		s.Start()
	}
	s.stopOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.stopped.Store(true)
		close(s.stop)
		s.lifecycleMu.Unlock()
	})
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) List(ctx context.Context, page, pageSize int) ([]auditdomain.Record, int64, error) {
	page, pageSize = repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
	return s.audits.List(ctx, (page-1)*pageSize, pageSize)
}

func (s *Service) Get(ctx context.Context, id uint64) (auditdomain.Record, error) {
	return s.audits.Get(ctx, id)
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
	_, pageSize = repository.NormalizePage(1, pageSize, repository.DefaultCursorPageSize)
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
	return s.summaryCache.Load(ctx, cacheKey, end, func() (SummaryResult, error) {
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
	var timer *time.Timer
	var timerC <-chan time.Time
	requests := make([]auditWriteRequest, 0, int(s.batchSize.Load()))
	hasAck := false
	resetTimer := func(delay time.Duration) {
		if delay <= 0 {
			delay = auditDefaultCommitDelay
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(delay)
		}
		timerC = timer.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	flush := func() {
		if len(requests) == 0 {
			return
		}
		s.persistBatch(requests)
		requests = requests[:0]
		hasAck = false
		timerC = nil
	}
	appendRequest := func(request auditWriteRequest) {
		wasEmpty := len(requests) == 0
		requests = append(requests, request)
		if request.ack != nil && !hasAck {
			hasAck = true
			resetTimer(time.Duration(s.commitDelay.Load()))
		} else if wasEmpty {
			resetTimer(time.Duration(s.flushInterval.Load()))
		}
	}
	for {
		select {
		case request := <-s.queue:
			s.notifyQueueSpace()
			appendRequest(request)
			if len(requests) >= int(s.batchSize.Load()) {
				flush()
			}
		case <-timerC:
			flush()
		case <-s.configChanged:
			if len(requests) >= int(s.batchSize.Load()) {
				flush()
			} else if len(requests) > 0 {
				if hasAck {
					resetTimer(time.Duration(s.commitDelay.Load()))
				} else {
					resetTimer(time.Duration(s.flushInterval.Load()))
				}
			}
		case <-s.stop:
			for {
				select {
				case request := <-s.queue:
					s.notifyQueueSpace()
					appendRequest(request)
					if len(requests) >= int(s.batchSize.Load()) {
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

func (s *Service) persistBatch(requests []auditWriteRequest) {
	startedAt := time.Now()
	pending := append([]auditWriteRequest(nil), requests...)
	retryDelay := auditWriteRetryBase
	retryRound := 0
	for len(pending) > 0 {
		lastErr := s.persistAuditRequests(pending)
		var invalid *repository.InvalidBatchRecordError
		if errors.As(lastErr, &invalid) && invalid.Index >= 0 && invalid.Index < len(pending) {
			rejected := pending[invalid.Index]
			completeAuditWrites([]auditWriteRequest{rejected}, lastErr)
			s.recordRejectedAudit(rejected, lastErr)
			pending = append(pending[:invalid.Index], pending[invalid.Index+1:]...)
			continue
		}
		if lastErr == nil {
			records := auditRecords(pending)
			s.recordLedgerSuccess()
			s.notifyCommitted(records)
			perfmetrics.Default.Add("audit_records_total", perfmetrics.Labels{Subsystem: "audit", Stage: "batch", Outcome: "success"}, int64(len(records)))
			perfmetrics.Default.Add("audit_batch_size", perfmetrics.Labels{Subsystem: "audit", Stage: "batch", Outcome: "success"}, int64(len(records)))
			perfmetrics.Default.ObserveDuration("audit_batch_commit_duration_us", perfmetrics.Labels{Subsystem: "audit", Stage: "batch", Outcome: "success"}, time.Since(startedAt))
			completeAuditWrites(pending, nil)
			return
		}
		perfmetrics.Default.Add("audit_batch_size", perfmetrics.Labels{Subsystem: "audit", Stage: "batch", Outcome: "retry"}, int64(len(pending)))
		retryRound++
		var panicErr *batch.PanicError
		if errors.As(lastErr, &panicErr) {
			s.logger.Error("audit_batch_write_retrying", "count", len(pending), "attempts", auditWriteAttempts, "retry_in", retryDelay, "error", panicErr, "stack", string(panicErr.Stack))
		} else if retryRound == 1 {
			s.logger.Warn("audit_batch_write_retrying", "count", len(pending), "attempts", auditWriteAttempts, "retry_in", retryDelay, "error", lastErr)
		} else {
			s.logger.Debug("audit_batch_write_retrying", "count", len(pending), "attempts", auditWriteAttempts, "retry_in", retryDelay, "error", lastErr)
		}
		timer := time.NewTimer(retryDelay)
		<-timer.C
		retryDelay = min(retryDelay*2, auditWriteRetryMax)
	}
}

func (s *Service) persistAuditRequests(requests []auditWriteRequest) error {
	records := auditRecords(requests)
	var lastErr error
	for attempt := 1; attempt <= auditWriteAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
		lastErr = batch.Do(ctx, func(workCtx context.Context) error { return s.audits.CreateBatch(workCtx, records) })
		cancel()
		if lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, repository.ErrInvalidRecord) {
			return lastErr
		}
		s.recordLedgerFailure()
		if attempt < auditWriteAttempts {
			timer := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
			select {
			case <-s.stop:
				timer.Stop()
			case <-timer.C:
			}
		}
	}
	return lastErr
}

func auditRecords(requests []auditWriteRequest) []auditdomain.Record {
	records := make([]auditdomain.Record, len(requests))
	for index := range requests {
		records[index] = requests[index].record
	}
	return records
}

func (s *Service) recordRejectedAudit(request auditWriteRequest, err error) {
	dropped := s.dropped.Add(1)
	s.recordLedgerDrop()
	perfmetrics.Default.Inc("audit_records_total", perfmetrics.Labels{Subsystem: "audit", Operation: string(request.record.Operation), Stage: "batch", Outcome: "rejected"})
	s.logger.Error("audit_record_rejected", "event_id", request.record.EventID, "dropped", dropped, "error", err)
	s.notifyDropped(request.record.EventID)
}

func completeAuditWrites(requests []auditWriteRequest, err error) {
	for _, request := range requests {
		if request.ack == nil {
			continue
		}
		select {
		case request.ack <- err:
		default:
		}
	}
}

func (s *Service) recordLedgerSuccess() {
	now := s.now().UTC()
	s.ledgerMu.Lock()
	s.ledgerFailures = 0
	s.ledgerLastSuccess = now
	if s.dropped.Load() == 0 && s.ledgerQueueHighSince.IsZero() {
		s.ledgerUnhealthySince = time.Time{}
	}
	s.ledgerMu.Unlock()
}

func (s *Service) recordLedgerFailure() {
	now := s.now().UTC()
	s.ledgerMu.Lock()
	s.ledgerFailures++
	s.ledgerLastFailure = now
	if s.ledgerFailures >= s.ledgerConfig.FailureThreshold && s.ledgerUnhealthySince.IsZero() {
		s.ledgerUnhealthySince = now
	}
	s.warnLedgerIfNeededLocked(now, "durable_write_failed")
	s.ledgerMu.Unlock()
}

func (s *Service) recordLedgerDrop() {
	now := s.now().UTC()
	s.ledgerMu.Lock()
	s.ledgerLastDrop = now
	if s.ledgerUnhealthySince.IsZero() {
		s.ledgerUnhealthySince = now
	}
	s.warnLedgerIfNeededLocked(now, "audit_record_dropped")
	s.ledgerMu.Unlock()
}

func (s *Service) updateQueuePressureLocked(now time.Time, depth, capacity int) {
	if capacity <= 0 || depth*100 < capacity*s.ledgerConfig.QueueHighWatermarkPercent {
		wasHigh := !s.ledgerQueueHighSince.IsZero()
		s.ledgerQueueHighSince = time.Time{}
		if wasHigh && s.ledgerFailures == 0 && s.dropped.Load() == 0 {
			s.ledgerUnhealthySince = time.Time{}
		}
		return
	}
	if s.ledgerQueueHighSince.IsZero() {
		s.ledgerQueueHighSince = now
	}
	if s.ledgerUnhealthySince.IsZero() {
		s.ledgerUnhealthySince = s.ledgerQueueHighSince
	}
}

func (s *Service) ledgerReadyLocked(now time.Time) bool {
	degradedSince := s.ledgerUnhealthySince
	if !s.ledgerQueueHighSince.IsZero() && (degradedSince.IsZero() || s.ledgerQueueHighSince.Before(degradedSince)) {
		degradedSince = s.ledgerQueueHighSince
	}
	if degradedSince.IsZero() {
		return true
	}
	return now.Sub(degradedSince) < s.ledgerConfig.UnhealthyGrace
}

func (s *Service) warnLedgerIfNeededLocked(now time.Time, reason string) {
	if !s.ledgerLastWarning.IsZero() && now.Sub(s.ledgerLastWarning) < time.Minute {
		return
	}
	s.ledgerLastWarning = now
	s.logger.Warn("billing_ledger_degraded", "reason", reason, "mode", s.ledgerConfig.Mode, "consecutive_failures", s.ledgerFailures, "queue_depth", len(s.queue), "queue_capacity", cap(s.queue))
}

func (s *Service) notifyCommitted(records []auditdomain.Record) {
	s.observerMu.RLock()
	observer := s.commitObserver
	s.observerMu.RUnlock()
	if observer == nil {
		return
	}
	eventIDs := make([]string, 0, len(records))
	for _, record := range records {
		if record.EventID != "" {
			eventIDs = append(eventIDs, record.EventID)
		}
	}
	if len(eventIDs) > 0 {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					s.logger.Error("audit_commit_observer_panicked", "error", recovered)
				}
			}()
			observer(eventIDs)
		}()
	}
}

func (s *Service) notifyDropped(eventID string) {
	if eventID == "" {
		return
	}
	s.observerMu.RLock()
	observer := s.dropObserver
	s.observerMu.RUnlock()
	if observer == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("audit_drop_observer_panicked", "error", recovered)
		}
	}()
	observer([]string{eventID})
}
