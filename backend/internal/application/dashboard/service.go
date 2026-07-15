package dashboard

import (
	"context"
	"errors"
	"strings"
	"time"

	dashboarddomain "github.com/chenyme/grok2api/backend/internal/domain/dashboard"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var ErrInvalidPeriod = errors.New("Dashboard 时间范围无效")
var ErrInvalidTimezone = errors.New("Dashboard 时区无效")

const dashboardCacheTTL = 15 * time.Second

type Period string

const (
	Period24Hours Period = "24h"
	Period7Days   Period = "7d"
	Period30Days  Period = "30d"
	Period90Days  Period = "90d"
)

type Range struct {
	Start time.Time
	End   time.Time
}

type SeriesPoint struct {
	Start              time.Time
	End                time.Time
	Requests           int64
	InputTokens        int64
	CachedInputTokens  int64
	OutputTokens       int64
	ReasoningTokens    int64
	Tokens             int64
	BilledCostUSDTicks int64
	Models             []ModelBucket
}

type ModelBucket struct {
	Model              string
	Tokens             int64
	BilledCostUSDTicks int64
}

type ModelUsage struct {
	Model              string
	Requests           int64
	InputTokens        int64
	CachedInputTokens  int64
	OutputTokens       int64
	ReasoningTokens    int64
	Tokens             int64
	BilledCostUSDTicks int64
}

type Result struct {
	Period      Period
	GeneratedAt time.Time
	Range       Range
	Resources   dashboarddomain.Resources
	Usage       dashboarddomain.Usage
	SuccessRate float64
	Series      []SeriesPoint
	TopModels   []ModelUsage
}

// Service 负责 Dashboard 时间范围校验和固定时间桶编排。
type Service struct {
	dashboard repository.DashboardRepository
	now       func() time.Time
	cache     *resultcache.Cache[string, Result]
}

func NewService(dashboard repository.DashboardRepository) *Service {
	return &Service{dashboard: dashboard, now: time.Now, cache: resultcache.New[string, Result](32, dashboardCacheTTL)}
}

// Get 返回指定时间范围的 Dashboard 聚合快照。
func (s *Service) Get(ctx context.Context, rawPeriod, rawTimezone string) (Result, error) {
	return s.get(ctx, rawPeriod, rawTimezone, true)
}

// Refresh 绕过短缓存，供管理员显式刷新时读取最新聚合数据。
func (s *Service) Refresh(ctx context.Context, rawPeriod, rawTimezone string) (Result, error) {
	return s.get(ctx, rawPeriod, rawTimezone, false)
}

func (s *Service) get(ctx context.Context, rawPeriod, rawTimezone string, useCache bool) (Result, error) {
	period, bucketCount, bucketDays, err := parsePeriod(rawPeriod)
	if err != nil {
		return Result{}, err
	}
	location, err := parseTimezone(rawTimezone)
	if err != nil {
		return Result{}, err
	}
	rawNow := s.now()
	if !useCache {
		return s.load(ctx, period, bucketCount, bucketDays, location, rawNow)
	}
	cacheKey := string(period) + "\x00" + location.String()
	return s.cache.Load(ctx, cacheKey, rawNow, func() (Result, error) {
		return s.load(ctx, period, bucketCount, bucketDays, location, rawNow)
	})
}

func (s *Service) load(ctx context.Context, period Period, bucketCount, bucketDays int, location *time.Location, rawNow time.Time) (Result, error) {
	now := rawNow.In(location)
	series := buildSeries(now, period, bucketCount, bucketDays)
	boundaries := make([]time.Time, 0, len(series))
	for _, point := range series {
		boundaries = append(boundaries, point.Start)
	}
	boundaries = append(boundaries, series[len(series)-1].End)
	generatedAt := rawNow.UTC()
	aggregate, err := s.dashboard.Snapshot(ctx, boundaries, generatedAt)
	if err != nil {
		return Result{}, err
	}
	for _, bucket := range aggregate.Buckets {
		if bucket.Index < 0 || bucket.Index >= len(series) {
			continue
		}
		series[bucket.Index].Requests = bucket.Requests
		series[bucket.Index].InputTokens = bucket.InputTokens
		series[bucket.Index].CachedInputTokens = bucket.CachedInputTokens
		series[bucket.Index].OutputTokens = bucket.OutputTokens
		series[bucket.Index].ReasoningTokens = bucket.ReasoningTokens
		series[bucket.Index].Tokens = bucket.Tokens
		series[bucket.Index].BilledCostUSDTicks = bucket.BilledCostUSDTicks
	}
	for _, bucket := range aggregate.ModelBuckets {
		if bucket.Index < 0 || bucket.Index >= len(series) {
			continue
		}
		series[bucket.Index].Models = append(series[bucket.Index].Models, ModelBucket{Model: bucket.Model, Tokens: bucket.Tokens, BilledCostUSDTicks: bucket.BilledCostUSDTicks})
	}
	successRate := 0.0
	if aggregate.Usage.Requests > 0 {
		successRate = float64(aggregate.Usage.SuccessfulRequests) / float64(aggregate.Usage.Requests) * 100
	}
	topModels := make([]ModelUsage, 0, len(aggregate.TopModels))
	for _, item := range aggregate.TopModels {
		topModels = append(topModels, ModelUsage{Model: item.Model, Requests: item.Requests, InputTokens: item.InputTokens, CachedInputTokens: item.CachedInputTokens, OutputTokens: item.OutputTokens, ReasoningTokens: item.ReasoningTokens, Tokens: item.Tokens, BilledCostUSDTicks: item.BilledCostUSDTicks})
	}
	return Result{Period: period, GeneratedAt: generatedAt, Range: Range{Start: boundaries[0], End: boundaries[len(boundaries)-1]}, Resources: aggregate.Resources, Usage: aggregate.Usage, SuccessRate: successRate, Series: series, TopModels: topModels}, nil
}

func parseTimezone(value string) (*time.Location, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "UTC"
	}
	if value == "Local" || len(value) > 128 {
		return nil, ErrInvalidTimezone
	}
	location, err := time.LoadLocation(value)
	if err != nil {
		return nil, ErrInvalidTimezone
	}
	return location, nil
}

