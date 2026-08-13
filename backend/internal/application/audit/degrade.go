package audit

import (
	"context"
	"math"
	"sort"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	degradeWindow1h       = "1h"
	degradeWindow6h       = "6h"
	degradeWindow24h      = "24h"
	degradeWindow7d       = "7d"
	degradeEventLimit     = 20_000
	degradeRecentEventCap = 80
)

type DegradeThresholds struct {
	SoftTPS  float64
	HardTPS  float64
	MinGenMS int64
	MinOut   int64
}

type DegradeSummary struct {
	Window      string
	GeneratedAt time.Time
	Thresholds  DegradeThresholds
	Totals      DegradeTotals
	Series      []DegradeBucket
	Nodes       []DegradeNode
	Accounts    []DegradeAccount
	Events      []DegradeEventView
}

type DegradeTotals struct {
	Hits         int
	Accounts     int
	StillEnabled int
	Disabled     int
	Hard         int
	Soft         int
	Burst        int
	MaxTPS       float64
}

type DegradeBucket struct {
	Label  string `json:"label"`
	Count  int    `json:"count"`
	Severe int    `json:"severe"`
}

type DegradeNode struct {
	Name     string  `json:"name"`
	Hits     int     `json:"hits"`
	Accounts int     `json:"accounts"`
	MaxTPS   float64 `json:"maxTPS"`
}

