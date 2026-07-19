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