func parsePeriod(value string) (Period, int, int, error) {
	if value == "" {
		value = string(Period24Hours)
	}
	switch Period(value) {
	case Period24Hours:
		return Period24Hours, 24, 0, nil
	case Period7Days:
		return Period7Days, 7, 1, nil
	case Period30Days:
		return Period30Days, 30, 1, nil
	case Period90Days:
		return Period90Days, 6, 15, nil
	default:
		return "", 0, 0, ErrInvalidPeriod
	}
}

func alignedRange(now time.Time, period Period) (time.Time, time.Time) {
	location := now.Location()
	if period == Period24Hours {
		currentHour := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, location)
		return currentHour.Add(-23 * time.Hour), currentHour.Add(time.Hour)
	}
	tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, location)
	switch period {
	case Period7Days:
		return tomorrow.AddDate(0, 0, -7), tomorrow
	case Period30Days:
		return tomorrow.AddDate(0, 0, -30), tomorrow
	default:
		return tomorrow.AddDate(0, 0, -90), tomorrow
	}
}

func buildSeries(now time.Time, period Period, bucketCount, bucketDays int) []SeriesPoint {
	start, _ := alignedRange(now, period)
	series := make([]SeriesPoint, bucketCount)
	for index := range series {
		bucketStart := start.AddDate(0, 0, index*bucketDays)
		bucketEnd := bucketStart.AddDate(0, 0, bucketDays)
		if period == Period24Hours {
			bucketStart = start.Add(time.Duration(index) * time.Hour)
			bucketEnd = bucketStart.Add(time.Hour)
		}
		series[index] = SeriesPoint{Start: bucketStart.UTC(), End: bucketEnd.UTC()}
	}
	return series
}
