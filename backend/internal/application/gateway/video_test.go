package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestVideoQuotaModeUsesWeb720pProduct(t *testing.T) {
	tests := []struct {
		provider   account.Provider
		resolution string
		want       string
	}{
		{account.ProviderWeb, "", account.QuotaModeWebVideo720p},
		{account.ProviderWeb, "720p", account.QuotaModeWebVideo720p},
		{account.ProviderWeb, "480p", account.QuotaModeWebVideo},
		{account.ProviderConsole, "720p", account.QuotaModeWebVideo},
	}
	for _, test := range tests {
		if got := videoQuotaMode(test.provider, account.QuotaModeWebVideo, test.resolution); got != test.want {
			t.Fatalf("videoQuotaMode(%s, %q) = %q, want %q", test.provider, test.resolution, got, test.want)
		}
	}
}

func TestRecoverVideoJobsRetriesUsageWithoutRegeneratingVideo(t *testing.T) {
	completedAt := time.Now().UTC()
	repository := &videoUsageRepository{job: media.Job{
		ID: "video_usage_recovery", RequestID: "request-usage-recovery",
		ClientKeyID: 1, ClientKeyName: "client", AccountID: 2, AccountName: "account",
		Provider: "grok_web", Model: "custom-video", ModelRouteID: 3, UpstreamModel: "Web/grok-imagine-video",
		Seconds: 8, Quality: "720p", Status: media.StatusCompleted, InputImageCount: 2, CreatedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
	}}
	recorder := &durableVideoAuditRecorder{failures: 1}
	service := &Service{mediaJobs: repository, audits: recorder}
	if err := service.RecoverVideoJobs(context.Background()); err == nil {
		t.Fatal("first durable audit failure was ignored")
	}
	if repository.job.UsageRecordedAt != nil {
		t.Fatal("usage was marked before durable audit commit")
	}
	if err := service.RecoverVideoJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.job.UsageRecordedAt == nil || recorder.calls != 2 {
		t.Fatalf("recordedAt = %v, audit calls = %d", repository.job.UsageRecordedAt, recorder.calls)
	}
	if recorder.last.EventID != "video_usage_video_usage_recovery" || recorder.last.EstimatedCostInUSDTicks <= 0 || recorder.last.MediaInputImages != 2 {
		t.Fatalf("audit = %#v", recorder.last)
	}
}

func TestVideoEditRouteUsesResolvedUpstreamInsteadOfPublicName(t *testing.T) {
	routes := []model.Route{
		{ID: 1, PublicID: "grok-imagine-video", Provider: account.ProviderBuild, UpstreamModel: "grok-imagine-video"},
		{ID: 2, PublicID: "company-video-editor", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video"},
		{ID: 3, PublicID: "company-video-editor", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video-1.5"},
	}
	compatible, err := routesForVideoOperation(routes, provider.VideoOperationEdit)
	if err != nil {
		t.Fatal(err)
	}
	if len(compatible) != 1 || compatible[0].ID != 2 {
		t.Fatalf("compatible routes = %#v", compatible)
	}
	if _, err := routesForVideoOperation(routes[:1], provider.VideoOperationExtend); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("unsupported route error = %v", err)
	}
}

func TestVideoPricingLeavesUnmeasurableOperationsUnpriced(t *testing.T) {
	pricing, priced := resolveVideoPricing(provider.VideoOperationGenerate, "Console/grok-imagine-video", "720p", 6, 1)
	if !priced || pricing.CostInUSDTicks <= 0 {
		t.Fatalf("priced generation = %#v, priced=%v", pricing, priced)
	}
	if pricing, priced := resolveVideoPricing(provider.VideoOperationGenerate, "Console/grok-imagine-video", "", 6, 0); priced || pricing.CostInUSDTicks != 0 {
		t.Fatalf("generation without resolution = %#v, priced=%v", pricing, priced)
	}
	if pricing, priced := resolveVideoPricing(provider.VideoOperationExtend, "Console/grok-imagine-video", "", 6, 0); priced || pricing.CostInUSDTicks != 0 {
		t.Fatalf("extension without measurable input duration = %#v, priced=%v", pricing, priced)
	}
}

func TestVideo1080pValidationUsesResolvedUpstreamModel(t *testing.T) {
	if err := validateVideoRouteParameters(provider.VideoOperationGenerate, "grok-imagine-video-1.5", "1080P", false); err != nil {
		t.Fatalf("1.5 text/image 1080p rejected: %v", err)
	}
	if err := validateVideoRouteParameters(provider.VideoOperationGenerate, "grok-imagine-video", "1080p", false); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("legacy 1080p error = %v", err)
	}
	if err := validateVideoRouteParameters(provider.VideoOperationGenerate, "grok-imagine-video-1.5", "1080p", true); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("reference 1080p error = %v", err)
	}
}

