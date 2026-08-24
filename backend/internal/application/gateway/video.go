package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
)

const (
	videoJobTimeout          = 2 * time.Hour
	videoJobLease            = videoJobTimeout + 5*time.Minute
	videoJobRecoveryInterval = 30 * time.Second
	videoOutputAttempts      = 3
	// Base64 物化会同时持有原图和编码后字符串，单独限流避免高 mediaConcurrency 放大内存峰值。
	videoInputMaterializeConcurrency = 4
	videoInputJSONBaseBytes          = int64(len(`{"image_urls":[]}`))
)

// VideoInputFileReference 将本地临时 file_id 编码为只在 Gateway 内部解释的引用。
func VideoInputFileReference(fileID string) string {
	return media.InputReference(fileID)
}

type VideoInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	// Operation defaults to generate when empty.
	Operation   provider.VideoOperation
	Prompt      string
	Duration    int
	AspectRatio string
	Resolution  string
	// ImageURL is the optional first-frame image (official "image").
	ImageURL string
	// ReferenceURLs are style/content references (official "reference_images").
	ReferenceURLs []string
	// ReferenceAudios are preset voice_ids for reference-to-video.
	ReferenceAudios []string
	// VideoURL is required for edit/extend (official "video").
	VideoURL string
}

func (s *Service) CreateVideo(ctx context.Context, input VideoInput) (media.Job, error) {
	if s.mediaJobs == nil || s.mediaQueue == nil {
		return media.Job{}, fmt.Errorf("视频任务服务未配置")
	}
	operation := input.Operation
	if operation == "" {
		operation = provider.VideoOperationGenerate
	}
	switch operation {
	case provider.VideoOperationGenerate, provider.VideoOperationEdit, provider.VideoOperationExtend:
	default:
		return media.Job{}, fmt.Errorf("不支持的视频操作")
	}
	if len(input.Prompt) > 100000 {
		return media.Job{}, fmt.Errorf("prompt 过长")
	}
	if operation == provider.VideoOperationGenerate {
		hasImage := strings.TrimSpace(input.ImageURL) != ""
		hasRefs := len(input.ReferenceURLs) > 0
		hasRefAudio := len(input.ReferenceAudios) > 0
		if hasImage && (hasRefs || hasRefAudio) {
			return media.Job{}, fmt.Errorf("image 不能与 reference_images/reference_audios 同时使用")
		}
		if hasRefs || hasRefAudio {
			if len(input.Prompt) == 0 {
				return media.Job{}, fmt.Errorf("参考图/参考音频视频必须提供 prompt")
			}
			if resolution := strings.ToLower(strings.TrimSpace(input.Resolution)); resolution == "1080p" {
				return media.Job{}, fmt.Errorf("参考图视频 resolution 最高 720p")
			}
		}
		if len(input.Prompt) == 0 && !hasImage && !hasRefs && !hasRefAudio {
			return media.Job{}, fmt.Errorf("文本生视频必须提供 prompt；图片生视频可以省略 prompt")
		}
		if strings.TrimSpace(input.VideoURL) != "" {
			return media.Job{}, fmt.Errorf("视频生成不支持 video 输入")
		}
		if err := validateVideoReferenceAudios(input.ReferenceAudios); err != nil {
			return media.Job{}, err
		}
	} else {
		if strings.TrimSpace(input.Prompt) == "" {
			return media.Job{}, fmt.Errorf("视频编辑/延长必须提供 prompt")
		}
		if strings.TrimSpace(input.VideoURL) == "" {
			return media.Job{}, fmt.Errorf("视频编辑/延长必须提供 video")
		}
		if strings.TrimSpace(input.ImageURL) != "" || len(input.ReferenceURLs) > 0 || len(input.ReferenceAudios) > 0 {
			return media.Job{}, fmt.Errorf("视频编辑/延长不支持 image、reference_images 或 reference_audios")
		}
		if operation == provider.VideoOperationEdit && input.Duration != 0 {
			return media.Job{}, fmt.Errorf("视频编辑不支持 duration")
		}
		if operation == provider.VideoOperationExtend {
			if input.Duration == 0 {
				input.Duration = 6
			}
			if input.Duration < 2 || input.Duration > 10 {
				return media.Job{}, fmt.Errorf("视频延长 duration 必须在 2 到 10 秒之间")
			}
		}
	}
	routes, err := s.models.GetByPublicIDCandidates(ctx, input.PublicModel)
	if err != nil {
		return media.Job{}, ErrModelNotFound
	}
	routes, err = routesForVideoOperation(routes, operation)
	if err != nil {
		return media.Job{}, err
	}
	providerSupported := func(providerValue account.Provider) bool {
		_, ok := s.providers.Videos(providerValue)
		return ok
	}
	routes, _, err = s.eligibleMediaRoutes(routes, input.ClientKey, model.CapabilityVideo, providerSupported)
	if err != nil {
		return media.Job{}, err
	}
	routes, err = routesForVideoParameters(routes, operation, input.Resolution, strings.TrimSpace(input.ImageURL) != "", len(input.ReferenceURLs), input.Duration)
	if err != nil {
		return media.Job{}, err
	}
	allRefs := videoInputReferences(input.ImageURL, input.ReferenceURLs)
	if err := s.validateVideoInputReferences(ctx, allRefs, "image"); err != nil {
		return media.Job{}, err
	}
	if videoURL := strings.TrimSpace(input.VideoURL); videoURL != "" {
		if err := s.validateVideoInputReferences(ctx, []string{videoURL}, "video"); err != nil {
			return media.Job{}, err
		}
	}
	inputJSON, err := encodeVideoInputFull(operation, input.ImageURL, input.ReferenceURLs, input.ReferenceAudios, input.VideoURL)
	if err != nil {
		return media.Job{}, err
	}
	route, selection, err := s.selectSchedulableEligibleMediaRouteWithQuotaMode(ctx, routes, input.ClientKey, true, func(route model.Route) string {
		return videoQuotaMode(route.Provider, s.providers.QuotaMode(route.Provider, route.UpstreamModel), input.Resolution)
	})
	if err != nil {
		return media.Job{}, err
	}
	if err := s.checkLedgerReady(); err != nil {
		return media.Job{}, err
	}
	pricing, priced := resolveVideoPricing(operation, route.UpstreamModel, input.Resolution, input.Duration, len(allRefs))
	externalModel := model.ExternalPublicID(route.Provider, route.PublicID)
	lease, err := selection.Acquire(ctx, nil, false)
	if err != nil {
		return media.Job{}, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
	}
	accountID := lease.Credential.ID
	lease.Release()
	token, err := security.NewOpaqueToken(18)
	if err != nil {
		return media.Job{}, err
	}
	now := time.Now().UTC()
	job := media.Job{
		ID: "video_" + token, RequestID: input.RequestID,
		ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		ClientIP:  requestmeta.ClientIP(ctx),
		AccountID: accountID, AccountName: lease.Credential.Name,
		Provider: string(route.Provider), Model: externalModel, ModelRouteID: route.ID, UpstreamModel: model.DisplayUpstreamModel(route.Provider, route.UpstreamModel), Operation: operation, Prompt: input.Prompt,
		Seconds: input.Duration, Size: input.AspectRatio, Quality: input.Resolution,
		Status: media.StatusQueued, Progress: 0, InputJSON: inputJSON, InputImageCount: len(allRefs), CreatedAt: now, UpdatedAt: now,
	}
	reserved := false
	if priced {
		reserved, err = s.clientKeys.ReserveBilling(ctx, input.ClientKey, "video_usage_"+job.ID, pricing.CostInUSDTicks, mediaBillingReservationTTL)
		if err != nil {
			return media.Job{}, err
		}
	}
	if err := s.mediaJobs.CreateMediaJob(ctx, job); err != nil {
		if reserved {
			s.cancelBillingReservation("video_usage_" + job.ID)
		}
		return media.Job{}, err
	}
	if !s.enqueueVideoJob(job.ID) {
		s.logger.Warn("video_job_queue_full", "job_id", job.ID)
	}
	return job, nil
}

