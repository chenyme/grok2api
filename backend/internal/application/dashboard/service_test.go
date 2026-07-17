package dashboard

import (
	"context"
	"testing"
	"time"

	dashboarddomain "github.com/chenyme/grok2api/backend/internal/domain/dashboard"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestGetBuildsStableBucketsAndEnrichedData(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 34, 56, 0, time.UTC)
	repository := &dashboardRepositoryStub{aggregate: dashboarddomain.Aggregate{
		Usage:           dashboarddomain.Usage{Requests: 4, SuccessfulRequests: 3, FailedRequests: 1, Tokens: 120},
		Buckets:         []dashboarddomain.Bucket{{Index: 0, Requests: 1, Tokens: 20}, {Index: 23, Requests: 3, Tokens: 100}},
		ActivityBuckets: []dashboarddomain.ActivityBucket{{Index: 179, Requests: 4}},
		TopModels:       []dashboarddomain.ModelUsage{{Model: "grok-test", Requests: 4, Tokens: 120}},
		Providers:       []dashboarddomain.ProviderUsage{{Provider: "grok_build", Requests: 4, SuccessfulRequests: 3, Tokens: 120}},
	}}
	service := NewService(repository)
	service.now = func() time.Time { return now }

	result, err := service.Get(context.Background(), "24h", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if result.Period != Period24Hours || len(result.Series) != 24 {
		t.Fatalf("result = %#v", result)
	}
	if result.Series[0].Requests != 1 || result.Series[23].Tokens != 100 {
		t.Fatalf("series = %#v", result.Series)
	}
	if len(result.TopModels) != 1 || result.TopModels[0].Model != "grok-test" || result.TopModels[0].Requests != 4 {
		t.Fatalf("top models = %#v", result.TopModels)
	}
	if len(result.Activity) != 180 || result.Activity[179].Requests != 4 || len(result.Providers) != 1 {
		t.Fatalf("enriched dashboard = %#v", result)
	}
	if !result.Range.Start.Equal(time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC)) || !result.Range.End.Equal(time.Date(2026, 7, 11, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("range = %#v", result.Range)
	}
	if !result.GeneratedAt.Equal(now) || !result.Series[23].Start.Equal(time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)) || !result.Series[23].End.Equal(time.Date(2026, 7, 11, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("current hour = %#v", result.Series[23])
	}
}

func TestGetUsesPeriodSpecificBucketCounts(t *testing.T) {
	for period, expected := range map[string]int{"24h": 24, "7d": 7, "30d": 30, "90d": 13} {
		repository := &dashboardRepositoryStub{}
		service := NewService(repository)
		if _, err := service.Get(context.Background(), period, "UTC"); err != nil {
			t.Fatalf("period %s: %v", period, err)
		}
		if repository.bucketCount != expected {
			t.Fatalf("period %s bucket count = %d, want %d", period, repository.bucketCount, expected)
		}
	}
}

func TestGetCachesRepeatedAggregate(t *testing.T) {
	repository := &dashboardRepositoryStub{}
	service := NewService(repository)
	service.now = func() time.Time { return time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC) }

	if _, err := service.Get(context.Background(), "24h", "UTC"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), "24h", "UTC"); err != nil {
		t.Fatal(err)
	}
	if repository.calls != 1 {
		t.Fatalf("snapshot calls = %d", repository.calls)
	}
}

func TestRefreshBypassesAggregateCache(t *testing.T) {
	repository := &dashboardRepositoryStub{}
	service := NewService(repository)
	service.now = func() time.Time { return time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC) }

	if _, err := service.Get(context.Background(), "24h", "UTC"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(context.Background(), "24h", "UTC"); err != nil {
		t.Fatal(err)
	}
	if repository.calls != 2 {
		t.Fatalf("snapshot calls = %d", repository.calls)
	}
}

