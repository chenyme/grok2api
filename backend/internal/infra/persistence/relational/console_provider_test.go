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
	if err := repository.SaveQuotaWindows(ctx, credential.ID, "", now, []account.QuotaWindow{
		{
			AccountID: credential.ID, Mode: "console", Remaining: 20, Total: 20, WindowSeconds: 3600,
			ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream, UpdatedAt: now,
		},
		{AccountID: credential.ID, Mode: "console_image", Remaining: 0, Total: 5, SyncedAt: &now, Source: account.QuotaSourceUpstream, UpdatedAt: now},
		{AccountID: credential.ID, Mode: "console_video", Remaining: 2, Total: 2, SyncedAt: &now, Source: account.QuotaSourceUpstream, UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	if complete, err := repository.HasQuotaWindows(ctx, credential.ID); err != nil || !complete {
		t.Fatalf("complete Console snapshot = %v, err = %v", complete, err)
	}
	var profileCount int64
	if err := database.db.WithContext(ctx).Model(&webAccountProfileModel{}).Where("account_id = ?", credential.ID).Count(&profileCount).Error; err != nil {
		t.Fatal(err)
	}
	if profileCount != 0 {
		t.Fatalf("console created %d web profiles", profileCount)
	}
	candidates, err := repository.ListRoutingCandidates(ctx, account.ProviderConsole, 0, "grok-4.3", "console")
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

func TestHasQuotaWindowsRejectsLegacyConsoleSnapshot(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAccountRepository(database)
	credential, _, err := repository.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "legacy", SourceKey: "console:legacy",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repository.SaveQuotaWindows(ctx, credential.ID, "", now, []account.QuotaWindow{{
		AccountID: credential.ID, Mode: "console", Remaining: 20, Total: 20, WindowSeconds: 3600,
		Source: account.QuotaSourceDefault, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	complete, err := repository.HasQuotaWindows(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Fatal("legacy synthetic Console quota must require /usage migration")
	}
}