// routesForVideoParameters removes only routes that cannot accept this request.
// Callers must first apply capability, client-key, and Provider eligibility:
// the same public model may aggregate Console, Build, and Web routes with
// different input contracts.
func routesForVideoParameters(routes []model.Route, operation provider.VideoOperation, resolution string, hasImage bool, referenceCount, duration int) ([]model.Route, error) {
	if len(routes) == 0 {
		return routes, nil
	}
	compatible := make([]model.Route, 0, len(routes))
	var firstErr error
	for _, candidate := range routes {
		if err := validateVideoRouteParameters(candidate.Provider, operation, candidate.UpstreamModel, resolution, hasImage, referenceCount, duration); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		compatible = append(compatible, candidate)
	}
	if len(compatible) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return compatible, nil
}

// Console 视频输入的上限与时长按「Provider + 入口字段 + 上游模型」分档。
// /v1/videos/generations 是异步接口，只在 adapter 层拦截会产出必然失败的任务；
// adapter 仍保留同一约束，作为最终出站边界的防御校验。
func validateVideoRouteParameters(providerValue account.Provider, operation provider.VideoOperation, upstreamModel, resolution string, hasImage bool, referenceCount, duration int) error {
	if operation != provider.VideoOperationGenerate {
		return nil
	}
	trimmedModel := strings.TrimSpace(upstreamModel)
	hasReferences := referenceCount > 0
	if providerValue == account.ProviderWeb && (hasImage || hasReferences) {
		return fmt.Errorf("%w: Grok Web 当前仅支持文本生视频；图片视频请使用 Build 或 Console Provider", ErrVideoOperationUnsupported)
	}
	if providerValue == account.ProviderConsole && (trimmedModel == "grok-imagine-video" || trimmedModel == "grok-imagine-video-1.5") {
		// 实测：8 张 reference_images 上游回 400
		// "Too many reference images: 8. Maximum allowed is 7."（两个视频模型一致）。
		if referenceCount > provider.ConsoleVideoMaxReferenceImages {
			return fmt.Errorf("%w: Console reference_images 最多 %d 张，当前为 %d 张", ErrVideoParameterInvalid, provider.ConsoleVideoMaxReferenceImages, referenceCount)
		}
		// 实测：grok-imagine-video 的 reference-to-video 上游回 400
		// "Duration 15s exceeds the maximum allowed for reference-to-video, which is 10s."；
		// 走 image 字段的 image-to-video 与 grok-imagine-video-1.5 都保持 15s。
		if trimmedModel == "grok-imagine-video" && hasReferences && duration > provider.ConsoleVideoMaxReferenceDurationSeconds {
			return fmt.Errorf("%w: Console %s 的参考图生视频最长 %d 秒，当前为 %d 秒", ErrVideoParameterInvalid, trimmedModel, provider.ConsoleVideoMaxReferenceDurationSeconds, duration)
		}
	}
	if !strings.EqualFold(strings.TrimSpace(resolution), "1080p") {
		return nil
	}
	if trimmedModel != "grok-imagine-video-1.5" {
		return fmt.Errorf("%w: %s 不支持 1080p", ErrVideoOperationUnsupported, upstreamModel)
	}
	if hasReferences {
		return fmt.Errorf("%w: reference_images 模式最高支持 720p", ErrVideoOperationUnsupported)
	}
	return nil
}

func routesForVideoOperation(routes []model.Route, operation provider.VideoOperation) ([]model.Route, error) {
	if operation == provider.VideoOperationGenerate {
		return routes, nil
	}
	compatible := make([]model.Route, 0, len(routes))
	for _, candidate := range routes {
		if candidate.Provider == account.ProviderConsole && strings.TrimSpace(candidate.UpstreamModel) == "grok-imagine-video" {
			compatible = append(compatible, candidate)
		}
	}
	if len(compatible) == 0 {
		return nil, ErrVideoOperationUnsupported
	}
	return compatible, nil
}

func resolveVideoPricing(operation provider.VideoOperation, upstreamModel, resolution string, duration, inputImages int) (audit.PricingResult, bool) {
	if operation == provider.VideoOperationGenerate {
		return audit.EstimateOfficialVideoCost(upstreamModel, resolution, duration, inputImages)
	}
	return audit.PricingResult{}, false
}

func (s *Service) getVideoJob(ctx context.Context, id string, key clientkey.Key) (media.Job, error) {
	if s.mediaJobs == nil {
		return media.Job{}, ErrResponseNotFound
	}
	job, err := s.mediaJobs.GetMediaJob(ctx, id, key.ID)
	if err != nil {
		return media.Job{}, ErrResponseNotFound
	}
	return job, nil
}

// GetVideo returns the client-facing job state. A result asset is exposed only
// while the local object can still be opened; completed assets are eligible for
// capacity cleanup, and a stale asset ID would otherwise produce a dead public URL.
func (s *Service) GetVideo(ctx context.Context, id string, key clientkey.Key) (media.Job, error) {
	job, err := s.getVideoJob(ctx, id, key)
	if err != nil {
		return media.Job{}, err
	}
	if job.Status != media.StatusCompleted || strings.TrimSpace(job.ResultAssetID) == "" {
		return job, nil
	}
	if s.mediaAssets == nil {
		job.ResultAssetID = ""
		return job, nil
	}
	_, body, openErr := s.mediaAssets.OpenVideo(ctx, job.ResultAssetID)
	if openErr != nil || body == nil {
		job.ResultAssetID = ""
		return job, nil
	}
	_ = body.Close()
	return job, nil
}