type DegradeAccount struct {
	ID      uint64
	Name    string
	Email   string
	Hits    int
	MaxTPS  float64
	Classes map[string]int
	Nodes   []string
	Last    time.Time
	Enabled bool
	BFS     int
	Found   bool
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

func (s *Service) DegradeSummary(ctx context.Context, window string, thresholds DegradeThresholds) (DegradeSummary, error) {
	window, start, end, err := resolveDegradeWindow(window, s.now().UTC())
	if err != nil {
		return DegradeSummary{}, err
	}
	thresholds = normalizeDegradeThresholds(thresholds)
	events, err := s.audits.ListDegradeEvents(ctx, repository.DegradeEventQuery{
		Start: start, End: end, MinOutputTokens: thresholds.MinOut, Limit: degradeEventLimit,
	})
	if err != nil {
		return DegradeSummary{}, err
	}
	return buildDegradeSummary(window, end, thresholds, events), nil
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

func buildDegradeSummary(window string, now time.Time, thresholds DegradeThresholds, events []repository.DegradeEvent) DegradeSummary {
	type accAgg struct {
		hits    int
		maxTPS  float64
		classes map[string]int
		nodes   map[string]struct{}
		last    time.Time
		name    string
		email   string
		enabled bool
		found   bool
		bfs     int
	}
	type nodeAgg struct {
		hits     int
		accounts map[uint64]struct{}
		maxTPS   float64
	}
	accounts := map[uint64]*accAgg{}
	nodes := map[string]*nodeAgg{}
	classCounts := map[string]int{}
	var classified []DegradeEventView
	var maxTPS float64

	for _, event := range events {
		class, tps, _ := auditdomain.ClassifyOutputSpeed(event.OutputTokens, event.FirstTokenMS, event.DurationMS, thresholds.SoftTPS, thresholds.HardTPS, thresholds.MinGenMS)
		if class == "" {
			continue
		}
		classCounts[class]++
		if tps > maxTPS {
			maxTPS = tps
		}
		nodeName := event.EgressNodeName
		if nodeName == "" {
			nodeName = "?"
		}
		classified = append(classified, DegradeEventView{
			ID: event.ID, RequestID: event.RequestID, AccountID: event.AccountID, AccountName: event.AccountName,
			NodeName: nodeName, OutputTokens: event.OutputTokens, TPS: math.Round(tps*100) / 100, Class: class,
			CreatedAt: event.CreatedAt, Model: event.Model,
		})
		if event.AccountID == nil || *event.AccountID == 0 {
			continue
		}
		id := *event.AccountID
		rec := accounts[id]
		if rec == nil {
			rec = &accAgg{classes: map[string]int{}, nodes: map[string]struct{}{}}
			accounts[id] = rec
		}
		rec.hits++
		if tps > rec.maxTPS {
			rec.maxTPS = tps
		}
		rec.classes[class]++
		rec.nodes[nodeName] = struct{}{}
		if event.CreatedAt.After(rec.last) {
			rec.last = event.CreatedAt
			rec.name = event.AccountName
		}
		if rec.name == "" {
			rec.name = event.AccountName
		}
		if event.Email != "" {
			rec.email = event.Email
		}
		if event.Enabled != nil {
			rec.enabled = *event.Enabled
			rec.found = true
		}
		if event.BuildBotFlagSource > rec.bfs {
			rec.bfs = event.BuildBotFlagSource
		}
		nd := nodes[nodeName]
		if nd == nil {
			nd = &nodeAgg{accounts: map[uint64]struct{}{}}
			nodes[nodeName] = nd
		}
		nd.hits++
		nd.accounts[id] = struct{}{}
		if tps > nd.maxTPS {
			nd.maxTPS = tps
		}
	}

	accountViews := make([]DegradeAccount, 0, len(accounts))
	enabled, disabled := 0, 0
	for id, rec := range accounts {
		if rec.found && rec.enabled {
			enabled++
		} else {
			disabled++
		}
		nodeNames := make([]string, 0, len(rec.nodes))
		for name := range rec.nodes {
			nodeNames = append(nodeNames, name)
		}
		sort.Strings(nodeNames)
		email := rec.email
		if email == "" {
			email = rec.name
		}
		accountViews = append(accountViews, DegradeAccount{
			ID: id, Name: rec.name, Email: email, Hits: rec.hits, MaxTPS: math.Round(rec.maxTPS*10) / 10,
			Classes: rec.classes, Nodes: nodeNames, Last: rec.last, Enabled: rec.found && rec.enabled, BFS: rec.bfs, Found: rec.found,
		})
	}
	sort.Slice(accountViews, func(i, j int) bool {
		if accountViews[i].Hits != accountViews[j].Hits {
			return accountViews[i].Hits > accountViews[j].Hits
		}
		return accountViews[i].MaxTPS > accountViews[j].MaxTPS
	})

	nodeViews := make([]DegradeNode, 0, len(nodes))
	for name, rec := range nodes {
		nodeViews = append(nodeViews, DegradeNode{Name: name, Hits: rec.hits, Accounts: len(rec.accounts), MaxTPS: math.Round(rec.maxTPS*10) / 10})
	}
	sort.Slice(nodeViews, func(i, j int) bool { return nodeViews[i].Hits > nodeViews[j].Hits })

	recent := classified
	if len(recent) > degradeRecentEventCap {
		recent = recent[:degradeRecentEventCap]
	}

	return DegradeSummary{
		Window: window, GeneratedAt: now, Thresholds: thresholds,
		Totals: DegradeTotals{
			Hits: len(classified), Accounts: len(accounts), StillEnabled: enabled, Disabled: disabled,
			Hard: classCounts[auditdomain.DegradeClassHard], Soft: classCounts[auditdomain.DegradeClassSoft],
			Burst: classCounts[auditdomain.DegradeClassBurst], MaxTPS: maxTPS,
		},
		Series: bucketDegradeSeries(classified, window, now),
		Nodes:  nodeViews, Accounts: accountViews, Events: recent,
	}
}

func bucketDegradeSeries(events []DegradeEventView, window string, now time.Time) []DegradeBucket {
	var step time.Duration
	var start time.Time
	var label func(time.Time) string
	switch window {
	case degradeWindow7d:
		step = 2 * time.Hour
		start = now.Add(-7 * 24 * time.Hour)
		label = func(t time.Time) string { return t.Format("01-02 15:00") }
	case degradeWindow24h:
		step = time.Hour
		start = now.Add(-24 * time.Hour)
		label = func(t time.Time) string { return t.Format("15:00") }
	case degradeWindow6h:
		step = 20 * time.Minute
		start = now.Add(-6 * time.Hour)
		label = func(t time.Time) string { return t.Format("15:04") }
	default:
		step = 5 * time.Minute
		start = now.Add(-time.Hour)
		label = func(t time.Time) string { return t.Format("15:04") }
	}
	var buckets []time.Time
	for cursor := start; cursor.Before(now); cursor = cursor.Add(step) {
		buckets = append(buckets, cursor)
	}
	if len(buckets) == 0 {
		return nil
	}
	counts := make([]int, len(buckets))
	severe := make([]int, len(buckets))
	stepSeconds := step.Seconds()
	for _, event := range events {
		idx := int(event.CreatedAt.Sub(start).Seconds() / stepSeconds)
		if idx < 0 || idx >= len(counts) {
			continue
		}
		counts[idx]++
		if event.Class == auditdomain.DegradeClassHard || event.Class == auditdomain.DegradeClassBurst {
			severe[idx]++
		}
	}
	out := make([]DegradeBucket, len(buckets))
	for i, bucket := range buckets {
		out[i] = DegradeBucket{Label: label(bucket), Count: counts[i], Severe: severe[i]}
	}
	return out
}
