package relational

import (
	"context"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
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

	repository := NewMediaJobRepository(database)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	jobs := []mediadomain.Job{
		testMediaJob("media_job_completed_old", accountValue.ID, key.ID, mediadomain.StatusCompleted, now.Add(-4*time.Hour)),
		testMediaJob("media_job_queued_mid", accountValue.ID, key.ID, mediadomain.StatusQueued, now.Add(-3*time.Hour)),
		testMediaJob("media_job_failed_newer", accountValue.ID, key.ID, mediadomain.StatusFailed, now.Add(-2*time.Hour)),
		testMediaJob("media_job_completed_new", accountValue.ID, key.ID, mediadomain.StatusCompleted, now.Add(-time.Hour)),
	}
	for _, job := range jobs {
		if err := repository.CreateMediaJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	firstPage, total, err := repository.ListMediaJobs(ctx, 1, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("total = %d", total)
	}
	assertMediaJobIDs(t, firstPage, "media_job_completed_new", "media_job_failed_newer")

	secondPage, total, err := repository.ListMediaJobs(ctx, 2, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("second page total = %d", total)
	}
	assertMediaJobIDs(t, secondPage, "media_job_queued_mid", "media_job_completed_old")

	completed, total, err := repository.ListMediaJobs(ctx, 1, 10, string(mediadomain.StatusCompleted))
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("completed total = %d", total)
	}
	assertMediaJobIDs(t, completed, "media_job_completed_new", "media_job_completed_old")
}

func TestMediaAssetRepositoryListMediaAssetsPaginatesAndCounts(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repository := NewMediaAssetRepository(database)

	count, err := repository.CountMediaAssets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("initial count = %d", count)
	}

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	assets := []mediadomain.Asset{
		testMediaAsset("media_asset_0001", "media/asset-0001.png", now.Add(-3*time.Hour)),
		testMediaAsset("media_asset_0002", "media/asset-0002.png", now.Add(-2*time.Hour)),
		testMediaAsset("media_asset_0003", "media/asset-0003.png", now.Add(-time.Hour)),
	}
	for _, asset := range assets {
		if err := repository.CreateMediaAsset(ctx, asset); err != nil {
			t.Fatal(err)
		}
	}

	firstPage, total, err := repository.ListMediaAssets(ctx, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("total = %d", total)
	}
	assertMediaAssetIDs(t, firstPage, "media_asset_0003", "media_asset_0002")

	secondPage, total, err := repository.ListMediaAssets(ctx, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("second page total = %d", total)
	}
	assertMediaAssetIDs(t, secondPage, "media_asset_0001")

	count, err = repository.CountMediaAssets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("count = %d", count)
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