func (s *Service) OpenVideoContent(ctx context.Context, id string, key clientkey.Key) (io.ReadCloser, string, int64, error) {
	job, err := s.getVideoJob(ctx, id, key)
	if err != nil {
		return nil, "", 0, err
	}
	if job.Status != media.StatusCompleted {
		return nil, "", 0, fmt.Errorf("视频内容尚未可用")
	}
	// 本地资产优先：XAI ZDR 上传完成后不经公网回环下载。
	if job.ResultAssetID != "" && s.mediaAssets != nil {
		asset, body, openErr := s.mediaAssets.OpenVideo(ctx, job.ResultAssetID)
		if openErr == nil {
			return body, asset.MIMEType, asset.SizeBytes, nil
		}
	}
	if job.UpstreamURL == "" {
		return nil, "", 0, fmt.Errorf("视频内容尚未可用")
	}
	adapter, ok := s.providers.Videos(account.Provider(job.Provider))
	if !ok {
		return nil, "", 0, ErrResponseAccountUnavailable
	}
	downloader, ok := adapter.(provider.VideoContentDownloader)
	if !ok || s.selector == nil || s.selector.accounts == nil || s.accounts == nil {
		return nil, "", 0, ErrResponseAccountUnavailable
	}
	credential, err := s.selector.accounts.Get(ctx, job.AccountID)
	if err != nil {
		return nil, "", 0, ErrResponseAccountUnavailable
	}
	credential, err = s.accounts.EnsureCredential(ctx, credential, false)
	if err != nil {
		return nil, "", 0, ErrResponseAccountUnavailable
	}
	return downloader.DownloadVideo(ctx, credential, job.UpstreamURL)
}

func (s *Service) RecoverVideoJobs(ctx context.Context) error {
	if s.mediaJobs == nil {
		return nil
	}
	usageErr := s.reconcileVideoUsage(ctx)
	values, err := s.mediaJobs.ListRecoverableMediaJobs(ctx, 1000)
	if err != nil {
		return errors.Join(usageErr, err)
	}
	for _, job := range values {
		if !s.enqueueVideoJob(job.ID) {
			break
		}
	}
	return usageErr
}

// RunVideoWorkers 使用固定 Worker 处理持久化任务，避免突发请求按任务创建无界 goroutine。
func (s *Service) RunVideoWorkers(ctx context.Context) {
	if s.mediaQueue == nil || s.mediaWorker <= 0 {
		return
	}
	var workers sync.WaitGroup
	workers.Add(s.mediaWorker)
	for range s.mediaWorker {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-s.mediaQueue:
					err := batch.Do(ctx, func(workCtx context.Context) error {
						s.processVideoJob(workCtx, id)
						return nil
					})
					s.mediaMu.Lock()
					delete(s.mediaQueued, id)
					s.mediaMu.Unlock()
					if err != nil && ctx.Err() == nil {
						if panicErr, ok := err.(*batch.PanicError); ok {
							s.logger.Error("video_worker_panicked", "job_id", id, "error", panicErr, "stack", string(panicErr.Stack))
						} else {
							s.logger.Error("video_worker_failed", "job_id", id, "error", err)
						}
					}
				}
			}
		}()
	}
	workers.Wait()
}

func (s *Service) enqueueVideoJob(id string) bool {
	if id == "" || s.mediaQueue == nil {
		return false
	}
	s.mediaMu.Lock()
	if _, exists := s.mediaQueued[id]; exists {
		s.mediaMu.Unlock()
		return true
	}
	s.mediaQueued[id] = struct{}{}
	s.mediaMu.Unlock()
	select {
	case s.mediaQueue <- id:
		return true
	default:
		s.mediaMu.Lock()
		delete(s.mediaQueued, id)
		s.mediaMu.Unlock()
		full := s.mediaQueueFull.Add(1)
		if s.logger != nil && (full == 1 || full%100 == 0) {
			s.logger.Warn("video_queue_full", "count", full, "queued", len(s.mediaQueue), "capacity", cap(s.mediaQueue))
		}
		return false
	}
}

func (s *Service) processVideoJob(ctx context.Context, id string) {
	job, claimed, err := s.claimVideoJob(ctx, id)
	if err != nil {
		s.logger.Warn("video_job_claim_failed", "job_id", id, "error", err)
		return
	}
	if !claimed {
		return
	}
	var route model.Route
	if job.ModelRouteID != 0 {
		route, err = s.models.Get(ctx, job.ModelRouteID)
	} else {
		route, err = s.models.GetByPublicID(ctx, job.Model)
	}
	if err != nil {
		s.failVideoJob(ctx, job, "model_not_found", errors.New("模型路由不存在"), 0, nil)
		return
	}
	s.runVideoJob(ctx, job, route)
}

// RunVideoRecovery 周期认领新建后未启动或执行实例失联后的媒体任务。
func (s *Service) RunVideoRecovery(ctx context.Context) {
	ticker := time.NewTicker(videoJobRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RecoverVideoJobs(ctx); err != nil {
				s.logger.Warn("video_job_recovery_failed", "error", err)
			}
		}
	}
}

func (s *Service) claimVideoJob(ctx context.Context, id string) (media.Job, bool, error) {
	now := time.Now().UTC()
	claimToken, err := security.NewOpaqueToken(18)
	if err != nil {
		return media.Job{}, false, err
	}
	return s.mediaJobs.TryClaimMediaJob(ctx, id, now, now.Add(videoJobLease), claimToken)
}

