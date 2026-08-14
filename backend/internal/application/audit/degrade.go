package audit

import (
	"context"
	"math"
	"strings"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	degradeWindow1h        = "1h"
	degradeWindow6h        = "6h"
	degradeWindow24h       = "24h"
	degradeWindow7d        = "7d"
	degradeRecentEventCap  = 80
	degradeDefaultPage     = 1
	degradeDefaultPageSize = 50
	degradeMaxPageSize     = 100
)

type DegradeThresholds struct {
	SoftTPS    float64
	HardTPS    float64
	MinGenMS   int64
	MinOut     int64
	FailClosed bool
}

type DegradeAccountFilter struct {
	Search   string
	Status   string
	Class    string
	MinHits  int
	Page     int
	PageSize int
}

type DegradeSummary struct {
	Window      string
	GeneratedAt time.Time
	Thresholds  DegradeThresholds
	Totals      DegradeTotals
	Series      []DegradeBucket
	Nodes       []DegradeNode
	Accounts    []DegradeAccount
	AccountPage DegradeAccountPage
	Events      []DegradeEventView
}

type DegradeTotals struct {
	Hits         int64
	Accounts     int64
	StillEnabled int64
	Disabled     int64
	Deleted      int64
	Hard         int64
	Soft         int64
	Burst        int64
	MaxTPS       float64
}

type DegradeAccountPage struct {
	Page     int
	PageSize int
	Total    int64
	HasMore  bool
}

type DegradeBucket struct {
	Label  string `json:"label"`
	Count  int64  `json:"count"`
	Severe int64  `json:"severe"`
}

type DegradeNode struct {
	Name     string  `json:"name"`
	Hits     int64   `json:"hits"`
	Accounts int64   `json:"accounts"`
	MaxTPS   float64 `json:"maxTPS"`
}

type DegradeAccount struct {
	ID      uint64
	Name    string
	Email   string
	Hits    int64
	MaxTPS  float64
	Classes map[string]int64
	Nodes   []string
	Last    time.Time
	Enabled bool
	Found   bool
	BFS     int
}

type DegradeEventView struct {
	ID           uint64
	RequestID    string
	AccountID    *uint64
	AccountName  string
	NodeName     string
	OutputTokens int64
	TPS          float64
	Class        string
	CreatedAt    time.Time
	Model        string
}

type degradeBucketSpec struct {
	Range repository.DegradeBucketRange
	Label string
}

func (s *Service) DegradeSummary(ctx context.Context, window string, thresholds DegradeThresholds, filter DegradeAccountFilter) (DegradeSummary, error) {
	window, start, end, err := resolveDegradeWindow(window, s.now().UTC())
	if err != nil {
		return DegradeSummary{}, err
	}
	thresholds = normalizeDegradeThresholds(thresholds)
	filter = normalizeDegradeAccountFilter(filter)
	bucketSpecs := degradeBucketSpecs(window, start, end)
	bucketRanges := make([]repository.DegradeBucketRange, 0, len(bucketSpecs))
	for _, spec := range bucketSpecs {
		bucketRanges = append(bucketRanges, spec.Range)
	}
	data, err := s.audits.SummarizeDegrade(ctx, repository.DegradeSummaryQuery{
		Start: start, End: end, SoftTPS: thresholds.SoftTPS, HardTPS: thresholds.HardTPS,
		MinGenerationMS: thresholds.MinGenMS, MinOutputTokens: thresholds.MinOut, FailClosed: thresholds.FailClosed,
		AccountSearch: filter.Search, AccountStatus: filter.Status, AccountClass: filter.Class, MinHits: filter.MinHits,
		AccountOffset: (filter.Page - 1) * filter.PageSize, AccountLimit: filter.PageSize,
		Buckets: bucketRanges, RecentLimit: degradeRecentEventCap,
	})
	if err != nil {
		return DegradeSummary{}, err
	}
	return buildDegradeSummary(window, end, thresholds, filter, bucketSpecs, data), nil
}

func resolveDegradeWindow(value string, now time.Time) (string, time.Time, time.Time, error) {
	if value == "" {
		value = degradeWindow24h
	}
	var duration time.Duration
	switch value {
	case degradeWindow1h:
		duration = time.Hour
	case degradeWindow6h:
		duration = 6 * time.Hour
	case degradeWindow24h:
		duration = 24 * time.Hour
	case degradeWindow7d:
		duration = 7 * 24 * time.Hour
	default:
		return "", time.Time{}, time.Time{}, ErrInvalidPeriod
	}
	return value, now.Add(-duration), now, nil
}

func normalizeDegradeThresholds(value DegradeThresholds) DegradeThresholds {
	if value.SoftTPS <= 0 || math.IsNaN(value.SoftTPS) || math.IsInf(value.SoftTPS, 0) {
		value.SoftTPS = auditdomain.DefaultDegradeSoftTPS
	}
	if value.HardTPS <= 0 || math.IsNaN(value.HardTPS) || math.IsInf(value.HardTPS, 0) {
		value.HardTPS = auditdomain.DefaultDegradeHardTPS
	}
	if value.SoftTPS >= value.HardTPS {
		value.SoftTPS = auditdomain.DefaultDegradeSoftTPS
		value.HardTPS = auditdomain.DefaultDegradeHardTPS
	}
	if value.MinGenMS <= 0 {
		value.MinGenMS = auditdomain.DefaultDegradeMinGenMS
	}
	if value.MinOut <= 0 {
		value.MinOut = auditdomain.DefaultDegradeMinOutput
	}
	return value
}

