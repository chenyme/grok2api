package relational

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestDashboardRepositorySnapshot(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	active := &accountModel{IdentityKey: testIdentityKey("active"), Provider: "grok_build", Name: "active", SourceKey: "active", Enabled: true, AuthStatus: "active", MaxConcurrent: 1}
	exhausted := &accountModel{IdentityKey: testIdentityKey("exhausted"), Provider: "grok_build", Name: "exhausted", SourceKey: "exhausted", Enabled: true, AuthStatus: "active", MaxConcurrent: 1}
	enabledRoute := &modelRouteModel{PublicID: "enabled", Provider: "grok_build", UpstreamModel: "enabled", Capability: "responses", Enabled: true}
	internalKind := "quality_guard"
	rows := []any{
		active,
		exhausted,
		&accountModel{IdentityKey: testIdentityKey("disabled"), Provider: "grok_build", Name: "disabled", SourceKey: "disabled", Enabled: false, AuthStatus: "active", MaxConcurrent: 1},
		enabledRoute,
		&modelRouteModel{PublicID: "disabled", Provider: "grok_build", UpstreamModel: "disabled", Capability: "responses", Enabled: false},
		&clientKeyModel{Name: "active", Prefix: "gkp_active", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true},
		&clientKeyModel{Name: "expired", Prefix: "gkp_expired", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, ExpiresAt: timePointer(now.Add(-time.Hour))},
		&clientKeyModel{Name: "internal", Prefix: "quality-guard-internal", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, InternalKind: &internalKind, Enabled: true},
	}
	for _, row := range rows {
		if err := database.db.WithContext(ctx).Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.WithContext(ctx).Create(&accountModelCapabilityModel{AccountID: active.ID, UpstreamModel: enabledRoute.UpstreamModel}).Error; err != nil {
		t.Fatal(err)
	}
	for _, value := range []accountCredentialModel{
		{AccountID: 1, AuthType: "oauth", EncryptedPrimary: testEncryptedToken, UpdatedAt: now},
		{AccountID: 2, AuthType: "oauth", EncryptedPrimary: testEncryptedToken, UpdatedAt: now},
		{AccountID: 3, AuthType: "oauth", EncryptedPrimary: testEncryptedToken, UpdatedAt: now},
	} {
		if err := database.db.WithContext(ctx).Create(&value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.WithContext(ctx).Create(&quotaRecoveryModel{AccountID: exhausted.ID, Kind: "free", Status: "exhausted", NextProbeAt: timePointer(now.Add(24 * time.Hour)), UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	firstTokenOne := int64(100)
	firstTokenTwo := int64(300)
	audits := []requestAuditModel{
		{RequestID: "success-1", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-primary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, Streaming: true, OutputTokens: 90, TotalTokens: 100, FirstTokenMS: &firstTokenOne, DurationMS: 1100, CreatedAt: now.Add(-23 * time.Hour)},
		{RequestID: "success-2", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-secondary", Provider: "grok_web", Operation: "responses", UsageSource: "upstream", StatusCode: 201, Streaming: true, OutputTokens: 10, TotalTokens: 50, FirstTokenMS: &firstTokenTwo, DurationMS: 1300, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "failed", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-primary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 500, Streaming: true, OutputTokens: 100, TotalTokens: 10, DurationMS: 550, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "outside", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, TotalTokens: 999, CreatedAt: now.Add(-25 * time.Hour)},
	}
	for index := range audits {
		if err := database.db.WithContext(ctx).Create(&audits[index]).Error; err != nil {
			t.Fatal(err)
		}
	}

	boundaries := testDashboardBoundaries(now.Add(-24*time.Hour), 2*time.Hour, 12)
	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(boundaries), now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Resources.ActiveAccounts != 1 || snapshot.Resources.TotalAccounts != 3 || snapshot.Resources.BuildAccounts != 3 || snapshot.Resources.WebAccounts != 0 || snapshot.Resources.ConsoleAccounts != 0 || snapshot.Resources.EnabledModels != 1 || snapshot.Resources.TotalModels != 2 || snapshot.Resources.ActiveClientKeys != 1 || snapshot.Resources.TotalClientKeys != 2 {
		t.Fatalf("resources = %#v", snapshot.Resources)
	}
	if snapshot.Usage.Requests != 3 || snapshot.Usage.SuccessfulRequests != 2 || snapshot.Usage.FailedRequests != 1 || snapshot.Usage.Tokens != 160 {
		t.Fatalf("usage = %#v", snapshot.Usage)
	}
	if snapshot.Usage.FirstTokenSamples != 2 || snapshot.Usage.FirstTokenTotalMS != 400 || snapshot.Usage.ThroughputSamples != 2 || snapshot.Usage.ThroughputTokens != 100 || snapshot.Usage.GenerationTotalMS != 2000 {
		t.Fatalf("performance usage = %#v", snapshot.Usage)
	}
	var bucketRequests int64
	var bucketTokens int64
	bucketsByIndex := make(map[int]dashboardBucketSummary)
	for _, bucket := range snapshot.Buckets {
		bucketRequests += bucket.Requests
		bucketTokens += bucket.Tokens
		bucketsByIndex[bucket.Index] = dashboardBucketSummary{Requests: bucket.Requests, Tokens: bucket.Tokens}
	}
	if bucketRequests != 3 || bucketTokens != 160 {
		t.Fatalf("buckets = %#v", snapshot.Buckets)
	}
	if bucketsByIndex[0] != (dashboardBucketSummary{Requests: 1, Tokens: 100}) || bucketsByIndex[11] != (dashboardBucketSummary{Requests: 2, Tokens: 60}) {
		t.Fatalf("bucket distribution = %#v", bucketsByIndex)
	}
	if len(snapshot.TopModels) != 3 || snapshot.TopModels[0].Model != "grok-primary" || snapshot.TopModels[0].Requests != 2 || snapshot.TopModels[0].Tokens != 110 || snapshot.TopModels[2].Model != "enabled" || snapshot.TopModels[2].Requests != 0 {
		t.Fatalf("top models = %#v", snapshot.TopModels)
	}
	if len(snapshot.Providers) != 2 || snapshot.Providers[0].Provider != "grok_build" || snapshot.Providers[0].Requests != 2 || snapshot.Providers[1].Provider != "grok_web" || snapshot.Providers[1].Requests != 1 {
		t.Fatalf("providers = %#v", snapshot.Providers)
	}
	var activityRequests int64
	for _, bucket := range snapshot.ActivityBuckets {
		activityRequests += bucket.Requests
	}
	if activityRequests != 3 {
		t.Fatalf("activity buckets = %#v", snapshot.ActivityBuckets)
	}
}

func TestDashboardRepositoryCounts2xxWithErrorAsFailure(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "dashboard-stream-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	audits := []requestAuditModel{
		{RequestID: "healthy", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, CreatedAt: now.Add(-2 * time.Minute)},
		{RequestID: "stream-interrupted", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, Streaming: true, ErrorCode: "upstream_stream_interrupted", CreatedAt: now.Add(-time.Minute)},
	}
	if err := database.db.WithContext(ctx).Create(&audits).Error; err != nil {
		t.Fatal(err)
	}
	boundaries := testDashboardBoundaries(now.Add(-time.Hour), time.Hour, 2)
	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(boundaries), now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Usage.Requests != 2 || snapshot.Usage.SuccessfulRequests != 1 || snapshot.Usage.FailedRequests != 1 {
		t.Fatalf("usage = %#v", snapshot.Usage)
	}
}

func BenchmarkDashboardUsageAggregate(b *testing.B) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(b.TempDir(), "dashboard-benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	audits := make([]requestAuditModel, 50_000)
	for index := range audits {
		firstTokenMS := int64(100 + index%900)
		audits[index] = requestAuditModel{
			RequestID: fmt.Sprintf("benchmark-%d", index), ClientKeyID: 1, ModelRouteID: 1,
			Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200,
			Streaming: true, InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100,
			FirstTokenMS: &firstTokenMS, DurationMS: firstTokenMS + 2000, CreatedAt: now.Add(-time.Duration(index%3600) * time.Second),
		}
	}
	if err := database.db.WithContext(ctx).CreateInBatches(audits, 100).Error; err != nil {
		b.Fatal(err)
	}
	start := now.Add(-24 * time.Hour)
	legacySelect := "COUNT(*) AS requests, COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0) AS successful_requests, COALESCE(SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END), 0) AS failed_requests, COALESCE(SUM(input_tokens), 0) AS input_tokens, COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens, COALESCE(SUM(output_tokens), 0) AS output_tokens, COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens, COALESCE(SUM(total_tokens), 0) AS tokens, COALESCE(SUM(CASE WHEN cost_in_usd_ticks > 0 THEN cost_in_usd_ticks ELSE estimated_cost_in_usd_ticks END), 0) AS billed_cost_usd_ticks"
	b.Run("legacy", func(b *testing.B) {
		for b.Loop() {
			var usage dashboardBenchmarkUsage
			if err := database.db.WithContext(ctx).Model(&requestAuditModel{}).Select(legacySelect).Where("created_at >= ? AND created_at < ?", start, now.Add(time.Second)).Scan(&usage).Error; err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("performance", func(b *testing.B) {
		for b.Loop() {
			var usage dashboardBenchmarkUsage
			if err := database.db.WithContext(ctx).Model(&requestAuditModel{}).Select(dashboardUsageAggregateSelect).Where("created_at >= ? AND created_at < ?", start, now.Add(time.Second)).Scan(&usage).Error; err != nil {
				b.Fatal(err)
			}
		}
	})
}

type dashboardBenchmarkUsage struct {
	Requests           int64
	SuccessfulRequests int64
	FailedRequests     int64
	InputTokens        int64
	CachedInputTokens  int64
	OutputTokens       int64
	ReasoningTokens    int64
	Tokens             int64
	BilledCostUSDTicks int64
	FirstTokenSamples  int64
	FirstTokenTotalMS  int64
	ThroughputSamples  int64
	ThroughputTokens   int64
	GenerationTotalMS  int64
}

func TestDashboardRepositoryRanksTopModels(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "dashboard-top-models.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	rows := []requestAuditModel{
		{RequestID: "primary-1", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-primary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, InputTokens: 80, CachedInputTokens: 20, OutputTokens: 20, ReasoningTokens: 5, TotalTokens: 100, CostInUSDTicks: 1_000_000_000, EstimatedCostInUSDTicks: 9_000_000_000, CreatedAt: now.Add(-3 * time.Hour)},
		{RequestID: "primary-2", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-primary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, InputTokens: 30, CachedInputTokens: 5, OutputTokens: 20, ReasoningTokens: 10, TotalTokens: 50, EstimatedCostInUSDTicks: 2_000_000_000, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "fallback", ClientKeyID: 1, ModelRouteID: 1, ModelUpstreamModel: "grok-fallback", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, TotalTokens: 200, CostInUSDTicks: 4_000_000_000, CreatedAt: now.Add(-time.Hour)},
	}
	if err := database.db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	boundaries := testDashboardBoundaries(now.Add(-24*time.Hour), time.Hour, 24)
	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(boundaries), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.TopModels) != 2 || snapshot.TopModels[0].Model != "grok-fallback" || snapshot.TopModels[0].BilledCostUSDTicks != 4_000_000_000 || snapshot.TopModels[1].Model != "grok-primary" || snapshot.TopModels[1].Requests != 2 || snapshot.TopModels[1].InputTokens != 110 || snapshot.TopModels[1].CachedInputTokens != 25 || snapshot.TopModels[1].OutputTokens != 40 || snapshot.TopModels[1].ReasoningTokens != 15 || snapshot.TopModels[1].Tokens != 150 || snapshot.TopModels[1].BilledCostUSDTicks != 3_000_000_000 {
		t.Fatalf("top models = %#v", snapshot.TopModels)
	}
}

func TestDashboardRepositoryFillsTopModelsFromEnabledRoutes(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "dashboard-enabled-models.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 12; index++ {
		name := fmt.Sprintf("grok-%02d", index)
		route := modelRouteModel{PublicID: "Build/" + name, Provider: "grok_build", UpstreamModel: name, Capability: "responses", Enabled: true}
		if err := database.db.WithContext(ctx).Create(&route).Error; err != nil {
			t.Fatal(err)
		}
	}
	duplicate := modelRouteModel{PublicID: "Web/grok-00", Provider: "grok_web", UpstreamModel: "grok-00", Capability: "responses", Enabled: true}
	if err := database.db.WithContext(ctx).Create(&duplicate).Error; err != nil {
		t.Fatal(err)
	}
	disabled := modelRouteModel{PublicID: "Build/disabled", Provider: "grok_build", UpstreamModel: "disabled", Capability: "responses", Enabled: false}
	if err := database.db.WithContext(ctx).Create(&disabled).Error; err != nil {
		t.Fatal(err)
	}

	boundaries := testDashboardBoundaries(now.Add(-24*time.Hour), time.Hour, 24)
	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(boundaries), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.TopModels) != 10 {
		t.Fatalf("top models count = %d, want 10: %#v", len(snapshot.TopModels), snapshot.TopModels)
	}
	for index, item := range snapshot.TopModels {
		want := fmt.Sprintf("grok-%02d", index)
		if item.Model != want || item.Requests != 0 || item.Tokens != 0 || item.BilledCostUSDTicks != 0 {
			t.Fatalf("top model %d = %#v, want zero-usage %q", index, item, want)
		}
	}
}

type dashboardBucketSummary struct {
	Requests int64
	Tokens   int64
}

func timePointer(value time.Time) *time.Time { return &value }

func testDashboardBoundaries(start time.Time, step time.Duration, count int) []time.Time {
	values := make([]time.Time, count+1)
	for index := range values {
		values[index] = start.Add(time.Duration(index) * step)
	}
	return values
}

func testDashboardWindow(boundaries []time.Time) repository.DashboardSnapshotWindow {
	return repository.DashboardSnapshotWindow{
		BucketBoundaries:   boundaries,
		ActivityBoundaries: boundaries,
	}
}
