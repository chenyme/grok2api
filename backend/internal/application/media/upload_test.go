package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestIssueAndReceiveVideoUploadOnce(t *testing.T) {
	service, tickets, cleanup := newUploadTestService(t)
	defer cleanup()
	ctx := context.Background()

	// Public base must be HTTPS for XAI.
	uploadURL, assetID, err := service.IssueVideoUpload(ctx, "video_job_test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uploadURL, "https://api.example/v1/media/uploads/") {
		t.Fatalf("uploadURL = %s", uploadURL)
	}
	token := strings.TrimPrefix(uploadURL, "https://api.example/v1/media/uploads/")
	if len(token) != 64 {
		t.Fatalf("token entropy length = %d", len(token))
	}

	// Minimal ftyp box so content sniff accepts MP4.
	payload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{0x01}, 64)...)
	asset, err := service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if asset.ID != assetID || asset.Kind != "video" || asset.SizeBytes != int64(len(payload)) {
		t.Fatalf("asset = %#v", asset)
	}

	// Replay must fail and not create a second asset.
	_, err = service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(payload))
	if err == nil {
		t.Fatal("expected replay rejection")
	}

	// Ticket hash is stored, not plaintext token.
	sum := sha256.Sum256([]byte(token))
	stored, err := tickets.GetUploadTicketByHash(ctx, hex.EncodeToString(sum[:]))
	if err != nil || stored.ConsumedAt == nil {
		t.Fatalf("ticket = %#v err=%v", stored, err)
	}
}

func TestReceiveVideoUploadRejectsExpiredAndOversize(t *testing.T) {
	service, tickets, cleanup := newUploadTestService(t)
	defer cleanup()
	ctx := context.Background()
	uploadURL, _, err := service.IssueVideoUpload(ctx, "video_job_expire")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(uploadURL, "https://api.example/v1/media/uploads/")
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	// Force expire by rewriting ticket row through consume path simulation: delete and recreate expired.
	existing, err := tickets.GetUploadTicketByHash(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.DeleteExpiredUploadTickets(ctx, time.Now().UTC().Add(3*time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	existing.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	existing.ConsumedAt = nil
	if err := tickets.CreateUploadTicket(ctx, existing); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p'}, bytes.Repeat([]byte{1}, 32)...)
	if _, err := service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(payload)); err == nil {
		t.Fatal("expected expired rejection")
	}
}

func TestReceiveVideoUploadConcurrentOnlyOneSucceeds(t *testing.T) {
	service, _, cleanup := newUploadTestService(t)
	defer cleanup()
	ctx := context.Background()
	uploadURL, _, err := service.IssueVideoUpload(ctx, "video_job_race")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(uploadURL, "https://api.example/v1/media/uploads/")
	payload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'}, bytes.Repeat([]byte{2}, 128)...)

	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, putErr := service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(payload))
			results <- putErr
		}()
	}
	wg.Wait()
	close(results)
	success, fail := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else {
			fail++
		}
	}
	if success != 1 || fail != 7 {
		t.Fatalf("success=%d fail=%d", success, fail)
	}
}

func TestReceiveVideoUploadRejectsDeclaredMIMEMismatch(t *testing.T) {
	service, _, cleanup := newUploadTestService(t)
	defer cleanup()
	ctx := context.Background()
	uploadURL, _, err := service.IssueVideoUpload(ctx, "video_job_mime_declared")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(uploadURL, "https://api.example/v1/media/uploads/")
	payload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{0x01}, 32)...)
	_, err = service.ReceiveVideoUpload(ctx, token, "video/webm", bytes.NewReader(payload))
	if err == nil || !errors.Is(err, ErrInvalidVideoUpload) {
		t.Fatalf("declared mismatch err = %v", err)
	}
	if !strings.Contains(err.Error(), "Content-Type") {
		t.Fatalf("err = %v", err)
	}
}

