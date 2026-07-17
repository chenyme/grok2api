package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

func TestPublicImageSupportsGetHeadAndETag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-http.db"))
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
	service := mediaapp.NewService(relational.NewMediaAssetRepository(database), relational.NewMediaJobRepository(database), objects, nil, mediaapp.Config{
		PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
		CleanupThresholdPercent: 80, CleanupInterval: 10 * time.Minute,
	})
	raw, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	asset, err := service.SaveImage(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewHandler(service).RegisterPublic(router)
	path := "/v1/media/images/" + asset.ID

	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, path, nil))
	if get.Code != http.StatusOK || get.Header().Get("Content-Type") != "image/png" || get.Body.Len() != len(raw) || get.Header().Get("ETag") == "" {
		t.Fatalf("GET status=%d headers=%#v size=%d", get.Code, get.Header(), get.Body.Len())
	}
	head := httptest.NewRecorder()
	router.ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD status=%d headers=%#v size=%d", head.Code, head.Header(), head.Body.Len())
	}
	notModifiedRequest := httptest.NewRequest(http.MethodGet, path, nil)
	notModifiedRequest.Header.Set("If-None-Match", get.Header().Get("ETag"))
	notModified := httptest.NewRecorder()
	router.ServeHTTP(notModified, notModifiedRequest)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("conditional GET status=%d size=%d", notModified.Code, notModified.Body.Len())
	}
}

func TestPutVideoUploadReturns413WhenBodyTooLarge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-upload-413.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects-413"))
	if err != nil {
		t.Fatal(err)
	}
	tickets := relational.NewMediaUploadTicketRepository(database)
	service := mediaapp.NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		tickets, objects, nil,
		mediaapp.Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	tokenRaw := make([]byte, 32)
	for i := range tokenRaw {
		tokenRaw[i] = byte(i + 7)
	}
	token := hex.EncodeToString(tokenRaw)
	sum := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	if err := tickets.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: hex.EncodeToString(sum[:]), AssetID: "vid_http_413_00000001", JobID: "job_413",
		MaxBytes: 32, AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{0x0a}, 64)...)
	router := gin.New()
	NewHandler(service).RegisterPublic(router)
	req := httptest.NewRequest(http.MethodPut, "/v1/media/uploads/"+token, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "video/mp4")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPutVideoUploadReturns400ForInvalidMIME(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-upload-400.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects-400"))
	if err != nil {
		t.Fatal(err)
	}
	service := mediaapp.NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		relational.NewMediaUploadTicketRepository(database), objects, nil,
		mediaapp.Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	uploadURL, _, err := service.IssueVideoUpload(ctx, "job_400_mime")
	if err != nil {
		t.Fatal(err)
	}
	token := uploadURL[len("https://api.example/v1/media/uploads/"):]
	router := gin.New()
	NewHandler(service).RegisterPublic(router)
	payload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p'}, bytes.Repeat([]byte{1}, 16)...)
	req := httptest.NewRequest(http.MethodPut, "/v1/media/uploads/"+token, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "video/webm")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestAdminVideoListRejectsInvalidFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-admin-http.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service := mediaapp.NewService(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		nil,
		nil,
		mediaapp.Config{},
	)
	router := gin.New()
	NewHandler(service).RegisterAdmin(router.Group("/api/admin/v1"))

	for _, path := range []string{
		"/api/admin/v1/media/videos?status=unknown",
		"/api/admin/v1/media/videos?sortBy=input_json&sortOrder=asc",
		"/api/admin/v1/media/videos?sortBy=createdAt&sortOrder=sideways",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status = %d, body = %s", path, recorder.Code, recorder.Body.String())
		}
	}
}