func (s *Service) runVideoJob(parent context.Context, job media.Job, route model.Route) {
	ctx, cancel := context.WithTimeout(parent, videoJobTimeout)
	defer cancel()
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	job.Progress = max(job.Progress, 1)
	job.UpdatedAt = time.Now().UTC()
	if err := s.mediaJobs.UpdateMediaJob(ctx, job); err != nil {
		s.logger.Warn("video_job_progress_write_failed", "job_id", job.ID, "error", err)
	}
	inputReferences := decodeVideoInput(job.InputJSON)
	releaseInputSlot, err := s.acquireVideoInputSlot(ctx, inputReferences)
	if err != nil {
		s.deferVideoJob(parent, job)
		return
	}
	defer releaseInputSlot()
	adapter, ok := s.providers.Videos(route.Provider)
	if !ok {
		s.failVideoJob(parent, job, "provider_unavailable", ErrNoAvailableAccount, 0, nil)
		return
	}
	operation := provider.VideoOperation(job.Operation)
	if operation == "" {
		operation = decodeVideoOperation(job.InputJSON)
	}
	imageURL, referenceURLs, referenceAudios, videoURL, err := s.resolveVideoJobInputs(ctx, operation, job.InputJSON)
	if err != nil {
		s.failVideoJob(parent, job, "input_unavailable", err, 0, nil)
		return
	}
	duration := job.Seconds
	aspectRatio := job.Size
	resolution := job.Quality
	switch operation {
	case provider.VideoOperationEdit:
		duration, aspectRatio, resolution = 0, "", ""
	case provider.VideoOperationExtend:
		aspectRatio, resolution = "", ""
	}

	quotaMode := videoQuotaMode(route.Provider, s.providers.QuotaMode(route.Provider, route.UpstreamModel), job.Quality)
	quotaRefreshGroup := s.providers.QuotaRefreshGroup(route.Provider, route.UpstreamModel)
	attemptPolicy := s.videoAttemptPolicy()
	excluded := make(map[uint64]bool)
	forbiddenEgressRetried := make(map[uint64]bool)
	var retryPinnedAccountID uint64
	failureAttempts := newFailureAttemptRecorder(http.MethodPost, "/videos/generations")
	var selection *selectionSession
	var lease *accountLease
	var result provider.VideoResult
	var lastErr error

	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		attemptStarted := time.Now()
		err = nil
		if lease != nil {
			lease.Release()
			lease = nil
		}
		// First try stays on the account chosen when the local job was created.
		// Create-stage failures may switch accounts; poll failures never reach here as retries.
		pinnedAccountID := retryPinnedAccountID
		retryPinnedAccountID = 0
		if attempt == 0 {
			pinnedAccountID = job.AccountID
		}
		if pinnedAccountID > 0 && !excluded[pinnedAccountID] {
			lease, err = s.selector.AcquirePinned(ctx, route.Provider, pinnedAccountID, route.ID, route.UpstreamModel, quotaMode, true)
			if err != nil {
				excluded[pinnedAccountID] = true
				lease = nil
			}
		}
		if lease == nil {
			if selection == nil {
				selection, err = s.selector.beginSelectionSession(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", excluded, false)
			}
			if err == nil {
				lease, err = selection.Acquire(ctx, excluded, false)
			}
		}
		if err != nil {
			if parent.Err() != nil {
				s.deferVideoJob(parent, job)
				return
			}
			if lastErr == nil {
				lastErr = err
			}
			s.failVideoJob(parent, job, "account_unavailable", lastErr, 0, failureAttempts.snapshot())
			return
		}
		excluded[lease.Credential.ID] = true
		credential, credErr := s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if credErr != nil {
			failureAttempts.captureCredentialFailure(lease.Credential, attemptStarted, false, credErr)
			lastErr = credErr
			if attemptPolicy.hasNext(attempt) {
				continue
			}
			s.failVideoJob(parent, job, "account_unavailable", credErr, 0, failureAttempts.snapshot())
			return
		}
		lease.Credential = credential
		if job.AccountID != credential.ID || job.AccountName != credential.Name {
			job.AccountID, job.AccountName = credential.ID, credential.Name
			job.UpdatedAt = time.Now().UTC()
			_ = s.mediaJobs.UpdateMediaJob(ctx, job)
		}

		lastProgress := job.Progress
		result, err = adapter.GenerateVideo(ctx, provider.VideoRequest{
			Credential: lease.Credential, Billing: lease.Billing, JobID: job.ID, Model: route.UpstreamModel,
			Operation: operation,
			Prompt:    job.Prompt, Duration: duration, AspectRatio: aspectRatio, Resolution: resolution,
			ImageURL: imageURL, ReferenceURLs: referenceURLs, ReferenceAudios: referenceAudios, VideoURL: videoURL,
			Progress: func(value int) {
				value = min(99, max(1, value))
				if value-lastProgress < 5 {
					return
				}
				lastProgress = value
				job.Progress, job.UpdatedAt = value, time.Now().UTC()
				leaseUntil := job.UpdatedAt.Add(videoJobLease)
				job.LeaseUntil = &leaseUntil
				updateCtx, updateCancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = s.mediaJobs.UpdateMediaJob(updateCtx, job)
				updateCancel()
			},
		})
		if err == nil && result.AssetID == "" && result.URL != "" {
			result, err = s.persistRemoteVideo(ctx, job.ID, adapter, lease.Credential, result)
		}
		if err == nil {
			break
		}
		lastErr = err
		captureVideoAttempt(failureAttempts, lease.Credential, attemptStarted, err)
		if parent.Err() != nil {
			s.deferVideoJob(parent, job)
			return
		}

		failureCtx, failureCancel := context.WithTimeout(context.Background(), finalizationTimeout)
		failureHandled := false
		retriableCreate := false
		stage, hasStage := provider.VideoErrorStage(err)
		safeCreateFailure := hasStage && stage == provider.VideoStageCreate
		status, hasStatus := provider.ErrorHTTPStatus(err)
		if errors.Is(err, provider.ErrUnauthorized) {
			if lease.Credential.AuthType == account.AuthTypeSSO {
				s.markSSOCredentialRejected(failureCtx, lease.Credential, fmt.Sprintf("%s SSO credential rejected", lease.Credential.Provider))
			}
			failureHandled = true
			retriableCreate = safeCreateFailure
		} else if hasStatus {
			switch {
			case status == http.StatusUnauthorized && lease.Credential.AuthType == account.AuthTypeSSO:
				s.markSSOCredentialRejected(failureCtx, lease.Credential, fmt.Sprintf("%s SSO credential rejected", lease.Credential.Provider))
				failureHandled = true
				retriableCreate = safeCreateFailure
			case status == http.StatusForbidden && s.providers.RetryForbiddenAsEgress(lease.Credential.Provider):
				// Web anti-bot 403 is egress-scoped. Retry the same account once so
				// egress invalidation can rebuild the route, then move on normally.
				failureHandled = true
				if safeCreateFailure {
					if !forbiddenEgressRetried[lease.Credential.ID] {
						forbiddenEgressRetried[lease.Credential.ID] = true
						retryPinnedAccountID = lease.Credential.ID
						delete(excluded, lease.Credential.ID)
					}
					retriableCreate = true
				}
			case status == http.StatusForbidden && lease.Credential.Provider == account.ProviderBuild:
				if !account.IsBuildSuper(lease.Credential, lease.Billing) {
					s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				}
				failureHandled = true
				retriableCreate = safeCreateFailure && !account.IsBuildSuper(lease.Credential, lease.Billing)
			case (status == http.StatusPaymentRequired || status == http.StatusTooManyRequests) && lease.QuotaMode != "":
				state, reconcileErr := s.accounts.ReconcileRateLimit(failureCtx, lease.Credential.ID, lease.QuotaMode, 0)
				s.applyRateLimitReconciliation(failureCtx, lease.Credential, status, 0, state, reconcileErr)
				failureHandled = true
				retriableCreate = safeCreateFailure
			case status == http.StatusTooManyRequests || status == http.StatusPaymentRequired:
				s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				failureHandled = true
				retriableCreate = safeCreateFailure
			case status >= http.StatusInternalServerError:
				// A 5xx may be returned after the upstream accepted the job. Never
				// switch accounts without an idempotency guarantee.
				failureHandled = true
				retriableCreate = false
			default:
				s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				failureHandled = true
				retriableCreate = false
			}
		}
		if !failureHandled && !provider.IsMediaPostProcessingError(err) {
			s.selector.MarkFailure(failureCtx, lease.Credential, 0, 0)
			retriableCreate = safeCreateFailure
		}
		failureCancel()
		applyMediaJobEgress(&job, egressTrace, route.Provider)
		s.logVideoGenerationFailure(job, lease.Credential, err)

		// Poll/post-processing failures are bound to the already-created upstream job.
		if provider.IsMediaPostProcessingError(err) || (hasStage && stage == provider.VideoStagePoll) || !retriableCreate || !attemptPolicy.hasNext(attempt) {
			failureCode, publicErr := "generation_failed", err
			upstreamStatus := 0
			if hasStatus {
				upstreamStatus = status
			}
			if errors.Is(err, provider.ErrUnauthorized) || (hasStatus && (status == http.StatusUnauthorized || status == http.StatusForbidden)) {
				failureCode, publicErr = "provider_unavailable", errors.New("上游服务暂不可用")
			} else if hasStatus && status == http.StatusTooManyRequests {
				failureCode = "rate_limited"
			}
			s.failVideoJob(parent, job, failureCode, publicErr, upstreamStatus, failureAttempts.snapshot())
			return
		}
		// Create-stage failure: switch account according to runtime attempt policy.
		continue
	}
	if lease == nil {
		s.failVideoJob(parent, job, "account_unavailable", ErrNoAvailableAccount, 0, failureAttempts.snapshot())
		return
	}
	defer lease.Release()

	// Provider 已消费请求体，尽早释放 Base64 物化名额和大字符串。
	referenceURLs = nil
	releaseInputSlot()

	now := time.Now().UTC()
	job.Status, job.Progress, job.UpstreamURL, job.ContentType = media.StatusCompleted, 100, result.URL, result.ContentType
	// 成功终态必须清空历史错误字段，避免管理端/恢复路径把中间失败文案当成最终结果。
	job.ErrorCode, job.ErrorMessage = "", ""
	if result.AssetID != "" {
		job.ResultAssetID = result.AssetID
	}
	applyMediaJobEgress(&job, egressTrace, route.Provider)
	job.LeaseUntil, job.UpdatedAt, job.CompletedAt = nil, now, &now
	if err := s.persistVideoJobWithRetry(parent, job); err != nil {
		s.logger.Error("video_job_terminal_write_failed", "job_id", job.ID, "error", err)
		return
	}
	s.selector.MarkSuccess(context.Background(), lease.Credential)
	refreshMode, decrementMode, availabilityMode := quotaFinalizationModes(lease.QuotaMode, quotaRefreshGroup)
	if decrementMode != "" && decrementMode != "weekly" {
		quotaCtx, quotaCancel := context.WithTimeout(context.Background(), accountStateWriteTimeout)
		updated, quotaErr := s.accounts.DecrementQuota(quotaCtx, job.AccountID, decrementMode, 1)
		quotaCancel()
		if quotaErr != nil {
			s.logger.Warn("video_quota_decrement_failed", "provider", route.Provider, "account_id", job.AccountID, "mode", decrementMode, "error", quotaErr)
		} else if updated {
			s.selector.ConsumeQuota(route.Provider, job.AccountID, decrementMode, 1)
		}
	}
	if err := s.recordVideoAudit(context.Background(), job, time.Since(startedAt).Milliseconds(), http.StatusOK, failureAttempts.snapshot()); err != nil {
		s.logger.Error("video_usage_record_failed", "job_id", job.ID, "event_id", "video_usage_"+job.ID, "error", err)
	}
	if quotaKind, _ := s.providers.QuotaKind(route.Provider); quotaKind == provider.QuotaRemoteWindow && refreshMode != "" {
		s.accounts.QueueQuotaRefresh(job.AccountID, refreshMode)
		if availabilityMode != "" && availabilityMode != refreshMode {
			s.accounts.QueueQuotaRefresh(job.AccountID, availabilityMode)
		}
	}
	// 输入回收放在账号状态、计费和审计收尾之后，存储抖动不得延迟关键终态逻辑。
	s.releaseVideoInputs(job)
}