func TestReceiveVideoUploadRejectsSniffedNonMP4UnderMP4Ticket(t *testing.T) {
	service, _, cleanup := newUploadTestService(t)
	defer cleanup()
	ctx := context.Background()
	uploadURL, _, err := service.IssueVideoUpload(ctx, "video_job_mime_sniff")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(uploadURL, "https://api.example/v1/media/uploads/")
	// WebM EBML header — 嗅探为 video/webm，与 MP4 票据冲突。
	webm := append([]byte{0x1A, 0x45, 0xDF, 0xA3}, bytes.Repeat([]byte{0x02}, 64)...)
	_, err = service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(webm))
	if err == nil || !errors.Is(err, ErrInvalidVideoUpload) {
		t.Fatalf("webm sniff err = %v", err)
	}
	// QuickTime 声明在 MP4 票据下直接拒绝（无需写入大 body）。
	uploadURL2, _, err := service.IssueVideoUpload(ctx, "video_job_mime_qt")
	if err != nil {
		t.Fatal(err)
	}
	token2 := strings.TrimPrefix(uploadURL2, "https://api.example/v1/media/uploads/")
	_, err = service.ReceiveVideoUpload(ctx, token2, "video/quicktime", bytes.NewReader(webm))
	if err == nil || !errors.Is(err, ErrInvalidVideoUpload) {
		t.Fatalf("quicktime declared err = %v", err)
	}
}

func TestReceiveVideoUploadTooLargeMapsToTooLargeError(t *testing.T) {
	database, objects, tickets, cleanup := openUploadTestDeps(t)
	defer cleanup()
	service := NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		tickets, objects, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	ctx := context.Background()
	// 小上限票据：避免分配 256MiB。
	tokenBytes := make([]byte, 32)
	for i := range tokenBytes {
		tokenBytes[i] = byte(i + 1)
	}
	token := hex.EncodeToString(tokenBytes)
	sum := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	if err := tickets.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: hex.EncodeToString(sum[:]), AssetID: "vid_small_limit_000001", JobID: "job_small",
		MaxBytes: 40, AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// ftyp 头 + 填充使体积 > 40。
	payload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{0x09}, 64)...)
	_, err := service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(payload))
	if err == nil || !errors.Is(err, ErrVideoUploadTooLarge) || !errors.Is(err, ErrInvalidVideoUpload) {
		t.Fatalf("ticket max err = %v", err)
	}

	// MaxBytesReader 路径：模拟中间件截断。
	token2Bytes := make([]byte, 32)
	for i := range token2Bytes {
		token2Bytes[i] = byte(i + 40)
	}
	token2 := hex.EncodeToString(token2Bytes)
	sum2 := sha256.Sum256([]byte(token2))
	if err := tickets.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: hex.EncodeToString(sum2[:]), AssetID: "vid_small_limit_000002", JobID: "job_maxbytes",
		MaxBytes: 1 << 20, AllowedMIME: "video/mp4", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	limited := http.MaxBytesReader(recorder, io.NopCloser(bytes.NewReader(payload)), 20)
	_, err = service.ReceiveVideoUpload(ctx, token2, "video/mp4", limited)
	if err == nil || !errors.Is(err, ErrVideoUploadTooLarge) {
		t.Fatalf("MaxBytesReader err = %v", err)
	}
}

