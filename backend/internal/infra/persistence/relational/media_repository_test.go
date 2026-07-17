package relational

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestMediaJobRepositoryListMediaJobsPaginatesAndFilters(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)

	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider:             accountdomain.ProviderWeb,
		AuthType:             accountdomain.AuthTypeSSO,
		WebTier:              accountdomain.WebTierBasic,
		Name:                 "media-list-account",
		SourceKey:            "media-list-account",
		EncryptedAccessToken: testEncryptedToken,
		AuthStatus:           accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "media-list-key", Prefix: "media-list-key", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}

	jobRepo := NewMediaJobRepository(database)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	jobs := []mediadomain.Job{
		testMediaJob("media_job_completed_old", accountValue.ID, key.ID, mediadomain.StatusCompleted, now.Add(-4*time.Hour)),
		testMediaJob("media_job_queued_mid", accountValue.ID, key.ID, mediadomain.StatusQueued, now.Add(-3*time.Hour)),
		testMediaJob("media_job_failed_newer", accountValue.ID, key.ID, mediadomain.StatusFailed, now.Add(-2*time.Hour)),
		testMediaJob("media_job_completed_new", accountValue.ID, key.ID, mediadomain.StatusCompleted, now.Add(-time.Hour)),
	}
	jobs[0].Prompt = "A quiet harbor"
	jobs[1].Prompt = "Northern lights"
	jobs[2].Prompt = "Desert sunrise"
	jobs[3].Prompt = "City skyline"
	for _, job := range jobs {
		if err := jobRepo.CreateMediaJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	firstPage, total, err := jobRepo.ListMediaJobs(ctx, repository.MediaJobListQuery{
		Page: repository.PageQuery{Offset: 0, Limit: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("total = %d", total)
	}
	assertMediaJobIDs(t, firstPage, "media_job_completed_new", "media_job_failed_newer")

	secondPage, total, err := jobRepo.ListMediaJobs(ctx, repository.MediaJobListQuery{
		Page: repository.PageQuery{Offset: 2, Limit: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("second page total = %d", total)
	}
	assertMediaJobIDs(t, secondPage, "media_job_queued_mid", "media_job_completed_old")

	completed, total, err := jobRepo.ListMediaJobs(ctx, repository.MediaJobListQuery{
		Page:   repository.PageQuery{Offset: 0, Limit: 10},
		Filter: repository.MediaJobListFilter{Status: string(mediadomain.StatusCompleted)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("completed total = %d", total)
	}
	assertMediaJobIDs(t, completed, "media_job_completed_new", "media_job_completed_old")

	searched, total, err := jobRepo.ListMediaJobs(ctx, repository.MediaJobListQuery{
		Page: repository.PageQuery{Offset: 0, Limit: 1, Search: "northern"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("searched total = %d", total)
	}
	assertMediaJobIDs(t, searched, "media_job_queued_mid")

	sorted, total, err := jobRepo.ListMediaJobs(ctx, repository.MediaJobListQuery{
		Page: repository.PageQuery{
			Offset: 0,
			Limit:  4,
			Sort:   repository.SortQuery{Field: "prompt", Direction: repository.SortAscending},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("sorted total = %d", total)
	}
	assertMediaJobIDs(t, sorted, "media_job_completed_old", "media_job_completed_new", "media_job_failed_newer", "media_job_queued_mid")

	stats, err := jobRepo.SummarizeMediaJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalJobs != 4 || stats.Completed != 2 || stats.Failed != 1 || stats.InProgress != 0 || stats.Queued != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestMediaAssetRepositoryListMediaAssetsPaginatesAndCounts(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	assetRepo := NewMediaAssetRepository(database)

	stats, err := assetRepo.SummarizeMediaAssets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalImages != 0 || stats.TotalBytes != 0 {
		t.Fatalf("initial stats = %#v", stats)
	}

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	assets := []mediadomain.Asset{
		testMediaAsset("media_asset_0001", "media/asset-0001.png", now.Add(-3*time.Hour)),
		testMediaAsset("media_asset_0002", "media/asset-0002.png", now.Add(-2*time.Hour)),
		testMediaAsset("media_asset_0003", "media/asset-0003.png", now.Add(-time.Hour)),
	}
	for _, asset := range assets {
		if err := assetRepo.CreateMediaAsset(ctx, asset); err != nil {
			t.Fatal(err)
		}
	}

	firstPage, total, err := assetRepo.ListMediaAssets(ctx, repository.MediaAssetListQuery{
		Page: repository.PageQuery{Offset: 0, Limit: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("total = %d", total)
	}
	assertMediaAssetIDs(t, firstPage, "media_asset_0003", "media_asset_0002")

	secondPage, total, err := assetRepo.ListMediaAssets(ctx, repository.MediaAssetListQuery{
		Page: repository.PageQuery{Offset: 2, Limit: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("second page total = %d", total)
	}
	assertMediaAssetIDs(t, secondPage, "media_asset_0001")

	searched, total, err := assetRepo.ListMediaAssets(ctx, repository.MediaAssetListQuery{
		Page: repository.PageQuery{Offset: 0, Limit: 1, Search: "0001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("searched total = %d", total)
	}
	assertMediaAssetIDs(t, searched, "media_asset_0001")

	stats, err = assetRepo.SummarizeMediaAssets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalImages != 3 || stats.TotalBytes != 3*1024 {
		t.Fatalf("stats = %#v", stats)
	}
}

func testMediaJob(id string, accountID, clientKeyID uint64, status mediadomain.Status, createdAt time.Time) mediadomain.Job {
	job := mediadomain.Job{
		ID:            id,
		RequestID:     "request-" + id,
		ClientKeyID:   clientKeyID,
		ClientKeyName: "media-list-key",
		AccountID:     accountID,
		AccountName:   "media-list-account",
		Provider:      "grok_web",
		Model:         "grok-imagine-video",
		ModelRouteID:  1,
		UpstreamModel: "grok-imagine-video-upstream",
		Prompt:        "test prompt",
		Seconds:       8,
		Size:          "16:9",
		Quality:       "720p",
		Status:        status,
		InputJSON:     `{}`,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	}
	if status == mediadomain.StatusCompleted || status == mediadomain.StatusFailed {
		job.Progress = 100
		completedAt := createdAt.Add(time.Minute)
		job.CompletedAt = &completedAt
	}
	return job
}

func TestMediaUploadTicketRepositoryDeleteUploadTicketByHash(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewMediaUploadTicketRepository(database)
	now := time.Now().UTC()
	hash := strings.Repeat("ab", 32)
	otherHash := strings.Repeat("cd", 32)
	if err := repo.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: hash, AssetID: "vid_delete_by_hash_1", JobID: "job_delete_by_hash",
		MaxBytes: 1024, AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: otherHash, AssetID: "vid_delete_by_hash_2", JobID: "job_delete_keep",
		MaxBytes: 1024, AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteUploadTicketByHash(ctx, hash); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.GetUploadTicketByHash(ctx, hash); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("deleted ticket still readable: %v", err)
	}
	// 精确删除不得影响其他票据。
	if _, err := repo.GetUploadTicketByHash(ctx, otherHash); err != nil {
		t.Fatalf("other ticket removed: %v", err)
	}
	// 幂等：再次删除缺失行成功。
	if err := repo.DeleteUploadTicketByHash(ctx, hash); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if err := repo.DeleteUploadTicketByHash(ctx, ""); err != nil {
		t.Fatalf("empty hash delete: %v", err)
	}
}

func TestMediaUploadTicketRepositoryConsumeRollsBackWhenTicketReadFails(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewMediaUploadTicketRepository(database)
	now := time.Now().UTC()
	hash := strings.Repeat("ef", 32)
	if err := repo.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: hash, AssetID: "vid_consume_rollback_1", JobID: "job_consume_rollback",
		MaxBytes: 1024, AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected ticket read failure")
	callbackName := "test:fail_consumed_ticket_read"
	failed := false
	if err := database.db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if !failed && tx.Statement != nil && tx.Statement.Table == "media_upload_tickets" {
			failed = true
			_ = tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}

	_, consumed, err := repo.ConsumeUploadTicket(ctx, hash, now)
	if removeErr := database.db.Callback().Query().Remove(callbackName); removeErr != nil {
		t.Fatal(removeErr)
	}
	if !errors.Is(err, injected) || consumed {
		t.Fatalf("consume = (%v, %v), want injected error and consumed=false", consumed, err)
	}
	if !failed {
		t.Fatal("ticket read failure callback did not run")
	}
	ticket, err := repo.GetUploadTicketByHash(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.ConsumedAt != nil {
		t.Fatalf("consume transaction was not rolled back: consumed_at=%v", ticket.ConsumedAt)
	}
}

func testMediaAsset(id, storageKey string, createdAt time.Time) mediadomain.Asset {
	return mediadomain.Asset{
		ID:         id,
		Kind:       "image",
		StorageKey: storageKey,
		MIMEType:   "image/png",
		SizeBytes:  1024,
		SHA256:     strings.Repeat("a", 64),
		CreatedAt:  createdAt,
	}
}

func assertMediaJobIDs(t *testing.T, values []mediadomain.Job, expected ...string) {
	t.Helper()
	if len(values) != len(expected) {
		t.Fatalf("len(values) = %d, expected %d: %#v", len(values), len(expected), values)
	}
	for index, id := range expected {
		if values[index].ID != id {
			t.Fatalf("values[%d].ID = %q, expected %q; values = %#v", index, values[index].ID, id, values)
		}
	}
}

func assertMediaAssetIDs(t *testing.T, values []mediadomain.Asset, expected ...string) {
	t.Helper()
	if len(values) != len(expected) {
		t.Fatalf("len(values) = %d, expected %d: %#v", len(values), len(expected), values)
	}
	for index, id := range expected {
		if values[index].ID != id {
			t.Fatalf("values[%d].ID = %q, expected %q; values = %#v", index, values[index].ID, id, values)
		}
	}
}
