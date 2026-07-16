package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
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

func TestOpenVideoDownloadUsesPinnedAccount(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	job := mediadomain.Job{
		ID: "video_download_1", RequestID: "req_1", ClientKeyID: 1, ClientKeyName: "client",
		AccountID: 9, AccountName: "web-account", Provider: "grok_web", Model: "grok-imagine-video",
		ModelRouteID: 1, UpstreamModel: "grok-imagine-video-upstream", Prompt: "demo", InputJSON: `{}`,
		Seconds: 8, Size: "16:9", Quality: "720p",
		Status: mediadomain.StatusCompleted, Progress: 100, UpstreamURL: "https://assets.grok.com/demo.mp4",
		ContentType: "video/mp4", CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	}
	jobs := &videoJobStub{job: job}
	service := NewService(nil, jobs, nil, nil, Config{})
	accounts := &videoAccountStub{credential: account.Credential{ID: 9, Name: "web-account"}}
	downloader := &videoDownloaderStub{open: provider.VideoAssetOpen{
		Body: io.NopCloser(strings.NewReader("video-bytes")), ContentType: "video/mp4", ContentLength: 11,
	}}
	service.ConfigureVideoDownload(accounts, downloader)

	download, err := service.OpenVideoDownload(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	if download.Filename != "video_download_1.mp4" || download.ContentType != "video/mp4" || download.ContentLength != 11 {
		t.Fatalf("download = %#v", download)
	}
	if accounts.calls != 1 || downloader.calls != 1 || downloader.lastURL != job.UpstreamURL || downloader.lastAccountID != 9 {
		t.Fatalf("accounts=%d downloader=%d url=%q account=%d", accounts.calls, downloader.calls, downloader.lastURL, downloader.lastAccountID)
	}
	raw, err := io.ReadAll(download.Body)
	if err != nil || string(raw) != "video-bytes" {
		t.Fatalf("body = %q err=%v", raw, err)
	}

	if _, err := service.OpenVideoDownload(ctx, "missing"); !errors.Is(err, ErrVideoJobNotFound) {
		t.Fatalf("missing job error = %v", err)
	}
	jobs.job.Status = mediadomain.StatusInProgress
	jobs.job.UpstreamURL = ""
	if _, err := service.OpenVideoDownload(ctx, job.ID); !errors.Is(err, ErrVideoNotDownloadable) {
		t.Fatalf("pending job error = %v", err)
	}
}

type videoJobStub struct {
	job mediadomain.Job
}

func (s *videoJobStub) CreateMediaJob(context.Context, mediadomain.Job) error { return nil }

func (s *videoJobStub) GetMediaJob(_ context.Context, id string, clientKeyID uint64) (mediadomain.Job, error) {
	if id != s.job.ID || clientKeyID != s.job.ClientKeyID {
		return mediadomain.Job{}, repository.ErrNotFound
	}
	return s.job, nil
}

func (s *videoJobStub) GetMediaJobByID(_ context.Context, id string) (mediadomain.Job, error) {
	if id != s.job.ID {
		return mediadomain.Job{}, repository.ErrNotFound
	}
	return s.job, nil
}

func (s *videoJobStub) UpdateMediaJob(_ context.Context, value mediadomain.Job) error {
	s.job = value
	return nil
}

func (s *videoJobStub) ListMediaJobs(context.Context, repository.MediaJobListQuery) ([]mediadomain.Job, int64, error) {
	return nil, 0, nil
}

func (s *videoJobStub) SummarizeMediaJobs(context.Context) (repository.MediaJobStats, error) {
	return repository.MediaJobStats{}, nil
}

func (s *videoJobStub) ListRecoverableMediaJobs(context.Context, int) ([]mediadomain.Job, error) {
	return nil, nil
}

func (s *videoJobStub) ListUnrecordedTerminalMediaJobs(context.Context, int) ([]mediadomain.Job, error) {
	return nil, nil
}

func (s *videoJobStub) TryClaimMediaJob(context.Context, string, time.Time, time.Time, string) (mediadomain.Job, bool, error) {
	return mediadomain.Job{}, false, nil
}

func (s *videoJobStub) MarkMediaJobUsageRecorded(context.Context, string, time.Time) error {
	return nil
}

type videoAccountStub struct {
	credential account.Credential
	calls      int
}

func (s *videoAccountStub) Get(_ context.Context, id uint64) (account.Credential, error) {
	s.calls++
	if id != s.credential.ID {
		return account.Credential{}, repository.ErrNotFound
	}
	return s.credential, nil
}

type videoDownloaderStub struct {
	open          provider.VideoAssetOpen
	calls         int
	lastURL       string
	lastAccountID uint64
}

func (s *videoDownloaderStub) OpenVideoAsset(_ context.Context, credential account.Credential, rawURL string) (provider.VideoAssetOpen, error) {
	s.calls++
	s.lastAccountID = credential.ID
	s.lastURL = rawURL
	return s.open, nil
}