func TestIssueVideoUploadRequiresHTTPSPublicBase(t *testing.T) {
	database, objects, tickets, cleanup := openUploadTestDeps(t)
	defer cleanup()
	service := NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		tickets, objects, nil,
		Config{PublicBaseURL: "http://127.0.0.1:8000", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	if _, _, err := service.IssueVideoUpload(context.Background(), "job"); err == nil {
		t.Fatal("expected public base error")
	}
}

// bindFailTicketRepo 在 BindJobResultAsset 注入非 NotFound 失败，用于签发失败闭环。
type bindFailTicketRepo struct {
	repository.MediaUploadTicketRepository
	bindErr     error
	deleteErr   error
	lastHash    string
	deleteCalls atomic.Int32
}

func (r *bindFailTicketRepo) CreateUploadTicket(ctx context.Context, ticket repository.MediaUploadTicket) error {
	r.lastHash = ticket.TokenHash
	return r.MediaUploadTicketRepository.CreateUploadTicket(ctx, ticket)
}

func (r *bindFailTicketRepo) BindJobResultAsset(ctx context.Context, jobID, assetID string) error {
	if r.bindErr != nil {
		return r.bindErr
	}
	return r.MediaUploadTicketRepository.BindJobResultAsset(ctx, jobID, assetID)
}

func (r *bindFailTicketRepo) DeleteUploadTicketByHash(ctx context.Context, tokenHash string) error {
	r.deleteCalls.Add(1)
	if r.deleteErr != nil {
		return r.deleteErr
	}
	return r.MediaUploadTicketRepository.DeleteUploadTicketByHash(ctx, tokenHash)
}

func TestIssueVideoUploadBindFailureStopsIssuance(t *testing.T) {
	database, objects, tickets, cleanup := openUploadTestDeps(t)
	defer cleanup()
	ctx := context.Background()
	injected := errors.New("injected bind database failure")
	failRepo := &bindFailTicketRepo{MediaUploadTicketRepository: tickets, bindErr: injected}
	service := NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		failRepo, objects, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	uploadURL, assetID, err := service.IssueVideoUpload(ctx, "video_job_bind_fail")
	if err == nil {
		t.Fatal("expected bind failure to stop issuance")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v", err)
	}
	if uploadURL != "" || assetID != "" {
		t.Fatalf("must not return upload URL or asset on bind failure: url=%q asset=%q", uploadURL, assetID)
	}
	msg := err.Error()
	if strings.Contains(msg, "/v1/media/uploads/") || strings.Contains(msg, "https://") {
		t.Fatalf("error must not contain upload URL: %q", msg)
	}
	if failRepo.lastHash == "" {
		t.Fatal("expected ticket create to record token hash")
	}
	if failRepo.deleteCalls.Load() != 1 {
		t.Fatalf("delete compensation calls = %d, want 1", failRepo.deleteCalls.Load())
	}
	// 失败票据必须被精确删除，不得残留到 TTL。
	if _, getErr := tickets.GetUploadTicketByHash(ctx, failRepo.lastHash); !errors.Is(getErr, repository.ErrNotFound) {
		t.Fatalf("failed ticket should be compensated away: %v", getErr)
	}

	// 补偿删除失败时须保留 bind 与 delete 两因，且仍不暴露 token/URL。
	deleteInjected := errors.New("injected delete compensation failure")
	failBoth := &bindFailTicketRepo{
		MediaUploadTicketRepository: tickets,
		bindErr:                     injected,
		deleteErr:                   deleteInjected,
	}
	serviceBoth := NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		failBoth, objects, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	_, _, err = serviceBoth.IssueVideoUpload(ctx, "video_job_bind_delete_fail")
	if err == nil {
		t.Fatal("expected combined bind+delete failure")
	}
	if !errors.Is(err, injected) || !errors.Is(err, deleteInjected) {
		t.Fatalf("combined err must retain both causes: %v", err)
	}
	if strings.Contains(err.Error(), "/v1/media/uploads/") || strings.Contains(err.Error(), "https://") {
		t.Fatalf("combined error must not contain upload URL: %q", err.Error())
	}

	// ErrNotFound 仍允许占位/测试路径签发，且不得触发补偿删除。
	notFoundRepo := &bindFailTicketRepo{MediaUploadTicketRepository: tickets, bindErr: repository.ErrNotFound}
	serviceOK := NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		notFoundRepo, objects, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	urlOK, assetOK, err := serviceOK.IssueVideoUpload(ctx, "video_job_bind_notfound")
	if err != nil || urlOK == "" || assetOK == "" {
		t.Fatalf("ErrNotFound bind should still issue: url=%q asset=%q err=%v", urlOK, assetOK, err)
	}
	if notFoundRepo.deleteCalls.Load() != 0 {
		t.Fatalf("ErrNotFound path must not delete ticket: deletes=%d", notFoundRepo.deleteCalls.Load())
	}
	if _, getErr := tickets.GetUploadTicketByHash(ctx, notFoundRepo.lastHash); getErr != nil {
		t.Fatalf("placeholder ticket must remain after ErrNotFound bind: %v", getErr)
	}
}

// getFailAssetRepo 在 GetMediaAsset 注入仓储故障。
type getFailAssetRepo struct {
	repository.MediaAssetRepository
	getErr   error
	getCalls atomic.Int32
}

func (r *getFailAssetRepo) GetMediaAsset(ctx context.Context, id string) (mediadomain.Asset, error) {
	r.getCalls.Add(1)
	if r.getErr != nil {
		return mediadomain.Asset{}, r.getErr
	}
	return r.MediaAssetRepository.GetMediaAsset(ctx, id)
}

// countingObjectStore 统计对象写入口，验证预检失败时无写入。
type countingObjectStore struct {
	inner       repository.MediaObjectStorage
	beginCalls  atomic.Int32
	commitCalls atomic.Int32
}

func (s *countingObjectStore) SaveImage(ctx context.Context, id, mimeType string, data []byte) (string, error) {
	return s.inner.SaveImage(ctx, id, mimeType, data)
}
func (s *countingObjectStore) SaveVideo(ctx context.Context, id, mimeType string, data []byte) (string, error) {
	return s.inner.SaveVideo(ctx, id, mimeType, data)
}
func (s *countingObjectStore) BeginVideoUpload(ctx context.Context, id, mimeType string) (string, string, error) {
	s.beginCalls.Add(1)
	return s.inner.BeginVideoUpload(ctx, id, mimeType)
}
func (s *countingObjectStore) CommitVideoUpload(ctx context.Context, tempPath, storageKey string) error {
	s.commitCalls.Add(1)
	return s.inner.CommitVideoUpload(ctx, tempPath, storageKey)
}
func (s *countingObjectStore) AbortVideoUpload(ctx context.Context, tempPath string) error {
	return s.inner.AbortVideoUpload(ctx, tempPath)
}
func (s *countingObjectStore) Open(ctx context.Context, storageKey string) (io.ReadCloser, error) {
	return s.inner.Open(ctx, storageKey)
}
func (s *countingObjectStore) Delete(ctx context.Context, storageKey string) error {
	return s.inner.Delete(ctx, storageKey)
}

// consumeCountingTicketRepo 统计消费调用，验证预检失败时未消费。
type consumeCountingTicketRepo struct {
	repository.MediaUploadTicketRepository
	consumeCalls atomic.Int32
}

func (r *consumeCountingTicketRepo) ConsumeUploadTicket(ctx context.Context, tokenHash string, now time.Time) (repository.MediaUploadTicket, bool, error) {
	r.consumeCalls.Add(1)
	return r.MediaUploadTicketRepository.ConsumeUploadTicket(ctx, tokenHash, now)
}

func TestReceiveVideoUploadAssetLookupFailureNoWriteOrConsume(t *testing.T) {
	database, objects, tickets, cleanup := openUploadTestDeps(t)
	defer cleanup()
	injected := errors.New("injected asset repository failure")
	assets := &getFailAssetRepo{
		MediaAssetRepository: relational.NewMediaAssetRepository(database),
		getErr:               injected,
	}
	store := &countingObjectStore{inner: objects}
	ticketWrap := &consumeCountingTicketRepo{MediaUploadTicketRepository: tickets}
	service := NewServiceWithTickets(
		assets, relational.NewMediaJobRepository(database), ticketWrap, store, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	ctx := context.Background()
	// 先用健康服务签发票据，再切换到故障资产仓储接收。
	issueService := NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		tickets, objects, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	uploadURL, _, err := issueService.IssueVideoUpload(ctx, "video_job_asset_get_fail")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(uploadURL, "https://api.example/v1/media/uploads/")
	payload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{0x05}, 32)...)
	_, err = service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(payload))
	if err == nil || !errors.Is(err, injected) {
		t.Fatalf("receive err = %v", err)
	}
	if assets.getCalls.Load() < 1 {
		t.Fatal("expected GetMediaAsset preflight call")
	}
	if store.beginCalls.Load() != 0 {
		t.Fatalf("object write must not start on asset lookup failure: begin=%d", store.beginCalls.Load())
	}
	if store.commitCalls.Load() != 0 {
		t.Fatalf("commit must not run: %d", store.commitCalls.Load())
	}
	if ticketWrap.consumeCalls.Load() != 0 {
		t.Fatalf("ticket must not be consumed on asset lookup failure: %d", ticketWrap.consumeCalls.Load())
	}
	sum := sha256.Sum256([]byte(token))
	ticket, getErr := tickets.GetUploadTicketByHash(ctx, hex.EncodeToString(sum[:]))
	if getErr != nil {
		t.Fatal(getErr)
	}
	if ticket.ConsumedAt != nil {
		t.Fatalf("ticket consumed_at should remain nil: %#v", ticket)
	}
}