func TestGetAlignsDailyBucketsToCalendarDays(t *testing.T) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Date(2026, 7, 11, 12, 34, 56, 0, location)
	repository := &dashboardRepositoryStub{}
	service := NewService(repository)
	service.now = func() time.Time { return now }

	result, err := service.Get(context.Background(), "7d", "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	wantStart := time.Date(2026, 7, 5, 0, 0, 0, 0, location).UTC()
	wantEnd := time.Date(2026, 7, 12, 0, 0, 0, 0, location).UTC()
	if !result.Range.Start.Equal(wantStart) || !result.Range.End.Equal(wantEnd) {
		t.Fatalf("range = %#v", result.Range)
	}
	if !result.Series[6].Start.Equal(time.Date(2026, 7, 11, 0, 0, 0, 0, location).UTC()) || !result.Series[6].End.Equal(wantEnd) {
		t.Fatalf("today bucket = %#v", result.Series[6])
	}
	if len(result.Activity) != 180 || !result.Activity[179].Start.Equal(time.Date(2026, 7, 11, 0, 0, 0, 0, location).UTC()) {
		t.Fatalf("activity range = %#v", result.Activity)
	}
}

func TestGetUsesWeeklyBucketsFor90Days(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 34, 56, 0, time.UTC)
	service := NewService(&dashboardRepositoryStub{})
	service.now = func() time.Time { return now }

	result, err := service.Get(context.Background(), "90d", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	for index, bucket := range result.Series[:len(result.Series)-1] {
		if bucket.End.Sub(bucket.Start) != 7*24*time.Hour {
			t.Fatalf("bucket %d duration = %s", index, bucket.End.Sub(bucket.Start))
		}
	}
	last := result.Series[len(result.Series)-1]
	if last.End.Sub(last.Start) != 6*24*time.Hour || !last.End.Equal(result.Range.End) {
		t.Fatalf("last bucket = %#v, range = %#v", last, result.Range)
	}
}

func TestGetRejectsUnknownPeriod(t *testing.T) {
	service := NewService(&dashboardRepositoryStub{})
	if _, err := service.Get(context.Background(), "365d", "UTC"); err != ErrInvalidPeriod {
		t.Fatalf("err = %v", err)
	}
}

func TestGetUsesCalendarBoundariesAcrossDST(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	repository := &dashboardRepositoryStub{}
	service := NewService(repository)
	service.now = func() time.Time { return now }

	result, err := service.Get(context.Background(), "7d", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	var foundShortDay bool
	for _, bucket := range result.Series {
		if bucket.End.Sub(bucket.Start) == 23*time.Hour {
			foundShortDay = true
		}
	}
	if !foundShortDay {
		t.Fatalf("DST transition was not represented: %#v", result.Series)
	}
	foundShortDay = false
	for index := 1; index < len(repository.window.ActivityBoundaries); index++ {
		if repository.window.ActivityBoundaries[index].Sub(repository.window.ActivityBoundaries[index-1]) == 23*time.Hour {
			foundShortDay = true
			break
		}
	}
	if !foundShortDay {
		t.Fatalf("DST transition was not represented in activity boundaries")
	}
}

func TestGetRejectsInvalidTimezone(t *testing.T) {
	service := NewService(&dashboardRepositoryStub{})
	if _, err := service.Get(context.Background(), "24h", "Mars/Olympus"); err != ErrInvalidTimezone {
		t.Fatalf("err = %v", err)
	}
}

type dashboardRepositoryStub struct {
	aggregate   dashboarddomain.Aggregate
	bucketCount int
	calls       int
	window      repository.DashboardSnapshotWindow
}

func (s *dashboardRepositoryStub) Snapshot(_ context.Context, window repository.DashboardSnapshotWindow, _ time.Time) (dashboarddomain.Aggregate, error) {
	s.calls++
	s.bucketCount = len(window.BucketBoundaries) - 1
	s.window = window
	return s.aggregate, nil
}