func videoQuotaMode(providerValue account.Provider, catalogMode, resolution string) string {
	if providerValue == account.ProviderWeb && catalogMode == account.QuotaModeWebVideo {
		resolution = strings.ToLower(strings.TrimSpace(resolution))
		if resolution == "" || resolution == "720p" {
			return account.QuotaModeWebVideo720p
		}
	}
	return catalogMode
}

func (s *Service) acquireVideoInputSlot(ctx context.Context, references []string) (func(), error) {
	hasLocalInput := false
	for _, reference := range references {
		if strings.HasPrefix(reference, media.InputReferencePrefix) {
			hasLocalInput = true
			break
		}
	}
	if !hasLocalInput || s.mediaInputSlots == nil {
		return func() {}, nil
	}
	select {
	case s.mediaInputSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.mediaInputSlots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) validateVideoInputReferences(ctx context.Context, references []string, expectedKind string) error {
	estimatedBytes := videoInputJSONBaseBytes
	for _, reference := range references {
		fileID, local := media.ParseInputReference(reference)
		if !strings.HasPrefix(reference, media.InputReferencePrefix) {
			if !addVideoReferenceBytes(&estimatedBytes, int64(len(reference))) {
				return ErrVideoInputTooLarge
			}
			continue
		}
		if s.mediaAssets == nil || !local {
			return ErrVideoInputUnavailable
		}
		asset, body, err := s.mediaAssets.OpenInputAsset(ctx, fileID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVideoInputUnavailable, err)
		}
		_ = body.Close()
		if asset.Kind != expectedKind {
			return fmt.Errorf("%w: file_id 必须引用%s", ErrVideoInputUnavailable, expectedKind)
		}
		if !addVideoReferenceBytes(&estimatedBytes, materializedVideoReferenceBytes(asset.MIMEType, asset.SizeBytes)) {
			return ErrVideoInputTooLarge
		}
	}
	return nil
}

func (s *Service) resolveVideoInputParts(ctx context.Context, inputJSON string) (string, []string, error) {
	imageURL, referenceURLs := decodeVideoInputParts(inputJSON)
	all := videoInputReferences(imageURL, referenceURLs)
	resolved, err := s.resolveVideoInputReferences(ctx, all, "image")
	if err != nil {
		return "", nil, err
	}
	// Map resolved URLs back while preserving empty/non-empty slots by original order.
	next := 0
	take := func(original string) string {
		original = strings.TrimSpace(original)
		if original == "" {
			return ""
		}
		if next >= len(resolved) {
			return original
		}
		value := resolved[next]
		next++
		return value
	}
	outImage := take(imageURL)
	outRefs := make([]string, 0, len(referenceURLs))
	for _, raw := range referenceURLs {
		if value := take(raw); value != "" {
			outRefs = append(outRefs, value)
		}
	}
	return outImage, outRefs, nil
}