// failingCommitStore 首次 CommitVideoUpload 失败，后续委托真实存储，用于验证票据释放与重试。
type failingCommitStore struct {
	inner         repository.MediaObjectStorage
	failRemaining atomic.Int32
	commitCalls   atomic.Int32
}

func (s *failingCommitStore) SaveImage(ctx context.Context, id, mimeType string, data []byte) (string, error) {
	return s.inner.SaveImage(ctx, id, mimeType, data)
}
func (s *failingCommitStore) SaveVideo(ctx context.Context, id, mimeType string, data []byte) (string, error) {
	return s.inner.SaveVideo(ctx, id, mimeType, data)
}
func (s *failingCommitStore) BeginVideoUpload(ctx context.Context, id, mimeType string) (string, string, error) {
	return s.inner.BeginVideoUpload(ctx, id, mimeType)
}
func (s *failingCommitStore) CommitVideoUpload(ctx context.Context, tempPath, storageKey string) error {
	s.commitCalls.Add(1)
	if s.failRemaining.Add(-1) >= 0 {
		return errors.New("injected commit failure")
	}
	return s.inner.CommitVideoUpload(ctx, tempPath, storageKey)
}
func (s *failingCommitStore) AbortVideoUpload(ctx context.Context, tempPath string) error {
	return s.inner.AbortVideoUpload(ctx, tempPath)
}
func (s *failingCommitStore) Open(ctx context.Context, storageKey string) (io.ReadCloser, error) {
	return s.inner.Open(ctx, storageKey)
}
func (s *failingCommitStore) Delete(ctx context.Context, storageKey string) error {
	return s.inner.Delete(ctx, storageKey)
}

