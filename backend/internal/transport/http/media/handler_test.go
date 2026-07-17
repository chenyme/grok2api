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
	"strconv"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
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

func TestAdminVideoListHidesCompletedErrorsAndExposesPreview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-admin-list.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects-list"))
	if err != nil {
		t.Fatal(err)
	}
	assets := relational.NewMediaAssetRepository(database)
	jobs := relational.NewMediaJobRepository(database)
	accountID, clientKeyID := seedAdminMediaOwners(t, database, "list")
	service := mediaapp.NewService(assets, jobs, objects, nil, mediaapp.Config{
		PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
		CleanupThresholdPercent: 80, CleanupInterval: time.Minute,
	})

	videoPayload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{0x02}, 32)...)
	videoID := "vid_http_list_0001"
	storageKey, err := objects.SaveVideo(ctx, videoID, "video/mp4", videoPayload)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 4, 5, 6, 0, time.UTC)
	if err := assets.CreateMediaAsset(ctx, mediaAssetForTest(videoID, storageKey, "video", "video/mp4", int64(len(videoPayload)), now)); err != nil {
		t.Fatal(err)
	}
	completedAt := now.Add(time.Hour)
	if err := jobs.CreateMediaJob(ctx, mediaJobForTest("job_completed_dirty", accountID, clientKeyID, "completed", videoID, "当前账号池不支持该模型", now, &completedAt)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CreateMediaJob(ctx, mediaJobForTest("job_failed_real", accountID, clientKeyID, "failed", "", "upstream disconnected", now.Add(time.Minute), &completedAt)); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	NewHandler(service).RegisterAdmin(router.Group("/api/admin/v1"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/videos?page=1&pageSize=20", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"previewAvailable":true`)) {
		t.Fatalf("expected previewAvailable true, body=%s", body)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"errorMessage":"upstream disconnected"`)) {
		t.Fatalf("failed job error missing, body=%s", body)
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("当前账号池不支持该模型")) {
		t.Fatalf("completed dirty error leaked, body=%s", body)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"createdAt":"2026-07-13T04:05:06Z"`)) &&
		!bytes.Contains(recorder.Body.Bytes(), []byte(`"createdAt":"2026-07-13T04:06:06Z"`)) {
		t.Fatalf("expected RFC3339 UTC timestamps, body=%s", body)
	}
}

func TestAdminVideoContentEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-admin-content.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects-content"))
	if err != nil {
		t.Fatal(err)
	}
	assets := relational.NewMediaAssetRepository(database)
	jobs := relational.NewMediaJobRepository(database)
	accountID, clientKeyID := seedAdminMediaOwners(t, database, "content")
	service := mediaapp.NewService(assets, jobs, objects, nil, mediaapp.Config{
		PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
		CleanupThresholdPercent: 80, CleanupInterval: time.Minute,
	})

	videoPayload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{0x03}, 48)...)
	videoID := "vid_http_content_0001"
	storageKey, err := objects.SaveVideo(ctx, videoID, "video/mp4", videoPayload)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := assets.CreateMediaAsset(ctx, mediaAssetForTest(videoID, storageKey, "video", "video/mp4", int64(len(videoPayload)), now)); err != nil {
		t.Fatal(err)
	}
	completedAt := now.Add(time.Minute)
	if err := jobs.CreateMediaJob(ctx, mediaJobForTest("job_content_ok", accountID, clientKeyID, "completed", videoID, "", now, &completedAt)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CreateMediaJob(ctx, mediaJobForTest("job_content_missing_asset", accountID, clientKeyID, "completed", "vid_gone_asset_01", "", now, &completedAt)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CreateMediaJob(ctx, mediaJobForTest("job_content_failed", accountID, clientKeyID, "failed", "", "boom", now, &completedAt)); err != nil {
		t.Fatal(err)
	}
	// 非视频资产绑定不得被预览。
	imageID := "img_http_content_0001"
	rawPNG, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	imageKey, err := objects.SaveImage(ctx, imageID, "image/png", rawPNG)
	if err != nil {
		t.Fatal(err)
	}
	if err := assets.CreateMediaAsset(ctx, mediaAssetForTest(imageID, imageKey, "image", "image/png", int64(len(rawPNG)), now)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CreateMediaJob(ctx, mediaJobForTest("job_content_image_asset", accountID, clientKeyID, "completed", imageID, "", now, &completedAt)); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	NewHandler(service).RegisterAdmin(router.Group("/api/admin/v1"))

	ok := httptest.NewRecorder()
	router.ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/videos/job_content_ok/content", nil))
	if ok.Code != http.StatusOK || ok.Header().Get("Content-Type") != "video/mp4" ||
		ok.Header().Get("Content-Disposition") != "inline" ||
		ok.Header().Get("Cache-Control") != "private, no-store" ||
		ok.Header().Get("X-Content-Type-Options") != "nosniff" ||
		ok.Body.Len() != len(videoPayload) {
		t.Fatalf("ok response status=%d headers=%#v size=%d", ok.Code, ok.Header(), ok.Body.Len())
	}
	if ok.Header().Get("Content-Length") != strconv.Itoa(len(videoPayload)) {
		t.Fatalf("content-length = %q", ok.Header().Get("Content-Length"))
	}

	for _, path := range []string{
		"/api/admin/v1/media/videos/job_missing/content",
		"/api/admin/v1/media/videos/job_content_missing_asset/content",
		"/api/admin/v1/media/videos/job_content_failed/content",
		"/api/admin/v1/media/videos/job_content_image_asset/content",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d body=%s", path, recorder.Code, recorder.Body.String())
		}
		if bytes.Contains(recorder.Body.Bytes(), []byte("videos/")) || bytes.Contains(recorder.Body.Bytes(), []byte("http")) {
			t.Fatalf("404 body leaked path/url: %s", recorder.Body.String())
		}
	}

	// 底层对象缺失：元数据仍在时内容端点也必须 404。
	if err := objects.Delete(ctx, storageKey); err != nil {
		t.Fatal(err)
	}
	missingObject := httptest.NewRecorder()
	router.ServeHTTP(missingObject, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/videos/job_content_ok/content", nil))
	if missingObject.Code != http.StatusNotFound {
		t.Fatalf("missing object status = %d body=%s", missingObject.Code, missingObject.Body.String())
	}
}

func mediaJobForTest(id string, accountID, clientKeyID uint64, status, resultAssetID, errorMessage string, createdAt time.Time, completedAt *time.Time) mediadomain.Job {
	job := mediadomain.Job{
		ID: id, RequestID: "request-" + id, ClientKeyID: clientKeyID, ClientKeyName: "key",
		AccountID: accountID, AccountName: "acct", Provider: "grok_web", Model: "grok-imagine-video",
		ModelRouteID: 1, UpstreamModel: "video", Prompt: "prompt " + id, Seconds: 6, Size: "16:9", Quality: "720p",
		Status: mediadomain.Status(status), Progress: 100, InputJSON: `{}`, ResultAssetID: resultAssetID,
		ErrorMessage: errorMessage, CreatedAt: createdAt, UpdatedAt: createdAt, CompletedAt: completedAt,
	}
	if errorMessage != "" {
		job.ErrorCode = "test_error"
	}
	return job
}

func mediaAssetForTest(id, storageKey, kind, mime string, size int64, createdAt time.Time) mediadomain.Asset {
	return mediadomain.Asset{
		ID: id, Kind: kind, StorageKey: storageKey, MIMEType: mime, SizeBytes: size,
		SHA256: hex.EncodeToString(sha256.New().Sum(nil)), CreatedAt: createdAt,
	}
}

func seedAdminMediaOwners(t *testing.T, database *relational.Database, suffix string) (accountID, clientKeyID uint64) {
	t.Helper()
	ctx := context.Background()
	accountValue, _, err := relational.NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider:             accountdomain.ProviderWeb,
		AuthType:             accountdomain.AuthTypeSSO,
		WebTier:              accountdomain.WebTierBasic,
		Name:                 "media-admin-" + suffix,
		SourceKey:            "media-admin-" + suffix,
		EncryptedAccessToken: "encrypted-token",
		AuthStatus:           accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := relational.NewClientKeyRepository(database).Create(ctx, clientkeydomain.Key{
		Name: "media-admin-key-" + suffix, Prefix: "media-admin-key-" + suffix,
		SecretHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		EncryptedSecret: "encrypted-secret", Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	return accountValue.ID, key.ID
}
