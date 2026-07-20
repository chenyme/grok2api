package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func TestDeleteAutoCleanReauthBatchSkipsActiveMediaJobs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "auto-clean-media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(database)

	blocked, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "blocked", SourceKey: "blocked",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-2 * time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	free, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "free", SourceKey: "free",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-2 * time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}

	key := clientKeyModel{Name: "auto-clean-key", Prefix: "auto-clean-key", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	accountID := blocked.ID
	job := mediaJobModel{
		ID: "media_job_auto_clean_block", RequestID: "req_auto_clean_block",
		ClientKeyID: key.ID, ClientKeyName: "key", AccountID: &accountID, AccountName: "blocked",
		EgressScope: "grok_build", EgressMode: "direct", Provider: string(accountdomain.ProviderBuild),
		Model: "video", ModelRouteID: 1, UpstreamModel: "video", Prompt: "x", Seconds: 1, Size: "16:9",
		Quality: "720p", Status: string(media.StatusInProgress), Progress: 10, InputJSON: "{}",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := database.db.WithContext(ctx).Create(&job).Error; err != nil {
		t.Fatal(err)
	}

	candidates, err := repo.ListAutoCleanReauthCandidates(ctx, now.Add(-time.Hour), false, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.DeleteAutoCleanReauthCandidates(ctx, now.Add(-time.Hour), false, []uint64{blocked.ID, free.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0] != free.ID {
		t.Fatalf("candidates=%d deleted=%v", len(candidates), deleted)
	}
	if len(deleted) != 1 || deleted[0] != free.ID {
		t.Fatalf("deleted=%v want only free=%d", deleted, free.ID)
	}
	if _, err := repo.Get(ctx, blocked.ID); err != nil {
		t.Fatalf("blocked account should remain: %v", err)
	}
	if _, err := repo.Get(ctx, free.ID); err == nil {
		t.Fatal("free account should be deleted")
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func TestDeleteAutoCleanSkipsNullAnchorAndQueuedMedia(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 21, 0, 0, 0, time.UTC)
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "auto-clean-null.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(database)

	// 显式清空 reauth_marked_at：历史脏数据不得被删。
	nullAnchor, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "null-anchor", SourceKey: "null-anchor",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-2 * time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Model(&accountModel{}).Where("id = ?", nullAnchor.ID).Update("reauth_marked_at", nil).Error; err != nil {
		t.Fatal(err)
	}

	queued, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "queued", SourceKey: "queued",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-2 * time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "auto-clean-key-q", Prefix: "auto-clean-key-q", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	qid := queued.ID
	job := mediaJobModel{
		ID: "media_job_auto_clean_queued", RequestID: "req_auto_clean_queued",
		ClientKeyID: key.ID, ClientKeyName: "key", AccountID: &qid, AccountName: "queued",
		EgressScope: "grok_build", EgressMode: "direct", Provider: string(accountdomain.ProviderBuild),
		Model: "video", ModelRouteID: 1, UpstreamModel: "video", Prompt: "x", Seconds: 1, Size: "16:9",
		Quality: "720p", Status: string(media.StatusQueued), Progress: 0, InputJSON: "{}",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := database.db.WithContext(ctx).Create(&job).Error; err != nil {
		t.Fatal(err)
	}

	candidates, err := repo.ListAutoCleanReauthCandidates(ctx, now.Add(-time.Hour), false, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.DeleteAutoCleanReauthCandidates(ctx, now.Add(-time.Hour), false, []uint64{queued.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 || len(deleted) != 0 {
		t.Fatalf("candidates=%d deleted=%v (null anchor excluded, queued skipped)", len(candidates), deleted)
	}
	if _, err := repo.Get(ctx, nullAnchor.ID); err != nil {
		t.Fatalf("null-anchor account missing: %v", err)
	}
	if _, err := repo.Get(ctx, queued.ID); err != nil {
		t.Fatalf("queued account missing: %v", err)
	}
}

func TestDeleteAutoCleanRevalidatesStatusAfterListing(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 21, 1, 0, 0, 0, time.UTC)
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "auto-clean-revalidate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(database)
	value, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "recovered", SourceKey: "recovered",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-2 * time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := repo.ListAutoCleanReauthCandidates(ctx, now.Add(-time.Hour), false, 0, 100)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%v err=%v", candidates, err)
	}
	value.AuthStatus = accountdomain.AuthStatusActive
	if _, err := repo.Update(ctx, value); err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.DeleteAutoCleanReauthCandidates(ctx, now.Add(-time.Hour), false, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("recovered account deleted: %v", deleted)
	}
	if refreshed, err := repo.Get(ctx, value.ID); err != nil || refreshed.AuthStatus != accountdomain.AuthStatusActive || refreshed.ReauthMarkedAt != nil {
		t.Fatalf("recovered account=%#v err=%v", refreshed, err)
	}
}

func TestReauthMarkedAtMigrationBackfillsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "auto-clean-migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(database)
	markedAt := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	reauth, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "legacy-reauth", SourceKey: "legacy-reauth",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
	})
	if err != nil {
		t.Fatal(err)
	}
	active, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "legacy-active", SourceKey: "legacy-active",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Model(&accountModel{}).Where("id = ?", reauth.ID).Update("updated_at", markedAt).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Exec("DROP INDEX IF EXISTS idx_accounts_auto_clean_reauth").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Exec("DROP INDEX IF EXISTS idx_accounts_auto_clean_reauth_cursor").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Migrator().DropColumn(&accountModel{}, "ReauthMarkedAt"); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
	reauthAfter, err := repo.Get(ctx, reauth.ID)
	if err != nil {
		t.Fatal(err)
	}
	activeAfter, err := repo.Get(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reauthAfter.ReauthMarkedAt == nil || !reauthAfter.ReauthMarkedAt.Equal(markedAt) {
		t.Fatalf("reauth anchor=%v want=%s", reauthAfter.ReauthMarkedAt, markedAt)
	}
	if activeAfter.ReauthMarkedAt != nil {
		t.Fatalf("active anchor=%v", activeAfter.ReauthMarkedAt)
	}
}