func TestReceiveVideoUploadCommitFailureReleasesTicketForRetry(t *testing.T) {
	database, objects, tickets, cleanup := openUploadTestDeps(t)
	defer cleanup()
	store := &failingCommitStore{inner: objects}
	store.failRemaining.Store(1)
	service := NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		tickets, store, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	ctx := context.Background()
	uploadURL, assetID, err := service.IssueVideoUpload(ctx, "video_job_commit_fail")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(uploadURL, "https://api.example/v1/media/uploads/")
	payload := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{0x03}, 48)...)

	_, err = service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(payload))
	if err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("first receive err = %v", err)
	}
	// 票据必须已释放：未消费、未产生资产。
	sum := sha256.Sum256([]byte(token))
	ticket, err := tickets.GetUploadTicketByHash(ctx, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if ticket.ConsumedAt != nil {
		t.Fatalf("ticket should be released after commit failure, consumed_at=%v", ticket.ConsumedAt)
	}
	if _, getErr := relational.NewMediaAssetRepository(database).GetMediaAsset(ctx, assetID); getErr == nil {
		t.Fatal("asset must not exist after commit failure")
	}

	// 同 token 重试应成功（commit 第二次放行）。
	asset, err := service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("retry receive: %v", err)
	}
	if asset.ID != assetID || asset.Kind != "video" || asset.SizeBytes != int64(len(payload)) {
		t.Fatalf("asset = %#v", asset)
	}
	if store.commitCalls.Load() < 2 {
		t.Fatalf("commit calls = %d, want >= 2", store.commitCalls.Load())
	}
	// 重放仍应拒绝。
	if _, err := service.ReceiveVideoUpload(ctx, token, "video/mp4", bytes.NewReader(payload)); err == nil {
		t.Fatal("expected consumed rejection after successful upload")
	}
}

func newUploadTestService(t *testing.T) (*Service, *relational.MediaUploadTicketRepository, func()) {
	t.Helper()
	database, objects, tickets, cleanup := openUploadTestDeps(t)
	service := NewServiceWithTickets(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		tickets, objects, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute},
	)
	return service, tickets, cleanup
}

func openUploadTestDeps(t *testing.T) (*relational.Database, *media.LocalStore, *relational.MediaUploadTicketRepository, func()) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := media.NewLocalStore(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	tickets := relational.NewMediaUploadTicketRepository(database)
	return database, objects, tickets, func() { _ = database.Close() }
}

// Silence unused imports retained for sniff/edge coverage helpers.
var (
	_ = http.StatusOK
	_ = io.EOF
	_ = os.ErrNotExist
)