func TestEncodeVideoInputEnforcesPersistedLimit(t *testing.T) {
	// image_url and combined image_urls both store the same value, so the URL is counted twice.
	base := `{"image_url":"","image_urls":[""]}`
	overhead := len(base)
	urlLen := (media.MaxInputJSONBytes - overhead) / 2
	atLimit := strings.Repeat("A", urlLen)
	encoded, err := encodeVideoInput(atLimit, nil)
	if err != nil {
		t.Fatalf("encode at limit: %v", err)
	}
	if len(encoded) > media.MaxInputJSONBytes {
		t.Fatalf("encoded len=%d exceeds limit", len(encoded))
	}
	if _, err := encodeVideoInput(atLimit+"AA", nil); !errors.Is(err, ErrVideoInputTooLarge) {
		t.Fatalf("oversized input error = %v", err)
	}
}

func TestEncodeDecodeVideoInputPreservesImageAndReferences(t *testing.T) {
	encoded, err := encodeVideoInput("https://example.com/first.png", []string{"https://example.com/ref.png"})
	if err != nil {
		t.Fatal(err)
	}
	imageURL, refs := decodeVideoInputParts(encoded)
	if imageURL != "https://example.com/first.png" || len(refs) != 1 || refs[0] != "https://example.com/ref.png" {
		t.Fatalf("decoded split = %q %#v from %s", imageURL, refs, encoded)
	}

	encoded, err = encodeVideoInput("", []string{"https://example.com/ref-only.png"})
	if err != nil {
		t.Fatal(err)
	}
	imageURL, refs = decodeVideoInputParts(encoded)
	if imageURL != "" || len(refs) != 1 || refs[0] != "https://example.com/ref-only.png" {
		t.Fatalf("single reference decoded = %q %#v from %s", imageURL, refs, encoded)
	}

	imageURL, refs = decodeVideoInputParts(`{"image_urls":["https://legacy/one.png"]}`)
	if imageURL != "https://legacy/one.png" || len(refs) != 0 {
		t.Fatalf("legacy single = %q %#v", imageURL, refs)
	}
	imageURL, refs = decodeVideoInputParts(`{"image_urls":["https://legacy/a.png","https://legacy/b.png"]}`)
	if imageURL != "" || len(refs) != 2 || refs[0] != "https://legacy/a.png" || refs[1] != "https://legacy/b.png" {
		t.Fatalf("legacy multi = %q %#v", imageURL, refs)
	}
}

