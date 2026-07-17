package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func TestServicePersistsAndReopensImage(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(relational.NewMediaAssetRepository(database), relational.NewMediaJobRepository(database), objects, nil, Config{
		PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
		CleanupThresholdPercent: 80, CleanupInterval: 10 * time.Minute,
	})
	raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	asset, err := service.SaveImage(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if asset.MIMEType != "image/png" || asset.SizeBytes != int64(len(raw)) || len(asset.SHA256) != 64 {
		t.Fatalf("asset = %#v", asset)
	}
	if got := service.PublicImageURL(asset.ID); got != "https://api.example/v1/media/images/"+asset.ID {
		t.Fatalf("public URL = %q", got)
	}
	stored, body, err := service.OpenImage(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || stored.ID != asset.ID || !bytes.Equal(data, raw) {
		t.Fatalf("stored=%#v size=%d err=%v", stored, len(data), err)
	}
	if _, err := service.SaveImage(ctx, []byte("not an image")); err == nil {
		t.Fatal("invalid image content was accepted")
	}
}

func TestAdminDeleteVideoJobsRemovesTerminalJobAssetAndTicket(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-video-delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountValue, _, err := relational.NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, WebTier: accountdomain.WebTierBasic,
		Name: "video-delete-account", SourceKey: "video-delete-account", EncryptedAccessToken: "encrypted-access-token", AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := relational.NewClientKeyRepository(database).Create(ctx, clientkeydomain.Key{
		Name: "video-delete-key", Prefix: "video-delete", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted-secret",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "video-delete-objects"))
	if err != nil {
		t.Fatal(err)
	}
	assets := relational.NewMediaAssetRepository(database)
	jobs := relational.NewMediaJobRepository(database)
	tickets := relational.NewMediaUploadTicketRepository(database)
	service := NewServiceWithTickets(assets, jobs, tickets, objects, nil, Config{MaxTotalBytes: 1 << 30})
	payload := append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{3}, 64)...)
	asset, err := service.SaveVideo(ctx, "", "video/mp4", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	completedAt := now.Add(time.Minute)
	job := mediadomain.Job{
		ID: "video_delete_completed", RequestID: "request-video-delete", ClientKeyID: key.ID, ClientKeyName: key.Name,
		AccountID: accountValue.ID, AccountName: accountValue.Name, Provider: string(accountdomain.ProviderWeb),
		Model: "grok-imagine-video", ModelRouteID: 1, UpstreamModel: "grok-imagine-video", Prompt: "delete me",
		Seconds: 6, Size: "16:9", Quality: "720p", Status: mediadomain.StatusCompleted, Progress: 100,
		InputJSON: `{}`, ResultAssetID: asset.ID, ContentType: asset.MIMEType, CreatedAt: now, UpdatedAt: completedAt, CompletedAt: &completedAt,
	}
	if err := jobs.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	tokenHash := strings.Repeat("b", 64)
	if err := tickets.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: tokenHash, AssetID: asset.ID, JobID: job.ID, MaxBytes: DefaultMaxVideoBytes,
		AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), ConsumedAt: &now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := service.AdminDeleteVideoJobs(ctx, []string{job.ID})
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if values, err := jobs.GetMediaJobsByIDs(ctx, []string{job.ID}); err != nil || len(values) != 0 {
		t.Fatalf("remaining jobs=%#v err=%v", values, err)
	}
	if _, err := assets.GetMediaAsset(ctx, asset.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("asset error=%v, want not found", err)
	}
	if body, err := objects.Open(ctx, asset.StorageKey); !errors.Is(err, os.ErrNotExist) {
		if body != nil {
			_ = body.Close()
		}
		t.Fatalf("object error=%v, want not exist", err)
	}
	if _, err := tickets.GetUploadTicketByHash(ctx, tokenHash); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("ticket error=%v, want not found", err)
	}
}

func TestCleanupDeletesOldestAssetsAtThreshold(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	repository := relational.NewMediaAssetRepository(database)
	raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	now := time.Now().UTC()
	ids := []string{"img_cleanup_0000000000000001", "img_cleanup_0000000000000002", "img_cleanup_0000000000000003", "img_cleanup_0000000000000004"}
	for index, id := range ids {
		key, err := objects.SaveImage(ctx, id, "image/png", raw)
		if err != nil {
			t.Fatal(err)
		}
		createdAt := now.Add(time.Duration(index-4) * time.Hour)
		if index == len(ids)-1 {
			createdAt = now
		}
		if err := repository.CreateMediaAsset(ctx, mediadomain.Asset{
			ID: id, Kind: "image", StorageKey: key, MIMEType: "image/png", SizeBytes: int64(len(raw)),
			SHA256: strings.Repeat("a", 64), CreatedAt: createdAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(repository, relational.NewMediaJobRepository(database), objects, nil, Config{
		PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20,
		MaxTotalBytes: int64(len(raw) * 2), CleanupThresholdPercent: 50,
		CleanupInterval: 10 * time.Minute,
	})
	deleted, err := service.Cleanup(ctx)
	if err != nil || deleted != 3 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	total, err := repository.TotalMediaAssetBytes(ctx)
	if err != nil || total != int64(len(raw)) {
		t.Fatalf("remaining bytes=%d err=%v", total, err)
	}
	if _, _, err := service.OpenImage(ctx, ids[0]); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("oldest asset still exists: %v", err)
	}
	if _, body, err := service.OpenImage(ctx, ids[3]); err != nil {
		t.Fatalf("recent asset was deleted: %v", err)
	} else {
		_ = body.Close()
	}
}

func TestCleanupPagesPastProtectedAssetsAndDeletesLaterUnprotected(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-cleanup-protected.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects-protected"))
	if err != nil {
		t.Fatal(err)
	}
	assetRepo := relational.NewMediaAssetRepository(database)
	ticketRepo := relational.NewMediaUploadTicketRepository(database)
	raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	now := time.Now().UTC()
	// 超过 cleanupAssetBatchSize(200) 的受保护前缀 + 1 个可删资产。
	const protectedCount = cleanupAssetBatchSize + 1
	for i := 0; i < protectedCount; i++ {
		id := fmt.Sprintf("img_prot_%04d_aaaaaaaaaa", i)
		key, err := objects.SaveImage(ctx, id, "image/png", raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := assetRepo.CreateMediaAsset(ctx, mediadomain.Asset{
			ID: id, Kind: "image", StorageKey: key, MIMEType: "image/png", SizeBytes: int64(len(raw)),
			SHA256: strings.Repeat("b", 64), CreatedAt: now.Add(time.Duration(i-protectedCount-1) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("prot-ticket-%d", i)))
		if err := ticketRepo.CreateUploadTicket(ctx, repository.MediaUploadTicket{
			TokenHash: hex.EncodeToString(sum[:]), AssetID: id, JobID: fmt.Sprintf("job_prot_%d", i),
			MaxBytes: 1024, AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	deletableID := "img_free_00000000000001"
	key, err := objects.SaveImage(ctx, deletableID, "image/png", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := assetRepo.CreateMediaAsset(ctx, mediadomain.Asset{
		ID: deletableID, Kind: "image", StorageKey: key, MIMEType: "image/png", SizeBytes: int64(len(raw)),
		SHA256: strings.Repeat("c", 64), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 阈值极低：强制触发清理；受保护资产不可删，应通过 offset 扫到可删项。
	service := NewServiceWithTickets(assetRepo, relational.NewMediaJobRepository(database), ticketRepo, objects, nil, Config{
		PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20,
		MaxTotalBytes: int64(len(raw)), CleanupThresholdPercent: 50, CleanupInterval: time.Minute,
	})
	deleted, err := service.Cleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1 (only unprotected asset)", deleted)
	}
	if _, err := assetRepo.GetMediaAsset(ctx, deletableID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("deletable asset still present: %v", err)
	}
	if _, err := assetRepo.GetMediaAsset(ctx, fmt.Sprintf("img_prot_%04d_aaaaaaaaaa", 0)); err != nil {
		t.Fatalf("protected oldest was deleted: %v", err)
	}
	if _, err := assetRepo.GetMediaAsset(ctx, fmt.Sprintf("img_prot_%04d_aaaaaaaaaa", protectedCount-1)); err != nil {
		t.Fatalf("protected near-end was deleted: %v", err)
	}
}

func TestCleanupAllProtectedTerminatesWithoutDelete(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-cleanup-all-prot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects-all-prot"))
	if err != nil {
		t.Fatal(err)
	}
	assetRepo := relational.NewMediaAssetRepository(database)
	ticketRepo := relational.NewMediaUploadTicketRepository(database)
	raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("img_allp_%04d_aaaaaaaa", i)
		key, err := objects.SaveImage(ctx, id, "image/png", raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := assetRepo.CreateMediaAsset(ctx, mediadomain.Asset{
			ID: id, Kind: "image", StorageKey: key, MIMEType: "image/png", SizeBytes: int64(len(raw)),
			SHA256: strings.Repeat("d", 64), CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("all-prot-%d", i)))
		if err := ticketRepo.CreateUploadTicket(ctx, repository.MediaUploadTicket{
			TokenHash: hex.EncodeToString(sum[:]), AssetID: id, JobID: fmt.Sprintf("job_allp_%d", i),
			MaxBytes: 1024, AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := NewServiceWithTickets(assetRepo, relational.NewMediaJobRepository(database), ticketRepo, objects, nil, Config{
		PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20,
		MaxTotalBytes: int64(len(raw)), CleanupThresholdPercent: 10, CleanupInterval: time.Minute,
	})
	// 若仍无限循环，此用例会挂起失败。
	done := make(chan struct{})
	var deleted int
	var cleanupErr error
	go func() {
		deleted, cleanupErr = service.Cleanup(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not terminate when all assets are protected")
	}
	if cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if deleted != 0 {
		t.Fatalf("deleted=%d, want 0", deleted)
	}
	total, err := assetRepo.TotalMediaAssetBytes(ctx)
	if err != nil || total != int64(len(raw)*3) {
		t.Fatalf("total=%d err=%v", total, err)
	}
}

func TestCleanupPrunesExpiredUploadTickets(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-cleanup-tickets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects-tickets"))
	if err != nil {
		t.Fatal(err)
	}
	ticketRepo := relational.NewMediaUploadTicketRepository(database)
	now := time.Now().UTC()
	expiredSum := sha256.Sum256([]byte("expired-token"))
	activeSum := sha256.Sum256([]byte("active-token"))
	expiredHash := hex.EncodeToString(expiredSum[:])
	activeHash := hex.EncodeToString(activeSum[:])
	if err := ticketRepo.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: expiredHash, AssetID: "vid_expired_00000001", JobID: "job_expired",
		MaxBytes: 1024, AllowedMIME: "video/mp4", ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ticketRepo.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: activeHash, AssetID: "vid_active_0000000001", JobID: "job_active",
		MaxBytes: 1024, AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	service := NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		ticketRepo, objects, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	if _, err := service.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ticketRepo.GetUploadTicketByHash(ctx, expiredHash); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("expired ticket should be pruned: %v", err)
	}
	active, err := ticketRepo.GetUploadTicketByHash(ctx, activeHash)
	if err != nil {
		t.Fatalf("active ticket pruned: %v", err)
	}
	if !active.ExpiresAt.After(now) {
		t.Fatalf("active ticket corrupted: %#v", active)
	}
}

func TestCleanupCapsExpiredTicketPruningPerInvocation(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-cleanup-ticket-cap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects-ticket-cap"))
	if err != nil {
		t.Fatal(err)
	}
	ticketRepo := relational.NewMediaUploadTicketRepository(database)
	now := time.Now().UTC()
	// 超过单次调用上限：maxBatches * batchSize + 额外过期票据。
	expiredCount := cleanupTicketBatchSize*cleanupTicketMaxBatchesPerRun + 50
	expiredHashes := make([]string, 0, expiredCount)
	for i := 0; i < expiredCount; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("expired-cap-token-%d", i)))
		hash := hex.EncodeToString(sum[:])
		expiredHashes = append(expiredHashes, hash)
		if err := ticketRepo.CreateUploadTicket(ctx, repository.MediaUploadTicket{
			TokenHash: hash, AssetID: fmt.Sprintf("vid_exp_cap_%04d", i), JobID: fmt.Sprintf("job_exp_cap_%d", i),
			MaxBytes: 1024, AllowedMIME: "video/mp4", ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	activeSum := sha256.Sum256([]byte("active-cap-token"))
	activeHash := hex.EncodeToString(activeSum[:])
	if err := ticketRepo.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: activeHash, AssetID: "vid_active_cap_0000001", JobID: "job_active_cap",
		MaxBytes: 1024, AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	service := NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		ticketRepo, objects, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)

	if _, err := service.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	// 活跃票据必须保留。
	if _, err := ticketRepo.GetUploadTicketByHash(ctx, activeHash); err != nil {
		t.Fatalf("active ticket must survive cleanup: %v", err)
	}
	// 第一次调用最多删除 cap 条，剩余过期票据仍在。
	remainingAfterFirst := 0
	for _, hash := range expiredHashes {
		if _, err := ticketRepo.GetUploadTicketByHash(ctx, hash); err == nil {
			remainingAfterFirst++
		} else if !errors.Is(err, repository.ErrNotFound) {
			t.Fatal(err)
		}
	}
	capPerRun := cleanupTicketBatchSize * cleanupTicketMaxBatchesPerRun
	wantRemaining := expiredCount - capPerRun
	if remainingAfterFirst != wantRemaining {
		t.Fatalf("remaining expired after first cleanup = %d, want %d (cap=%d total=%d)", remainingAfterFirst, wantRemaining, capPerRun, expiredCount)
	}

	// 后续调用继续回收剩余过期票据。
	if _, err := service.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	remainingAfterSecond := 0
	for _, hash := range expiredHashes {
		if _, err := ticketRepo.GetUploadTicketByHash(ctx, hash); err == nil {
			remainingAfterSecond++
		} else if !errors.Is(err, repository.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if remainingAfterSecond != 0 {
		t.Fatalf("remaining expired after second cleanup = %d, want 0", remainingAfterSecond)
	}
	if _, err := ticketRepo.GetUploadTicketByHash(ctx, activeHash); err != nil {
		t.Fatalf("active ticket must still exist: %v", err)
	}
}

func TestCleanupPreservesMetadataWhenLocalObjectIsMissing(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	repository := relational.NewMediaAssetRepository(database)
	raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	id := "img_missing_0000000000000001"
	key, err := objects.SaveImage(ctx, id, "image/png", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateMediaAsset(ctx, mediadomain.Asset{ID: id, Kind: "image", StorageKey: key, MIMEType: "image/png", SizeBytes: int64(len(raw)), SHA256: strings.Repeat("a", 64), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := objects.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, relational.NewMediaJobRepository(database), objects, nil, Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: int64(len(raw)), CleanupThresholdPercent: 50, CleanupInterval: 10 * time.Minute})
	if _, err := service.Cleanup(ctx); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup error = %v", err)
	}
	if _, err := repository.GetMediaAsset(ctx, id); err != nil {
		t.Fatalf("shared metadata was deleted: %v", err)
	}
}

func TestPublicImageURLUsesHotReloadedBase(t *testing.T) {
	service := NewService(nil, nil, nil, nil, Config{PublicBaseURL: "https://config.example/base/"})
	if got := service.PublicImageURL("img_demo"); got != "https://config.example/base/v1/media/images/img_demo" {
		t.Fatalf("configured URL = %q", got)
	}
	updated := service.runtimeConfig()
	updated.PublicBaseURL = "https://runtime.example/api/"
	service.UpdateConfig(updated)
	if got := service.PublicImageURL("img_demo"); got != "https://runtime.example/api/v1/media/images/img_demo" {
		t.Fatalf("hot-reloaded URL = %q", got)
	}
}