func (s *Service) resolveVideoInputReferences(ctx context.Context, references []string, expectedKind string) ([]string, error) {
	resolved := make([]string, 0, len(references))
	estimatedBytes := videoInputJSONBaseBytes
	for _, reference := range references {
		fileID, local := media.ParseInputReference(reference)
		if !strings.HasPrefix(reference, media.InputReferencePrefix) {
			if !addVideoReferenceBytes(&estimatedBytes, int64(len(reference))) {
				return nil, ErrVideoInputTooLarge
			}
			resolved = append(resolved, reference)
			continue
		}
		if s.mediaAssets == nil || !local {
			return nil, ErrVideoInputUnavailable
		}
		asset, body, err := s.mediaAssets.OpenInputAsset(ctx, fileID)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrVideoInputUnavailable, err)
		}
		if asset.Kind != expectedKind {
			_ = body.Close()
			return nil, fmt.Errorf("%w: file_id 必须引用%s", ErrVideoInputUnavailable, expectedKind)
		}
		data, readErr := io.ReadAll(io.LimitReader(body, media.MaxInputJSONBytes+1))
		closeErr := body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("读取视频临时输入: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("关闭视频临时输入: %w", closeErr)
		}
		if len(data) == 0 || len(data) > media.MaxInputJSONBytes {
			return nil, ErrVideoInputTooLarge
		}
		if !addVideoReferenceBytes(&estimatedBytes, materializedVideoReferenceBytes(asset.MIMEType, int64(len(data)))) {
			return nil, ErrVideoInputTooLarge
		}
		resolved = append(resolved, "data:"+asset.MIMEType+";base64,"+base64.StdEncoding.EncodeToString(data))
	}
	return resolved, nil
}

func materializedVideoReferenceBytes(mimeType string, sizeBytes int64) int64 {
	if sizeBytes <= 0 || sizeBytes > int64(media.MaxInputJSONBytes) {
		return -1
	}
	return int64(len("data:")+len(mimeType)+len(";base64,")) + ((sizeBytes+2)/3)*4
}

func addVideoReferenceBytes(total *int64, referenceBytes int64) bool {
	// 两个引号加一个逗号是保守的单元 JSON 开销（首项不需要逗号）。
	const jsonElementOverhead = 3
	addition := referenceBytes + jsonElementOverhead
	limit := int64(media.MaxInputJSONBytes)
	if referenceBytes < 0 || addition < 0 || *total > limit-addition {
		return false
	}
	*total += addition
	return true
}

// persistRemoteVideo 只重试已经生成的视频结果下载与本地归档，不重新调用生成接口，
// 且所有尝试固定使用创建任务的同一凭据。
func (s *Service) persistRemoteVideo(ctx context.Context, jobID string, adapter provider.VideoAdapter, credential account.Credential, result provider.VideoResult) (provider.VideoResult, error) {
	if s.mediaAssets == nil {
		return result, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, errors.New("视频媒体存储未配置"))
	}
	downloader, ok := adapter.(provider.VideoContentDownloader)
	if !ok {
		return result, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, errors.New("Provider 不支持视频内容下载"))
	}
	var lastErr error
	for attempt := 0; attempt < videoOutputAttempts; attempt++ {
		body, contentType, _, downloadErr := downloader.DownloadVideo(ctx, credential, result.URL)
		if downloadErr != nil {
			lastErr = provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, downloadErr)
		} else {
			asset, saveErr := s.mediaAssets.SaveVideo(ctx, jobID, contentType, body)
			_ = body.Close()
			if saveErr == nil {
				result.AssetID = asset.ID
				result.ContentType = asset.MIMEType
				return result, nil
			}
			lastErr = provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, saveErr)
		}
		if ctx.Err() != nil || attempt+1 >= videoOutputAttempts {
			break
		}
		if waitErr := waitVideoOutputRetry(ctx, attempt); waitErr != nil {
			return result, waitErr
		}
	}
	return result, lastErr
}

func waitVideoOutputRetry(ctx context.Context, attempt int) error {
	delays := [...]time.Duration{200 * time.Millisecond, 750 * time.Millisecond}
	timer := time.NewTimer(delays[min(attempt, len(delays)-1)])
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) reconcileVideoUsage(ctx context.Context) error {
	jobs, err := s.mediaJobs.ListUnrecordedTerminalMediaJobs(ctx, 200)
	if err != nil {
		return err
	}
	var result error
	for _, job := range jobs {
		durationMS := int64(0)
		if job.CompletedAt != nil {
			durationMS = max(int64(0), job.CompletedAt.Sub(job.CreatedAt).Milliseconds())
		}
		if err := s.recordVideoAudit(ctx, job, durationMS, 0, nil); err != nil {
			result = firstError(result, fmt.Errorf("任务 %s: %w", job.ID, err))
		}
	}
	return result
}

func (s *Service) recordVideoAudit(ctx context.Context, job media.Job, durationMS int64, upstreamStatus int, attempts []audit.Attempt) error {
	var accountID *uint64
	if job.AccountID > 0 {
		value := job.AccountID
		accountID = &value
	}
	createdAt := time.Now().UTC()
	if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
		createdAt = job.CompletedAt.UTC()
	}
	statusCode := resolveVideoAuditStatusCode(job, upstreamStatus, attempts)
	record := audit.Record{
		EventID: "video_usage_" + job.ID, RequestID: job.RequestID, ClientKeyID: job.ClientKeyID, ClientKeyName: job.ClientKeyName,
		ClientIP:     job.ClientIP,
		ModelRouteID: job.ModelRouteID, ModelPublicID: job.Model, ModelUpstreamModel: job.UpstreamModel,
		Provider: job.Provider, Operation: audit.OperationVideo, UsageSource: audit.UsageSourceNone,
		AccountID: accountID, AccountName: job.AccountName, StatusCode: statusCode, ErrorCode: job.ErrorCode,
		EgressNodeID: job.EgressNodeID, EgressNodeName: job.EgressNodeName, EgressScope: job.EgressScope, EgressMode: audit.EgressMode(job.EgressMode),
		MediaInputImages: int64(job.InputImageCount),
		DurationMS:       durationMS, AttemptCount: len(attempts), Attempts: append([]audit.Attempt(nil), attempts...), CreatedAt: createdAt,
		RequestMethod: http.MethodPost, RequestPath: "/v1/videos/generations",
	}
	if job.Status == media.StatusCompleted && job.Seconds > 0 {
		record.MediaOutputSeconds = int64(max(0, job.Seconds))
	}
	operation := provider.VideoOperation(job.Operation)
	if operation == "" {
		operation = provider.VideoOperationGenerate
	}
	if pricing, ok := audit.EstimateOfficialVideoCost(job.UpstreamModel, job.Quality, job.Seconds, job.InputImageCount); ok && job.Status == media.StatusCompleted && operation == provider.VideoOperationGenerate {
		record.EstimatedCostInUSDTicks = pricing.CostInUSDTicks
		record.PricingModel = pricing.Model
		record.PricingVersion = audit.OfficialPricingAsOf
	}
	if durable, ok := s.audits.(interface {
		CreateDurable(context.Context, audit.Record) error
	}); ok {
		if err := durable.CreateDurable(ctx, record); err != nil {
			return err
		}
	} else if err := s.audits.Create(ctx, record); err != nil {
		return err
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return s.mediaJobs.MarkMediaJobUsageRecorded(markCtx, job.ID, time.Now().UTC())
}

