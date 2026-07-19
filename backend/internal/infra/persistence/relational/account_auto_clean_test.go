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

	deleted, candidates, nextAfter, err := repo.DeleteAutoCleanReauthBatch(ctx, now.Add(-time.Hour), false, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if candidates != 2 {
		t.Fatalf("candidates=%d nextAfter=%d deleted=%v", candidates, nextAfter, deleted)
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

	deleted, candidates, _, err := repo.DeleteAutoCleanReauthBatch(ctx, now.Add(-time.Hour), false, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if candidates != 1 || len(deleted) != 0 {
		t.Fatalf("candidates=%d deleted=%v (null anchor excluded, queued skipped)", candidates, deleted)
	}
	if _, err := repo.Get(ctx, nullAnchor.ID); err != nil {
		t.Fatalf("null-anchor account missing: %v", err)
	}
	if _, err := repo.Get(ctx, queued.ID); err != nil {
		t.Fatalf("queued account missing: %v", err)
	}
}
