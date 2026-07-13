package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestConsoleQuotaParticipatesInRoutingAndSummary(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAccountRepository(database)
	credential, _, err := repository.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "console", SourceKey: "console:test",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	resetAt := now.Add(time.Hour)
	if err := repository.SaveQuotaWindows(ctx, credential.ID, "", now, []account.QuotaWindow{{
		AccountID: credential.ID, Mode: "console", Remaining: 20, Total: 20, WindowSeconds: 3600,
		ResetAt: &resetAt, Source: account.QuotaSourceDefault, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	var profileCount int64
	if err := database.db.WithContext(ctx).Model(&webAccountProfileModel{}).Where("account_id = ?", credential.ID).Count(&profileCount).Error; err != nil {
		t.Fatal(err)
	}
	if profileCount != 0 {
		t.Fatalf("console created %d web profiles", profileCount)
	}
	candidates, err := repository.ListRoutingCandidates(ctx, account.ProviderConsole, "grok-4.3", "console")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].QuotaWindow == nil || candidates[0].QuotaWindow.Remaining != 20 {
		t.Fatalf("candidates = %#v", candidates)
	}
	summary, err := repository.Summarize(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary) != 1 || summary[0].Available != 1 || summary[0].WaitingReset != 0 {
		t.Fatalf("summary before exhaustion = %#v", summary)
	}
	if err := repository.ExhaustQuotaWindow(ctx, credential.ID, "console", &resetAt, now); err != nil {
		t.Fatal(err)
	}
	summary, err = repository.Summarize(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary) != 1 || summary[0].Available != 0 || summary[0].WaitingReset != 1 {
		t.Fatalf("summary after exhaustion = %#v", summary)
	}
}