func encodeVideoInput(imageURL string, referenceURLs []string) (string, error) {
	return encodeVideoInputFull(provider.VideoOperationGenerate, imageURL, referenceURLs, nil, "")
}

func encodeVideoInputFull(operation provider.VideoOperation, imageURL string, referenceURLs []string, referenceAudios []string, videoURL string) (string, error) {
	payload := map[string]any{}
	if operation == "" {
		operation = provider.VideoOperationGenerate
	}
	if operation != provider.VideoOperationGenerate {
		payload["operation"] = string(operation)
	}
	if value := strings.TrimSpace(imageURL); value != "" {
		payload["image_url"] = value
	}
	refs := make([]string, 0, len(referenceURLs))
	for _, raw := range referenceURLs {
		if value := strings.TrimSpace(raw); value != "" {
			refs = append(refs, value)
		}
	}
	if len(refs) > 0 {
		payload["reference_urls"] = refs
	}
	audios := normalizeVideoReferenceAudios(referenceAudios)
	if len(audios) > 0 {
		payload["reference_audios"] = audios
	}
	if value := strings.TrimSpace(videoURL); value != "" {
		payload["video_url"] = value
	}
	// Keep a combined image_urls field for older readers/tools that only
	// understand the pre-split shape. New workers prefer image_url/reference_urls.
	combined := videoInputReferences(imageURL, referenceURLs)
	if len(combined) > 0 {
		payload["image_urls"] = combined
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码视频输入: %w", err)
	}
	if len(data) > media.MaxInputJSONBytes {
		return "", ErrVideoInputTooLarge
	}
	return string(data), nil
}

func decodeVideoInput(value string) []string {
	imageURL, refs, videoURL := decodeVideoInputFull(value)
	values := videoInputReferences(imageURL, refs)
	if v := strings.TrimSpace(videoURL); v != "" {
		values = append(values, v)
	}
	return values
}

func decodeVideoInputParts(value string) (string, []string) {
	imageURL, refs, _ := decodeVideoInputFull(value)
	return imageURL, refs
}

func decodeVideoInputFull(value string) (string, []string, string) {
	imageURL, refs, _, videoURL := decodeVideoInputDetailed(value)
	return imageURL, refs, videoURL
}

func decodeVideoInputDetailed(value string) (string, []string, []string, string) {
	var input struct {
		Operation       string   `json:"operation"`
		ImageURL        string   `json:"image_url"`
		ReferenceURLs   []string `json:"reference_urls"`
		ReferenceAudios []string `json:"reference_audios"`
		ImageURLs       []string `json:"image_urls"`
		VideoURL        string   `json:"video_url"`
	}
	_ = json.Unmarshal([]byte(value), &input)
	videoURL := strings.TrimSpace(input.VideoURL)
	audios := normalizeVideoReferenceAudios(input.ReferenceAudios)
	if strings.TrimSpace(input.ImageURL) != "" || len(input.ReferenceURLs) > 0 || len(audios) > 0 || videoURL != "" || strings.TrimSpace(input.Operation) != "" {
		refs := make([]string, 0, len(input.ReferenceURLs))
		for _, raw := range input.ReferenceURLs {
			if v := strings.TrimSpace(raw); v != "" {
				refs = append(refs, v)
			}
		}
		return strings.TrimSpace(input.ImageURL), refs, audios, videoURL
	}
	// Legacy jobs stored a flat image_urls list. Preserve historical mapping:
	// one URL was treated as the first-frame image; multiple URLs were references.
	legacy := make([]string, 0, len(input.ImageURLs))
	for _, raw := range input.ImageURLs {
		if v := strings.TrimSpace(raw); v != "" {
			legacy = append(legacy, v)
		}
	}
	switch len(legacy) {
	case 0:
		return "", nil, nil, ""
	case 1:
		return legacy[0], nil, nil, ""
	default:
		return "", legacy, nil, ""
	}
}

func decodeVideoOperation(value string) provider.VideoOperation {
	var input struct {
		Operation string `json:"operation"`
	}
	_ = json.Unmarshal([]byte(value), &input)
	switch provider.VideoOperation(strings.TrimSpace(input.Operation)) {
	case provider.VideoOperationEdit:
		return provider.VideoOperationEdit
	case provider.VideoOperationExtend:
		return provider.VideoOperationExtend
	default:
		return provider.VideoOperationGenerate
	}
}

func (s *Service) resolveVideoJobInputs(ctx context.Context, operation provider.VideoOperation, inputJSON string) (string, []string, []string, string, error) {
	_, _, referenceAudios, videoURL := decodeVideoInputDetailed(inputJSON)
	resolvedImage, resolvedRefs, err := s.resolveVideoInputParts(ctx, inputJSON)
	if err != nil {
		return "", nil, nil, "", err
	}
	if strings.TrimSpace(videoURL) == "" {
		return resolvedImage, resolvedRefs, referenceAudios, "", nil
	}
	if operation == provider.VideoOperationGenerate {
		return "", nil, nil, "", ErrVideoInputUnavailable
	}
	resolvedVideos, err := s.resolveVideoInputReferences(ctx, []string{videoURL}, "video")
	if err != nil {
		return "", nil, nil, "", err
	}
	if len(resolvedVideos) == 0 {
		return "", nil, nil, "", ErrVideoInputUnavailable
	}
	return resolvedImage, resolvedRefs, referenceAudios, resolvedVideos[0], nil
}

func validateVideoReferenceAudios(values []string) error {
	if len(values) > 3 {
		return fmt.Errorf("reference_audios 最多 3 个")
	}
	for _, raw := range values {
		if strings.TrimSpace(raw) == "" {
			return fmt.Errorf("reference_audios.voice_id 不能为空")
		}
	}
	return nil
}

