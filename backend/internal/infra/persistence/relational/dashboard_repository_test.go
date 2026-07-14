package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
	rows := []any{
		active,
		exhausted,
		&accountModel{IdentityKey: testIdentityKey("disabled"), Provider: "grok_build", Name: "disabled", SourceKey: "disabled", Enabled: false, AuthStatus: "active", MaxConcurrent: 1},
		enabledRoute,
		&modelRouteModel{PublicID: "disabled", Provider: "grok_build", UpstreamModel: "disabled", Capability: "responses", Enabled: false},
		&clientKeyModel{Name: "active", Prefix: "gkp_active", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true},
		&clientKeyModel{Name: "expired", Prefix: "gkp_expired", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, ExpiresAt: timePointer(now.Add(-time.Hour))},
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
	audits := []requestAuditModel{
		{RequestID: "success-1", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-primary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, TotalTokens: 100, CreatedAt: now.Add(-23 * time.Hour)},
		{RequestID: "success-2", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-secondary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 201, TotalTokens: 50, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "failed", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-primary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 500, TotalTokens: 10, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "outside", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, TotalTokens: 999, CreatedAt: now.Add(-25 * time.Hour)},
	}
	for index := range audits {
		if err := database.db.WithContext(ctx).Create(&audits[index]).Error; err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardBoundaries(now.Add(-24*time.Hour), 2*time.Hour, 12), now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Resources.ActiveAccounts != 1 || snapshot.Resources.TotalAccounts != 3 || snapshot.Resources.EnabledModels != 1 || snapshot.Resources.TotalModels != 2 || snapshot.Resources.ActiveClientKeys != 1 || snapshot.Resources.TotalClientKeys != 2 || snapshot.Resources.AllTimeRequests != 4 {
		t.Fatalf("resources = %#v", snapshot.Resources)
	}
	if snapshot.Usage.Requests != 3 || snapshot.Usage.SuccessfulRequests != 2 || snapshot.Usage.FailedRequests != 1 || snapshot.Usage.Tokens != 160 {
		t.Fatalf("usage = %#v", snapshot.Usage)
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
	if len(snapshot.TopModels) != 2 || snapshot.TopModels[0].Model != "grok-primary" || snapshot.TopModels[0].Requests != 2 || snapshot.TopModels[0].Tokens != 110 {
		t.Fatalf("top models = %#v", snapshot.TopModels)
	}
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
		{RequestID: "fallback", ClientKeyID: 1, ModelRouteID: 1, ModelUpstreamModel: "grok-fallback", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, TotalTokens: 200, CreatedAt: now.Add(-time.Hour)},
	}
	if err := database.db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardBoundaries(now.Add(-24*time.Hour), time.Hour, 24), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.TopModels) != 2 || snapshot.TopModels[0].Model != "grok-primary" || snapshot.TopModels[0].Requests != 2 || snapshot.TopModels[0].InputTokens != 110 || snapshot.TopModels[0].CachedInputTokens != 25 || snapshot.TopModels[0].OutputTokens != 40 || snapshot.TopModels[0].ReasoningTokens != 15 || snapshot.TopModels[0].Tokens != 150 || snapshot.TopModels[0].BilledCostUSDTicks != 3_000_000_000 || snapshot.TopModels[1].Model != "grok-fallback" {
		t.Fatalf("top models = %#v", snapshot.TopModels)
	}
	var modelBucketTokens int64
	var modelBucketCost int64
	for _, bucket := range snapshot.ModelBuckets {
		modelBucketTokens += bucket.Tokens
		modelBucketCost += bucket.BilledCostUSDTicks
	}
	if len(snapshot.ModelBuckets) != 3 || modelBucketTokens != 350 || modelBucketCost != 3_000_000_000 {
		t.Fatalf("model buckets = %#v", snapshot.ModelBuckets)
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
