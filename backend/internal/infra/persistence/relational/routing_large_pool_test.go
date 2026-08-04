package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestRoutingLargeSQLiteAccountPoolDoesNotExceedVariableLimit(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	const accountCount = 32767

	if err := database.db.WithContext(ctx).Exec(`
		WITH RECURSIVE seq(id) AS (
			SELECT 1 UNION ALL SELECT id + 1 FROM seq WHERE id < ?
		)
		INSERT INTO provider_accounts (
			id, identity_key, provider, name, email, user_id, team_id, source_key,
			enabled, auth_status, priority, max_concurrent, minimum_remaining,
			failure_count, last_error, observed_model, build_api_fallback,
			build_route_mode, build_super_entitled, egress_assignment_mode,
			created_at, updated_at
		)
		SELECT id, printf('%064x', id), 'grok_console', 'account-' || id, '', '', '',
			'source-' || id, 1, 'active', 1, 8, 0, 0, '', '', 0, 'auto', 0, '',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		FROM seq`, accountCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Exec(`
		INSERT INTO account_credentials (
			account_id, auth_type, client_id, encrypted_primary, encrypted_refresh,
			encrypted_cloudflare_cookie, refresh_failures, last_refresh_error_status,
			last_refresh_error, last_refresh_error_message, last_refresh_error_response,
			refresh_permanent, updated_at
		)
		SELECT id, 'oauth', '', 'token', '', '', 0, 0, '', '', '', 0, CURRENT_TIMESTAMP
		FROM provider_accounts`).Error; err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := database.db.WithContext(ctx).Create(&billingModel{
		AccountID: accountCount, PlanCode: "paid", MonthlyLimit: 100, Used: 12, HistoryJSON: "[]", SyncedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&quotaRecoveryModel{
		AccountID: accountCount, Kind: string(account.QuotaRecoveryKindPaid), Status: string(account.QuotaRecoveryStatusExhausted), UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&quotaWindowModel{
		AccountID: accountCount, Mode: "monthly", Remaining: 7, Total: 10, UsagePercent: 30,
		BreakdownJSON: "[]", WindowSeconds: 3600, Source: string(account.QuotaSourceUpstream), UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	repository := NewAccountRepository(database)
	enabled, err := repository.ListEnabled(ctx, account.ProviderConsole)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != accountCount || enabled[len(enabled)-1].EncryptedAccessToken != "token" {
		t.Fatalf("enabled account load = %d rows, last credential hydrated = %t", len(enabled), len(enabled) > 0 && enabled[len(enabled)-1].EncryptedAccessToken == "token")
	}

	bases, err := repository.ListRoutingAccountBases(ctx, account.ProviderConsole, "monthly")
	if err != nil {
		t.Fatal(err)
	}
	if len(bases) != accountCount {
		t.Fatalf("routing bases = %d, want %d", len(bases), accountCount)
	}
	last := bases[len(bases)-1]
	if last.Credential.ID != accountCount || last.Credential.AuthType != account.AuthTypeOAuth {
		t.Fatalf("last routing credential = %#v", last.Credential)
	}
	if last.Credential.EncryptedAccessToken != "" {
		t.Fatal("routing projection unexpectedly loaded credential secret")
	}
	if last.Billing == nil || last.Billing.PlanCode != "paid" {
		t.Fatalf("last routing billing = %#v", last.Billing)
	}
	if last.QuotaRecovery == nil || last.QuotaRecovery.Status != account.QuotaRecoveryStatusExhausted {
		t.Fatalf("last routing recovery = %#v", last.QuotaRecovery)
	}
	if last.QuotaWindow == nil || last.QuotaWindow.Remaining != 7 {
		t.Fatalf("last routing quota window = %#v", last.QuotaWindow)
	}

	candidates, err := repository.ListRoutingCandidates(ctx, account.ProviderConsole, 0, "grok-test", "monthly")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != accountCount {
		t.Fatalf("routing candidates = %d, want %d", len(candidates), accountCount)
	}
}
