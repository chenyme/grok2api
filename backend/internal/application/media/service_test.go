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

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
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
