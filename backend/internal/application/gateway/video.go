package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
)

const (
	videoJobTimeout          = 2 * time.Hour
	videoJobLease            = videoJobTimeout + 5*time.Minute
	videoJobRecoveryInterval = 30 * time.Second
	videoOutputAttempts      = 3
)

type VideoInput struct {
	RequestID     string
	ClientKey     clientkey.Key
	PublicModel   string
	Prompt        string
	Duration      int
	AspectRatio   string
	Resolution    string
	ReferenceURLs []string
}

func (s *Service) CreateVideo(ctx context.Context, input VideoInput) (media.Job, error) {
	if s.mediaJobs == nil || s.mediaQueue == nil {
		return media.Job{}, fmt.Errorf("视频任务服务未配置")
	}
	if len(input.Prompt) > 100000 || (len(input.Prompt) == 0 && len(input.ReferenceURLs) == 0) {
		return media.Job{}, fmt.Errorf("文本生视频必须提供 prompt；图片生视频可以省略 prompt")
	}
	inputJSON, err := encodeVideoInput(input.ReferenceURLs)
	if err != nil {
		return media.Job{}, err
	}
	routes, err := s.models.GetByPublicIDCandidates(ctx, input.PublicModel)
	if err != nil {
		return media.Job{}, ErrModelNotFound
	}
	route, err := s.selectMediaRoute(routes, input.ClientKey, model.CapabilityVideo, func(providerValue account.Provider) bool {
		_, ok := s.providers.Videos(providerValue)
		return ok
	})
	if err != nil {
		return media.Job{}, err
	}
	if err := s.checkLedgerReady(); err != nil {
		return media.Job{}, err
	}
	externalModel := model.ExternalPublicID(route.Provider, route.PublicID)
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	lease, err := s.selector.Acquire(ctx, route.Provider, route.UpstreamModel, quotaMode, "", nil, false)
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
		AccountID: accountID, AccountName: lease.Credential.Name,
		Provider: string(route.Provider), Model: externalModel, ModelRouteID: route.ID, UpstreamModel: model.DisplayUpstreamModel(route.Provider, route.UpstreamModel), Prompt: input.Prompt,
		Seconds: input.Duration, Size: input.AspectRatio, Quality: input.Resolution,
		Status: media.StatusQueued, Progress: 0, InputJSON: inputJSON, InputImageCount: len(input.ReferenceURLs), CreatedAt: now, UpdatedAt: now,
	}
	reserved := false
	if pricing, ok := audit.EstimateOfficialVideoCost(externalModel, input.Resolution, input.Duration); ok {
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

func (s *Service) GetVideo(ctx context.Context, id string, key clientkey.Key) (media.Job, error) {
	if s.mediaJobs == nil {
		return media.Job{}, ErrResponseNotFound
	}
	job, err := s.mediaJobs.GetMediaJob(ctx, id, key.ID)
	if err != nil {
		return media.Job{}, ErrResponseNotFound
	}
	return job, nil
}

func (s *Service) OpenVideoContent(ctx context.Context, id string, key clientkey.Key) (io.ReadCloser, string, int64, error) {
	job, err := s.GetVideo(ctx, id, key)
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
		s.failVideoJob(ctx, job, "model_not_found", errors.New("模型路由不存在"))
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
	// 视频任务创建时已持久化账号归属；恢复只能重新获取原账号，禁止因后续
	// 轮询或结果处理失败切换到其他账号。
	lease, err := s.selector.AcquirePinned(ctx, route.Provider, job.AccountID, route.UpstreamModel, "", true)
	if err != nil {
		if parent.Err() != nil {
			s.deferVideoJob(parent, job)
			return
		}
		s.failVideoJob(parent, job, "account_unavailable", err)
		return
	}
	defer lease.Release()
	adapter, ok := s.providers.Videos(route.Provider)
	if !ok {
		s.failVideoJob(parent, job, "provider_unavailable", ErrNoAvailableAccount)
		return
	}
	lastProgress := job.Progress
	result, err := adapter.GenerateVideo(ctx, provider.VideoRequest{
		Credential: lease.Credential, Billing: lease.Billing, JobID: job.ID, Prompt: job.Prompt, Duration: job.Seconds, AspectRatio: job.Size, Resolution: job.Quality,
		ReferenceURLs: decodeVideoInput(job.InputJSON),
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
	if err != nil {
		if parent.Err() != nil {
			s.deferVideoJob(parent, job)
			return
		}
		failureCtx, failureCancel := context.WithTimeout(context.Background(), finalizationTimeout)
		failureHandled := false
		if errors.Is(err, provider.ErrUnauthorized) {
			if lease.Credential.AuthType == account.AuthTypeSSO {
				s.markSSOCredentialRejected(failureCtx, lease.Credential, fmt.Sprintf("%s SSO credential rejected", lease.Credential.Provider))
			}
			failureHandled = true
		} else if status, ok := provider.ErrorHTTPStatus(err); ok {
			switch {
			case status == http.StatusUnauthorized && lease.Credential.AuthType == account.AuthTypeSSO:
				s.markSSOCredentialRejected(failureCtx, lease.Credential, fmt.Sprintf("%s SSO credential rejected", lease.Credential.Provider))
				failureHandled = true
			case status == http.StatusForbidden && s.providers.RetryForbiddenAsEgress(lease.Credential.Provider):
				// Web Provider 已对 anti-bot 403 降低出口健康并重建浏览器会话；
				// 视频请求已提交，不能换号重试，也不能误伤账号池。
				// 符合资格的 Build 主地址 403 由 Adapter 尝试 XAI，不在此禁用账号。
				failureHandled = true
			case status == http.StatusForbidden && lease.Credential.Provider == account.ProviderBuild:
				if !account.IsBuildSuper(lease.Credential, lease.Billing) {
					// 非 Super 的 403 按账号级故障处理；auto 模式不会因此回退 XAI。
					s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				}
				// Super（Billing paid 或 entitlement）的 403 保持服务级处理。
				failureHandled = true
			case (status == http.StatusPaymentRequired || status == http.StatusTooManyRequests) && lease.QuotaMode != "":
				exhausted, reconcileErr := s.accounts.ReconcileRateLimit(failureCtx, lease.Credential.ID, lease.QuotaMode, 0)
				s.selector.MarkQuotaStateChanged(lease.Credential.Provider)
				if reconcileErr != nil || !exhausted {
					s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				}
				failureHandled = true
			case status >= http.StatusInternalServerError:
				// 5xx 是 Provider 服务级故障，不应让某个账号退出号池。
				failureHandled = true
			default:
				s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				failureHandled = true
			}
		}
		if !failureHandled && !provider.IsMediaPostProcessingError(err) {
			s.selector.MarkFailure(failureCtx, lease.Credential, 0, 0)
		}
		failureCancel()
		applyMediaJobEgress(&job, egressTrace, route.Provider)
		s.logVideoGenerationFailure(job, lease.Credential, err)
		failureCode, publicErr := "generation_failed", err
		if status, ok := provider.ErrorHTTPStatus(err); errors.Is(err, provider.ErrUnauthorized) || (ok && (status == http.StatusUnauthorized || status == http.StatusForbidden)) {
			failureCode, publicErr = "provider_unavailable", errors.New("上游服务暂不可用")
		}
		s.failVideoJob(parent, job, failureCode, publicErr)
		return
	}
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
	if err := s.recordVideoAudit(context.Background(), job, time.Since(startedAt).Milliseconds()); err != nil {
		s.logger.Error("video_usage_record_failed", "job_id", job.ID, "event_id", "video_usage_"+job.ID, "error", err)
	}
	if quotaKind, _ := s.providers.QuotaKind(route.Provider); quotaKind == provider.QuotaRemoteWindow && lease.QuotaMode == "weekly" {
		s.accounts.QueueQuotaRefresh(job.AccountID, lease.QuotaMode)
	}
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
		if err := s.recordVideoAudit(ctx, job, durationMS); err != nil {
			result = firstError(result, fmt.Errorf("任务 %s: %w", job.ID, err))
		}
	}
	return result
}

func (s *Service) recordVideoAudit(ctx context.Context, job media.Job, durationMS int64) error {
	var accountID *uint64
	if job.AccountID > 0 {
		value := job.AccountID
		accountID = &value
	}
	createdAt := time.Now().UTC()
	if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
		createdAt = job.CompletedAt.UTC()
	}
	statusCode := http.StatusOK
	if job.Status == media.StatusFailed {
		statusCode = http.StatusBadGateway
		switch job.ErrorCode {
		case "account_unavailable", "provider_unavailable":
			statusCode = http.StatusServiceUnavailable
		case "model_not_found":
			statusCode = http.StatusNotFound
		}
	}
	record := audit.Record{
		EventID: "video_usage_" + job.ID, RequestID: job.RequestID, ClientKeyID: job.ClientKeyID, ClientKeyName: job.ClientKeyName,
		ModelRouteID: job.ModelRouteID, ModelPublicID: job.Model, ModelUpstreamModel: job.UpstreamModel,
		Provider: job.Provider, Operation: audit.OperationVideo, UsageSource: audit.UsageSourceNone,
		AccountID: accountID, AccountName: job.AccountName, StatusCode: statusCode, ErrorCode: job.ErrorCode,
		EgressNodeID: job.EgressNodeID, EgressNodeName: job.EgressNodeName, EgressScope: job.EgressScope, EgressMode: audit.EgressMode(job.EgressMode),
		MediaInputImages: int64(job.InputImageCount),
		DurationMS:       durationMS, CreatedAt: createdAt,
	}
	if job.Status == media.StatusCompleted {
		record.MediaOutputSeconds = int64(max(0, job.Seconds))
	}
	if pricing, ok := audit.EstimateOfficialVideoCost(job.Model, job.Quality, job.Seconds); ok && job.Status == media.StatusCompleted {
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

func encodeVideoInput(referenceURLs []string) (string, error) {
	data, err := json.Marshal(map[string][]string{"image_urls": referenceURLs})
	if err != nil {
		return "", fmt.Errorf("编码视频输入: %w", err)
	}
	if len(data) > media.MaxInputJSONBytes {
		return "", ErrVideoInputTooLarge
	}
	return string(data), nil
}

func decodeVideoInput(value string) []string {
	var input map[string][]string
	_ = json.Unmarshal([]byte(value), &input)
	return input["image_urls"]
}

func (s *Service) failVideoJob(ctx context.Context, job media.Job, code string, err error) {
	now := time.Now().UTC()
	job.Status, job.ErrorCode, job.ErrorMessage = media.StatusFailed, code, err.Error()
	if len(job.ErrorMessage) > 512 {
		job.ErrorMessage = job.ErrorMessage[:512]
	}
	job.LeaseUntil, job.UpdatedAt, job.CompletedAt = nil, now, &now
	if updateErr := s.persistVideoJobWithRetry(ctx, job); updateErr != nil {
		s.logger.Error("video_job_terminal_write_failed", "job_id", job.ID, "error", updateErr)
		return
	}
	if auditErr := s.recordVideoAudit(context.Background(), job, max(int64(0), now.Sub(job.CreatedAt).Milliseconds())); auditErr != nil {
		s.logger.Error("video_usage_record_failed", "job_id", job.ID, "event_id", "video_usage_"+job.ID, "error", auditErr)
	}
	s.cancelBillingReservation("video_usage_" + job.ID)
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