func normalizeVideoReferenceAudios(values []string) []string {
	out := make([]string, 0, len(values))
	for _, raw := range values {
		if value := strings.TrimSpace(raw); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func videoInputReferences(imageURL string, referenceURLs []string) []string {
	values := make([]string, 0, 1+len(referenceURLs))
	if v := strings.TrimSpace(imageURL); v != "" {
		values = append(values, v)
	}
	for _, raw := range referenceURLs {
		if v := strings.TrimSpace(raw); v != "" {
			values = append(values, v)
		}
	}
	return values
}

func (s *Service) failVideoJob(ctx context.Context, job media.Job, code string, err error, upstreamStatus int, attempts []audit.Attempt) {
	now := time.Now().UTC()
	message := ""
	if err != nil {
		message = err.Error()
	}
	job.Status, job.ErrorCode, job.ErrorMessage = media.StatusFailed, code, message
	if len(job.ErrorMessage) > 512 {
		job.ErrorMessage = job.ErrorMessage[:512]
	}
	job.LeaseUntil, job.UpdatedAt, job.CompletedAt = nil, now, &now
	if updateErr := s.persistVideoJobWithRetry(ctx, job); updateErr != nil {
		s.logger.Error("video_job_terminal_write_failed", "job_id", job.ID, "error", updateErr)
		return
	}
	if auditErr := s.recordVideoAudit(context.Background(), job, max(int64(0), now.Sub(job.CreatedAt).Milliseconds()), upstreamStatus, attempts); auditErr != nil {
		s.logger.Error("video_usage_record_failed", "job_id", job.ID, "event_id", "video_usage_"+job.ID, "error", auditErr)
	}
	s.cancelBillingReservation("video_usage_" + job.ID)
	s.releaseVideoInputs(job)
}

func resolveVideoAuditStatusCode(job media.Job, upstreamStatus int, attempts []audit.Attempt) int {
	if job.Status != media.StatusFailed {
		return http.StatusOK
	}
	if upstreamStatus >= 100 && upstreamStatus <= 599 {
		return upstreamStatus
	}
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].UpstreamStatusCode != nil {
			status := *attempts[i].UpstreamStatusCode
			if status >= 100 && status <= 599 {
				return status
			}
		}
	}
	if status := parseUpstreamStatusFromMessage(job.ErrorMessage); status > 0 {
		return status
	}
	switch job.ErrorCode {
	case "account_unavailable", "provider_unavailable":
		return http.StatusServiceUnavailable
	case "model_not_found":
		return http.StatusNotFound
	case "rate_limited":
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func parseUpstreamStatusFromMessage(message string) int {
	message = strings.TrimSpace(message)
	if message == "" {
		return 0
	}
	// Console/Web summaries commonly look like: "Console 媒体上游返回 429: ..."
	for _, token := range []string{"返回 ", "status ", "Status ", "HTTP "} {
		if idx := strings.Index(message, token); idx >= 0 {
			rest := strings.TrimSpace(message[idx+len(token):])
			n := 0
			for i := 0; i < len(rest); i++ {
				ch := rest[i]
				if ch < '0' || ch > '9' {
					break
				}
				n = n*10 + int(ch-'0')
				if n > 599 {
					return 0
				}
			}
			if n >= 100 && n <= 599 {
				return n
			}
		}
	}
	return 0
}

func captureVideoAttempt(recorder *failureAttemptRecorder, credential account.Credential, startedAt time.Time, err error) {
	if recorder == nil || err == nil {
		return
	}
	status, hasStatus := provider.ErrorHTTPStatus(err)
	stage := "upstream_response"
	if videoStage, ok := provider.VideoErrorStage(err); ok {
		stage = "video_" + string(videoStage)
	}
	attempt := audit.Attempt{
		Source:         audit.AttemptSourceUpstreamHTTP,
		Stage:          stage,
		AccountID:      auditAccountID(credential.ID),
		AccountName:    credential.Name,
		Method:         recorder.method,
		RequestPath:    recorder.path,
		StartedAt:      startedAt.UTC(),
		DurationMS:     time.Since(startedAt).Milliseconds(),
		TransportError: sanitizeDiagnosticText(err.Error(), diagnosticTextLimit),
		ErrorChain:     errorFrames(err),
	}
	if hasStatus {
		value := status
		attempt.UpstreamStatusCode = &value
		attempt.UpstreamStatus = http.StatusText(status)
		body := []byte(sanitizeDiagnosticText(err.Error(), diagnosticTextLimit))
		attempt.ResponseBody = body
	}
	recorder.append(attempt)
}

func (s *Service) videoAttemptPolicy() routingAttemptPolicy {
	configured := int(s.videoMaxAttempts.Load())
	// Legacy installs may still have 0 from the short-lived "inherit" default.
	// Treat it as the general default pool size instead of reintroducing inherit UI.
	if configured == 0 {
		configured = 999
	}
	return newRoutingAttemptPolicy(configured)
}

func (s *Service) releaseVideoInputs(job media.Job) {
	if s.mediaAssets == nil {
		return
	}
	references := decodeVideoInput(job.InputJSON)
	if len(references) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.mediaAssets.ReleaseInputAssets(ctx, references); err != nil {
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("video_input_release_failed", "job_id", job.ID, "error", err)
	}
}

func (s *Service) logVideoGenerationFailure(job media.Job, credential account.Credential, err error) {
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	attributes := []any{
		"job_id", job.ID,
		"request_id", job.RequestID,
		"account_id", credential.ID,
		"provider", credential.Provider,
		"model", job.UpstreamModel,
		"egress_scope", job.EgressScope,
		"egress_mode", job.EgressMode,
		"error", sanitizeDiagnosticText(err.Error(), 512),
	}
	if status, ok := provider.ErrorHTTPStatus(err); ok {
		attributes = append(attributes, "upstream_status", status)
	}
	if job.EgressNodeID != nil {
		attributes = append(attributes, "egress_node_id", *job.EgressNodeID, "egress_node_name", job.EgressNodeName)
	}
	logger.Warn("video_generation_failed", attributes...)
}

func (s *Service) deferVideoJob(ctx context.Context, job media.Job) {
	now := time.Now().UTC()
	leaseUntil := now.Add(5 * time.Minute)
	job.Status = media.StatusInProgress
	job.LeaseUntil = &leaseUntil
	job.UpdatedAt = now
	job.ErrorCode = ""
	job.ErrorMessage = ""
	if err := s.persistVideoJobWithRetry(ctx, job); err != nil {
		s.logger.Error("video_job_defer_write_failed", "job_id", job.ID, "error", err)
	}
}

// persistVideoJobWithRetry 至少执行一次收尾写入；后续退避可被工作进程关闭信号取消。
func (s *Service) persistVideoJobWithRetry(ctx context.Context, job media.Job) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		lastErr = s.mediaJobs.UpdateMediaJob(writeCtx, job)
		cancel()
		if lastErr == nil {
			return nil
		}
		if attempt < 3 {
			timer := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.Join(lastErr, ctx.Err())
			case <-timer.C:
			}
		}
	}
	return lastErr
}
