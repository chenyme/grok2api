package relational

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func BenchmarkRoutingAccountBaseProjectionWithLargePayloads(b *testing.B) {
	const accountCount = 300
	b.StopTimer()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(b.TempDir(), "routing-payload-benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	repository := NewAccountRepository(database)
	secret := strings.Repeat("s", 32<<10)
	credentials := make([]account.Credential, accountCount)
	for index := range credentials {
		credentials[index] = account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO,
			Name: fmt.Sprintf("payload-%04d", index), SourceKey: fmt.Sprintf("payload-source-%04d", index),
			EncryptedAccessToken: secret, EncryptedCloudflareCookie: secret,
			Enabled: true, AuthStatus: account.AuthStatusActive,
		}
	}
	created, err := repository.UpsertManyByIdentity(ctx, credentials)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC()
	history := make([]account.BillingHistoryEntry, 256)
	period := strings.Repeat("p", 64)
	for index := range history {
		history[index] = account.BillingHistoryEntry{Year: 2026, Month: index%12 + 1, PeriodType: period, PeriodStart: period, PeriodEnd: period}
	}
	breakdown := make([]account.QuotaBreakdown, 128)
	for index := range breakdown {
		breakdown[index] = account.QuotaBreakdown{ProductCode: index % 7, UsagePercent: 50}
	}
	resetAt := now.Add(time.Hour)
	for _, credential := range created {
		if err := repository.SaveBilling(ctx, account.Billing{AccountID: credential.ID, PlanName: "SuperGrok", MonthlyLimit: 100, History: history, SyncedAt: now}); err != nil {
			b.Fatal(err)
		}
		if err := repository.SaveQuotaWindows(ctx, credential.ID, account.WebTierSuper, now, []account.QuotaWindow{{
			AccountID: credential.ID, Mode: "weekly", Remaining: 10, Total: 20,
			Breakdown: breakdown, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream,
		}}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(accountCount, "accounts")
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for b.Loop() {
		values, loadErr := repository.ListRoutingAccountBases(ctx, account.ProviderWeb, "")
		if loadErr != nil {
			b.Fatal(loadErr)
		}
		if len(values) != accountCount {
			b.Fatalf("routing bases = %d, want %d", len(values), accountCount)
		}
	}
}

func BenchmarkSelectedCredentialHydration(b *testing.B) {
	b.StopTimer()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(b.TempDir(), "credential-hydration-benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	repository := NewAccountRepository(database)
	created, _, err := repository.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
		Name: "selected", SourceKey: "selected", OIDCClientID: "client-id",
		EncryptedAccessToken: strings.Repeat("a", 32<<10), EncryptedRefreshToken: strings.Repeat("r", 32<<10),
		Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for b.Loop() {
		material, loadErr := repository.GetCredentialMaterial(ctx, created.ID, account.ProviderBuild)
		if loadErr != nil {
			b.Fatal(loadErr)
		}
		if len(material.EncryptedAccessToken) != 32<<10 {
			b.Fatalf("access token bytes = %d", len(material.EncryptedAccessToken))
		}
	}
}