func normalizeDegradeAccountFilter(value DegradeAccountFilter) DegradeAccountFilter {
	value.Search = strings.TrimSpace(value.Search)
	if value.Status != "enabled" && value.Status != "disabled" && value.Status != "deleted" {
		value.Status = ""
	}
	if value.Class != auditdomain.DegradeClassBurst && value.Class != auditdomain.DegradeClassSoft && value.Class != auditdomain.DegradeClassHard {
		value.Class = ""
	}
	if value.MinHits < 1 {
		value.MinHits = 1
	}
	if value.Page < 1 {
		value.Page = degradeDefaultPage
	}
	if value.PageSize < 1 || value.PageSize > degradeMaxPageSize {
		value.PageSize = degradeDefaultPageSize
	}
	return value
}

func buildDegradeSummary(window string, now time.Time, thresholds DegradeThresholds, filter DegradeAccountFilter, bucketSpecs []degradeBucketSpec, data repository.DegradeSummaryResult) DegradeSummary {
	accounts := make([]DegradeAccount, 0, len(data.Accounts))
	for _, value := range data.Accounts {
		accounts = append(accounts, DegradeAccount{
			ID: value.ID, Name: value.Name, Email: value.Email, Hits: value.Hits, MaxTPS: round(value.MaxTPS, 1),
			Classes: map[string]int64{
				auditdomain.DegradeClassBurst: value.Burst,
				auditdomain.DegradeClassSoft:  value.Soft,
				auditdomain.DegradeClassHard:  value.Hard,
			},
			Nodes: value.Nodes, Last: value.Last, Enabled: value.Enabled, Found: value.Found, BFS: value.BuildBotFlagSource,
		})
	}
	nodes := make([]DegradeNode, 0, len(data.Nodes))
	for _, value := range data.Nodes {
		nodes = append(nodes, DegradeNode{Name: value.Name, Hits: value.Hits, Accounts: value.Accounts, MaxTPS: round(value.MaxTPS, 1)})
	}
	events := make([]DegradeEventView, 0, len(data.Events))
	for _, value := range data.Events {
		nodeName := value.EgressNodeName
		if nodeName == "" {
			nodeName = "?"
		}
		events = append(events, DegradeEventView{
			ID: value.ID, RequestID: value.RequestID, AccountID: value.AccountID, AccountName: value.AccountName,
			NodeName: nodeName, OutputTokens: value.OutputTokens, TPS: round(value.TPS, 2), Class: value.Class,
			CreatedAt: value.CreatedAt, Model: value.Model,
		})
	}
	series := make([]DegradeBucket, len(bucketSpecs))
	for index, spec := range bucketSpecs {
		series[index].Label = spec.Label
	}
	for _, value := range data.Buckets {
		if value.Index >= 0 && value.Index < len(series) {
			series[value.Index].Count = value.Count
			series[value.Index].Severe = value.Severe
		}
	}
	return DegradeSummary{
		Window: window, GeneratedAt: now, Thresholds: thresholds,
		Totals: DegradeTotals{
			Hits: data.Totals.Hits, Accounts: data.Totals.Accounts, StillEnabled: data.Totals.StillEnabled,
			Disabled: data.Totals.Disabled, Deleted: data.Totals.Deleted, Hard: data.Totals.Hard,
			Soft: data.Totals.Soft, Burst: data.Totals.Burst, MaxTPS: round(data.Totals.MaxTPS, 2),
		},
		Series: series, Nodes: nodes, Accounts: accounts,
		AccountPage: DegradeAccountPage{
			Page: data.AccountOffset/filter.PageSize + 1, PageSize: filter.PageSize, Total: data.AccountTotal,
			HasMore: int64(data.AccountOffset+filter.PageSize) < data.AccountTotal,
		},
		Events: events,
	}
}

func degradeBucketSpecs(window string, start, end time.Time) []degradeBucketSpec {
	step := 5 * time.Minute
	label := func(value time.Time) string { return value.Format("15:04") }
	switch window {
	case degradeWindow7d:
		step = 2 * time.Hour
		label = func(value time.Time) string { return value.Format("01-02 15:00") }
	case degradeWindow24h:
		step = time.Hour
		label = func(value time.Time) string { return value.Format("15:00") }
	case degradeWindow6h:
		step = 20 * time.Minute
	}
	var result []degradeBucketSpec
	for cursor := start; cursor.Before(end); cursor = cursor.Add(step) {
		bucketEnd := cursor.Add(step)
		if bucketEnd.After(end) {
			bucketEnd = end
		}
		result = append(result, degradeBucketSpec{
			Range: repository.DegradeBucketRange{Start: cursor, End: bucketEnd},
			Label: label(cursor),
		})
	}
	return result
}

func round(value float64, places int) float64 {
	factor := math.Pow10(places)
	return math.Round(value*factor) / factor
}