func TestEncodeDecodeVideoInputPreservesOperationAndReferenceAudio(t *testing.T) {
	encoded, err := encodeVideoInputFull(
		provider.VideoOperationGenerate,
		"",
		[]string{"https://example.com/ref.png"},
		[]string{"eve", " ara "},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	imageURL, refs, audios, videoURL := decodeVideoInputDetailed(encoded)
	if imageURL != "" || videoURL != "" || len(refs) != 1 || refs[0] != "https://example.com/ref.png" || len(audios) != 2 || audios[0] != "eve" || audios[1] != "ara" {
		t.Fatalf("decoded reference input = image %q refs %#v audios %#v video %q from %s", imageURL, refs, audios, videoURL, encoded)
	}
	if operation := decodeVideoOperation(encoded); operation != provider.VideoOperationGenerate {
		t.Fatalf("generation operation = %q", operation)
	}

	encoded, err = encodeVideoInputFull(provider.VideoOperationExtend, "", nil, nil, media.InputReference("source-video"))
	if err != nil {
		t.Fatal(err)
	}
	if operation := decodeVideoOperation(encoded); operation != provider.VideoOperationExtend {
		t.Fatalf("extension operation = %q from %s", operation, encoded)
	}
	_, _, _, videoURL = decodeVideoInputDetailed(encoded)
	if videoURL != media.InputReference("source-video") {
		t.Fatalf("extension video = %q", videoURL)
	}

	if err := validateVideoReferenceAudios([]string{"eve", ""}); err == nil {
		t.Fatal("blank reference voice was accepted")
	}
	if err := validateVideoReferenceAudios([]string{"a", "b", "c", "d"}); err == nil {
		t.Fatal("too many reference voices were accepted")
	}
}

func TestRecoverVideoJobsRecordsFailedAuditWithEgress(t *testing.T) {
	completedAt := time.Now().UTC()
	nodeID := uint64(42)
	repository := &videoUsageRepository{job: media.Job{
		ID: "video_failed_recovery", RequestID: "request-failed-recovery",
		ClientKeyID: 1, ClientKeyName: "client", AccountID: 2, AccountName: "account",
		Provider: "grok_web", Model: "grok-imagine-video", ModelRouteID: 3, UpstreamModel: "video",
		Seconds: 8, Quality: "720p", Status: media.StatusFailed, ErrorCode: "generation_failed", ErrorMessage: "upstream disconnected",
		EgressNodeID: &nodeID, EgressNodeName: "warp", EgressScope: "grok_web", EgressMode: "proxy",
		InputJSON: `{}`, CreatedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
	}}
	recorder := &durableVideoAuditRecorder{}
	service := &Service{mediaJobs: repository, audits: recorder}
	if err := service.RecoverVideoJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.job.UsageRecordedAt == nil || recorder.calls != 1 {
		t.Fatalf("recordedAt = %v, audit calls = %d", repository.job.UsageRecordedAt, recorder.calls)
	}
	if recorder.last.StatusCode != 502 || recorder.last.ErrorCode != "generation_failed" || recorder.last.EgressNodeID == nil || *recorder.last.EgressNodeID != nodeID || recorder.last.EgressNodeName != "warp" || recorder.last.EgressMode != audit.EgressModeProxy {
		t.Fatalf("audit = %#v", recorder.last)
	}
	if recorder.last.EstimatedCostInUSDTicks != 0 || recorder.last.MediaOutputSeconds != 0 {
		t.Fatalf("failed job was billed: %#v", recorder.last)
	}
}

func TestRecoverVideoJobsRecordsDetachedAccountSnapshot(t *testing.T) {
	completedAt := time.Now().UTC()
	repository := &videoUsageRepository{job: media.Job{
		ID: "video_detached_account", RequestID: "request-detached-account",
		ClientKeyID: 1, ClientKeyName: "client", AccountName: "deleted account",
		Provider: "grok_web", Model: "grok-imagine-video", ModelRouteID: 3, UpstreamModel: "video",
		Seconds: 8, Quality: "720p", Status: media.StatusFailed, ErrorCode: "generation_failed",
		InputJSON: `{}`, CreatedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
	}}
	recorder := &durableVideoAuditRecorder{}
	service := &Service{mediaJobs: repository, audits: recorder}
	if err := service.RecoverVideoJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorder.last.AccountID != nil || recorder.last.AccountName != "deleted account" {
		t.Fatalf("detached account audit = %#v", recorder.last)
	}
}

func TestLogVideoGenerationFailurePreservesUpstreamDiagnostic(t *testing.T) {
	var output bytes.Buffer
	service := &Service{logger: slog.New(slog.NewTextHandler(&output, nil))}
	nodeID := uint64(7)
	service.logVideoGenerationFailure(media.Job{
		ID: "video_failure", RequestID: "request-failure", UpstreamModel: "grok-imagine-video",
		EgressNodeID: &nodeID, EgressNodeName: "proxy-1", EgressScope: "grok_web", EgressMode: "proxy",
	}, account.Credential{ID: 42, Provider: account.ProviderWeb}, videoStatusError{
		status:  http.StatusForbidden,
		message: "Grok Web 媒体上游返回 403: upload denied access_token=secret https://assets.grok.com/video?token=secret",
	})
	logLine := output.String()
	for _, expected := range []string{
		"msg=video_generation_failed", "job_id=video_failure", "request_id=request-failure",
		"account_id=42", "provider=grok_web", "upstream_status=403", "upload denied",
		"egress_node_id=7", "egress_node_name=proxy-1",
	} {
		if !strings.Contains(logLine, expected) {
			t.Fatalf("log missing %q: %s", expected, logLine)
		}
	}
	for _, secret := range []string{"access_token=secret", "token=secret"} {
		if strings.Contains(logLine, secret) {
			t.Fatalf("log exposed %q: %s", secret, logLine)
		}
	}
}

type videoStatusError struct {
	status  int
	message string
}

func (e videoStatusError) Error() string       { return e.message }
func (e videoStatusError) HTTPStatusCode() int { return e.status }

func TestVideoQueueIsBoundedAndDeduplicated(t *testing.T) {
	service := &Service{}
	service.ConfigureMedia(&videoUsageRepository{}, 1)
	capacity := cap(service.mediaQueue)
	for index := range capacity {
		if !service.enqueueVideoJob(fmt.Sprintf("video_%d", index)) {
			t.Fatalf("enqueue %d failed before capacity", index)
		}
	}
	if !service.enqueueVideoJob("video_0") {
		t.Fatal("duplicate queued job should be treated as accepted")
	}
	if service.enqueueVideoJob("video_overflow") {
		t.Fatal("queue accepted a job beyond its capacity")
	}
}

func TestPersistRemoteVideoRetriesSameResultWithoutRegeneration(t *testing.T) {
	adapter := &videoPersistAdapter{failures: 1}
	store := &videoAssetStoreStub{}
	service := &Service{mediaAssets: store}
	credential := account.Credential{ID: 42, Provider: account.ProviderWeb}
	result, err := service.persistRemoteVideo(context.Background(), "video_job", adapter, credential, provider.VideoResult{URL: "https://assets.grok.com/video.mp4", ContentType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.generateCalls != 0 || adapter.downloadCalls != 2 || adapter.lastCredentialID != credential.ID {
		t.Fatalf("generate=%d download=%d credential=%d", adapter.generateCalls, adapter.downloadCalls, adapter.lastCredentialID)
	}
	if store.saveCalls != 1 || result.AssetID != "vid_local" || result.ContentType != "video/mp4" {
		t.Fatalf("store calls=%d result=%#v", store.saveCalls, result)
	}
}

func TestResolveVideoInputFileReferenceToDataURI(t *testing.T) {
	raw := []byte("png-bytes")
	inputID := "input_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	store := &videoAssetStoreStub{inputID: inputID, inputData: raw}
	service := &Service{mediaAssets: store}
	reference := VideoInputFileReference(inputID)
	if err := service.validateVideoInputReferences(context.Background(), []string{reference}, "image"); err != nil {
		t.Fatal(err)
	}
	if err := service.validateVideoInputReferences(context.Background(), []string{reference}, "video"); !errors.Is(err, ErrVideoInputUnavailable) {
		t.Fatalf("image accepted as video input: %v", err)
	}
	resolved, err := service.resolveVideoInputReferences(context.Background(), []string{"https://example.com/a.png", reference}, "image")
	if err != nil {
		t.Fatal(err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	if len(resolved) != 2 || resolved[0] != "https://example.com/a.png" || resolved[1] != want {
		t.Fatalf("resolved=%#v", resolved)
	}
	if err := service.validateVideoInputReferences(context.Background(), []string{VideoInputFileReference("missing")}, "image"); !errors.Is(err, ErrVideoInputUnavailable) {
		t.Fatalf("missing input error=%v", err)
	}
	store.inputSize = 20 << 20
	if err := service.validateVideoInputReferences(context.Background(), []string{reference, reference}, "image"); !errors.Is(err, ErrVideoInputTooLarge) {
		t.Fatalf("aggregate local input error=%v", err)
	}
}

func TestVideoInputMaterializationHasIndependentBulkhead(t *testing.T) {
	service := &Service{}
	service.ConfigureMedia(nil, 64)
	reference := VideoInputFileReference("input_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	releases := make([]func(), 0, videoInputMaterializeConcurrency)
	for range videoInputMaterializeConcurrency {
		release, err := service.acquireVideoInputSlot(context.Background(), []string{reference})
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.acquireVideoInputSlot(canceled, []string{reference}); !errors.Is(err, context.Canceled) {
		t.Fatalf("fifth local input acquire error=%v", err)
	}
	for _, release := range releases {
		release()
		release() // 释放函数必须可幂等调用。
	}
	if release, err := service.acquireVideoInputSlot(context.Background(), []string{"https://example.com/image.png"}); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}

type videoPersistAdapter struct {
	failures         int
	generateCalls    int
	downloadCalls    int
	lastCredentialID uint64
}

func (a *videoPersistAdapter) Provider() account.Provider { return account.ProviderWeb }

func (a *videoPersistAdapter) GenerateVideo(context.Context, provider.VideoRequest) (provider.VideoResult, error) {
	a.generateCalls++
	return provider.VideoResult{}, errors.New("must not regenerate")
}

func (a *videoPersistAdapter) DownloadVideo(_ context.Context, credential account.Credential, _ string) (io.ReadCloser, string, int64, error) {
	a.downloadCalls++
	a.lastCredentialID = credential.ID
	if a.downloadCalls <= a.failures {
		return nil, "", 0, errors.New("temporary download failure")
	}
	return io.NopCloser(strings.NewReader("video")), "video/mp4", 5, nil
}

type videoAssetStoreStub struct {
	saveCalls int
	inputID   string
	inputData []byte
	inputSize int64
	inputKind string
	inputMIME string
}

func (s *videoAssetStoreStub) SaveVideo(_ context.Context, jobID, contentType string, body io.Reader) (media.Asset, error) {
	s.saveCalls++
	if jobID != "video_job" {
		return media.Asset{}, fmt.Errorf("job ID = %s", jobID)
	}
	if contentType != "video/mp4" {
		return media.Asset{}, fmt.Errorf("content type = %s", contentType)
	}
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "video" {
		return media.Asset{}, fmt.Errorf("video body = %q: %w", data, err)
	}
	return media.Asset{ID: "vid_local", Kind: "video", MIMEType: "video/mp4", SizeBytes: int64(len(data))}, nil
}

func (*videoAssetStoreStub) OpenVideo(context.Context, string) (media.Asset, io.ReadCloser, error) {
	return media.Asset{}, nil, errors.New("not implemented")
}

func (s *videoAssetStoreStub) OpenInputAsset(_ context.Context, id string) (media.Asset, io.ReadCloser, error) {
	if id != s.inputID || len(s.inputData) == 0 {
		return media.Asset{}, nil, errors.New("not implemented")
	}
	size := s.inputSize
	if size <= 0 {
		size = int64(len(s.inputData))
	}
	kind := s.inputKind
	if kind == "" {
		kind = "image"
	}
	mimeType := s.inputMIME
	if mimeType == "" {
		mimeType = "image/png"
	}
	return media.Asset{ID: id, Kind: kind, MIMEType: mimeType, SizeBytes: size}, io.NopCloser(bytes.NewReader(s.inputData)), nil
}

func (*videoAssetStoreStub) ReleaseInputAssets(context.Context, []string) error { return nil }

type durableVideoAuditRecorder struct {
	failures int
	calls    int
	last     audit.Record
}

func (r *durableVideoAuditRecorder) Create(context.Context, audit.Record) error { return nil }

func (r *durableVideoAuditRecorder) CreateDurable(_ context.Context, value audit.Record) error {
	r.calls++
	r.last = value
	if r.calls <= r.failures {
		return errors.New("database unavailable")
	}
	return nil
}

type videoUsageRepository struct{ job media.Job }

func (r *videoUsageRepository) CreateMediaJob(context.Context, media.Job) error { return nil }

func (r *videoUsageRepository) GetMediaJob(context.Context, string, uint64) (media.Job, error) {
	return r.job, nil
}

func (r *videoUsageRepository) GetMediaJobsByIDs(context.Context, []string) ([]media.Job, error) {
	return []media.Job{r.job}, nil
}

func (r *videoUsageRepository) UpdateMediaJob(context.Context, media.Job) error { return nil }

func (r *videoUsageRepository) DeleteMediaJob(context.Context, string) error { return nil }

func (r *videoUsageRepository) ListMediaJobs(context.Context, repository.MediaJobListQuery) ([]media.Job, int64, error) {
	return nil, 0, nil
}

func (r *videoUsageRepository) SummarizeMediaJobs(context.Context) (repository.MediaJobStats, error) {
	return repository.MediaJobStats{}, nil
}

func (r *videoUsageRepository) ListRecoverableMediaJobs(context.Context, int) ([]media.Job, error) {
	return nil, nil
}

func (r *videoUsageRepository) ListUnrecordedTerminalMediaJobs(context.Context, int) ([]media.Job, error) {
	if r.job.UsageRecordedAt != nil || (r.job.Status != media.StatusCompleted && r.job.Status != media.StatusFailed) {
		return nil, nil
	}
	return []media.Job{r.job}, nil
}

func (r *videoUsageRepository) TryClaimMediaJob(context.Context, string, time.Time, time.Time, string) (media.Job, bool, error) {
	return media.Job{}, false, nil
}

func (r *videoUsageRepository) MarkMediaJobUsageRecorded(_ context.Context, _ string, recordedAt time.Time) error {
	r.job.UsageRecordedAt = &recordedAt
	return nil
}
