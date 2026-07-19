package media

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrAssetNotFound         = errors.New("媒体资源不存在")
	ErrInvalidImage          = errors.New("图片内容无效")
	ErrInvalidImageSelection = errors.New("图片选择无效")
	ErrInvalidVideoSelection = errors.New("视频任务选择无效")
	ErrActiveVideoSelection  = errors.New("排队中或生成中的视频任务不能删除")
	ErrInvalidFilter         = errors.New("媒体筛选条件无效")
	ErrMediaJobsUnavailable  = errors.New("视频任务仓储未配置")
)

// Service 负责图片/视频校验、文件落盘和元数据持久化的一致性收口。
type Service struct {
	assets        repository.MediaAssetRepository
	jobs          repository.MediaJobRepository
	tickets       repository.MediaUploadTicketRepository
	objects       repository.MediaObjectStorage
	cleanupLock   repository.DistributedLock
	publicBaseURL string
	configMu      sync.RWMutex
	maxImageBytes int64
	maxTotalBytes int64
	cleanupAt     int
	cleanupEvery  time.Duration
	cleanupSignal chan struct{}
	configChanged chan struct{}
	totalBytes    atomic.Int64
}

type Config struct {
	PublicBaseURL           string
	MaxImageBytes           int64
	MaxTotalBytes           int64
	CleanupThresholdPercent int
	CleanupInterval         time.Duration
}

type ImageStats struct {
	TotalImages int64
	TotalBytes  int64
}

type VideoStats struct {
	TotalJobs  int64
	Completed  int64
	Failed     int64
	InProgress int64
	Queued     int64
}

func NewService(assets repository.MediaAssetRepository, jobs repository.MediaJobRepository, objects repository.MediaObjectStorage, cleanupLock repository.DistributedLock, cfg Config) *Service {
	return NewServiceWithTickets(assets, jobs, nil, objects, cleanupLock, cfg)
}

// NewServiceWithTickets 构造包含视频上传票据能力的媒体服务。
func NewServiceWithTickets(assets repository.MediaAssetRepository, jobs repository.MediaJobRepository, tickets repository.MediaUploadTicketRepository, objects repository.MediaObjectStorage, cleanupLock repository.DistributedLock, cfg Config) *Service {
	return &Service{
		assets: assets, jobs: jobs, tickets: tickets, objects: objects, cleanupLock: cleanupLock,
		publicBaseURL: strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/"), maxImageBytes: cfg.MaxImageBytes,
		maxTotalBytes: cfg.MaxTotalBytes, cleanupAt: cfg.CleanupThresholdPercent, cleanupEvery: cfg.CleanupInterval,
		cleanupSignal: make(chan struct{}, 1), configChanged: make(chan struct{}, 1),
	}
}

// UpdateConfig 热更新媒体容量和清理策略，不重建底层存储实例。
func (s *Service) UpdateConfig(cfg Config) {
	s.configMu.Lock()
	s.publicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	s.maxImageBytes = cfg.MaxImageBytes
	s.maxTotalBytes = cfg.MaxTotalBytes
	s.cleanupAt = cfg.CleanupThresholdPercent
	s.cleanupEvery = cfg.CleanupInterval
	s.configMu.Unlock()
	select {
	case s.configChanged <- struct{}{}:
	default:
	}
}

// SaveImage 校验并保存一份不可变图片，文件写入失败或元数据落库失败时不会留下半成品。
func (s *Service) SaveImage(ctx context.Context, data []byte) (mediadomain.Asset, error) {
	cfg := s.runtimeConfig()
	if len(data) == 0 || int64(len(data)) > cfg.MaxImageBytes {
		return mediadomain.Asset{}, ErrInvalidImage
	}
	mimeType := http.DetectContentType(data)
	if !supportedImageMIME(mimeType) {
		return mediadomain.Asset{}, ErrInvalidImage
	}
	id, err := newAssetID()
	if err != nil {
		return mediadomain.Asset{}, err
	}
	digest := sha256.Sum256(data)
	createdAt := time.Now().UTC()
	storageKey, err := s.objects.SaveImage(ctx, id, mimeType, data)
	if err != nil {
		return mediadomain.Asset{}, err
	}
	asset := mediadomain.Asset{
		ID: id, Kind: "image", StorageKey: storageKey, MIMEType: mimeType,
		SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), CreatedAt: createdAt,
	}
	if err := s.assets.CreateMediaAsset(ctx, asset); err != nil {
		_ = s.objects.Delete(context.WithoutCancel(ctx), storageKey)
		return mediadomain.Asset{}, err
	}
	if s.totalBytes.Add(asset.SizeBytes) > cleanupThresholdBytes(cfg) {
		select {
		case s.cleanupSignal <- struct{}{}:
		default:
		}
	}
	return asset, nil
}

// PublicImageURL 返回可直接用于图片展示的公开资源地址。
func (s *Service) PublicImageURL(id string) string {
	return s.runtimeConfig().PublicBaseURL + "/v1/media/images/" + id
}

// OpenImage 读取图片元数据和正文，不向调用方暴露实际文件路径。
func (s *Service) OpenImage(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error) {
	asset, err := s.assets.GetMediaAsset(ctx, strings.TrimSpace(id))
	if errors.Is(err, repository.ErrNotFound) {
		return mediadomain.Asset{}, nil, ErrAssetNotFound
	}
	if err != nil {
		return mediadomain.Asset{}, nil, err
	}
	if asset.Kind != "image" {
		return mediadomain.Asset{}, nil, ErrAssetNotFound
	}
	body, err := s.objects.Open(ctx, asset.StorageKey)
	if errors.Is(err, os.ErrNotExist) {
		return mediadomain.Asset{}, nil, ErrAssetNotFound
	}
	if err != nil {
		return mediadomain.Asset{}, nil, err
	}
	return asset, body, nil
}

// AdminListImages 分页返回图片资源列表。
func (s *Service) AdminListImages(ctx context.Context, page, pageSize int, search string) ([]mediadomain.Asset, int64, error) {
	return s.assets.ListMediaAssets(ctx, repository.MediaAssetListQuery{Page: mediaPageQuery(page, pageSize, search, repository.SortQuery{})})
}

// AdminListVideoJobs 分页返回视频任务列表。
func (s *Service) AdminListVideoJobs(ctx context.Context, page, pageSize int, search, status string, sort repository.SortQuery) ([]mediadomain.Job, int64, error) {
	if s.jobs == nil {
		return nil, 0, ErrMediaJobsUnavailable
	}
	status = strings.TrimSpace(status)
	if !validMediaStatus(status) || !repository.IsValidSort(sort, "prompt", "model", "status", "progress", "spec", "account", "createdAt", "completedAt") {
		return nil, 0, ErrInvalidFilter
	}
	return s.jobs.ListMediaJobs(ctx, repository.MediaJobListQuery{
		Page:   mediaPageQuery(page, pageSize, search, sort),
		Filter: repository.MediaJobListFilter{Status: status},
	})
}

// AdminImageStats 返回图片统计信息。
func (s *Service) AdminImageStats(ctx context.Context) (ImageStats, error) {
	stats, err := s.assets.SummarizeMediaAssets(ctx)
	if err != nil {
		return ImageStats{}, err
	}
	return ImageStats{TotalImages: stats.TotalImages, TotalBytes: stats.TotalBytes}, nil
}

// AdminDeleteImages 删除管理员明确选择的图片对象及其元数据。
// 不存在或已被并发清理的图片按幂等成功处理；非图片资产不会被删除。
func (s *Service) AdminDeleteImages(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return 0, ErrInvalidImageSelection
	}
	unique := make(map[string]struct{}, len(ids))
	assets := make([]mediadomain.Asset, 0, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return 0, ErrInvalidImageSelection
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		asset, err := s.assets.GetMediaAsset(ctx, id)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if asset.Kind == "image" {
			assets = append(assets, asset)
		}
	}

	deleted := 0
	for _, asset := range assets {
		if err := s.objects.Delete(ctx, asset.StorageKey); err != nil && !errors.Is(err, os.ErrNotExist) {
			return deleted, err
		}
		if err := s.assets.DeleteMediaAsset(ctx, asset.ID); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	if total, err := s.assets.TotalMediaAssetBytes(ctx); err == nil {
		s.totalBytes.Store(total)
	}
	return deleted, nil
}

// AdminVideoStats 返回视频任务统计信息。
func (s *Service) AdminVideoStats(ctx context.Context) (VideoStats, error) {
	if s.jobs == nil {
		return VideoStats{}, ErrMediaJobsUnavailable
	}
	stats, err := s.jobs.SummarizeMediaJobs(ctx)
	if err != nil {
		return VideoStats{}, err
	}
	return VideoStats{
		TotalJobs: stats.TotalJobs, Completed: stats.Completed, Failed: stats.Failed,
		InProgress: stats.InProgress, Queued: stats.Queued,
	}, nil
}

// AdminDeleteVideoJobs 删除管理员明确选择的终态视频任务。
// 成功任务若已归档本地视频，则同步撤销上传票据并删除对象和媒体元数据；审计记录独立保留。
func (s *Service) AdminDeleteVideoJobs(ctx context.Context, ids []string) (int, error) {
	if s.jobs == nil {
		return 0, ErrMediaJobsUnavailable
	}
	if len(ids) == 0 || len(ids) > 100 {
		return 0, ErrInvalidVideoSelection
	}
	unique := make(map[string]struct{}, len(ids))
	normalized := make([]string, 0, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return 0, ErrInvalidVideoSelection
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		normalized = append(normalized, id)
	}
	jobs, err := s.jobs.GetMediaJobsByIDs(ctx, normalized)
	if err != nil {
		return 0, err
	}
	for _, job := range jobs {
		if job.Status != mediadomain.StatusCompleted && job.Status != mediadomain.StatusFailed {
			return 0, ErrActiveVideoSelection
		}
	}

	deleted := 0
	for _, job := range jobs {
		if s.tickets != nil {
			if err := s.tickets.DeleteUploadTicketsByJobID(ctx, job.ID); err != nil {
				return deleted, err
			}
		}
		if err := s.deleteVideoAsset(ctx, job.ResultAssetID); err != nil {
			return deleted, err
		}
		if err := s.jobs.DeleteMediaJob(ctx, job.ID); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	if total, err := s.assets.TotalMediaAssetBytes(ctx); err == nil {
		s.totalBytes.Store(total)
	}
	return deleted, nil
}

// deleteVideoAsset 删除任务绑定的本地视频对象与元数据；缺失对象按幂等成功处理。
func (s *Service) deleteVideoAsset(ctx context.Context, assetID string) error {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return nil
	}
	asset, err := s.assets.GetMediaAsset(ctx, assetID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if asset.Kind != "video" {
		return nil
	}
	if err := s.objects.Delete(ctx, asset.StorageKey); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.assets.DeleteMediaAsset(ctx, asset.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	return nil
}

func mediaPageQuery(page, pageSize int, search string, sort repository.SortQuery) repository.PageQuery {
	page, pageSize = repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
	return repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: strings.TrimSpace(search), Sort: sort}
}

func validMediaStatus(status string) bool {
	switch mediadomain.Status(status) {
	case "", mediadomain.StatusQueued, mediadomain.StatusInProgress, mediadomain.StatusCompleted, mediadomain.StatusFailed:
		return true
	default:
		return false
	}
}

// RunCleanup 响应容量阈值并按周期清理最旧媒体资源。
func (s *Service) RunCleanup(ctx context.Context, onError func(error)) {
	cfg := s.runtimeConfig()
	if total, err := s.assets.TotalMediaAssetBytes(ctx); err == nil {
		s.totalBytes.Store(total)
		if total > cleanupThresholdBytes(cfg) {
			if _, cleanupErr := s.Cleanup(ctx); cleanupErr != nil && onError != nil {
				onError(cleanupErr)
			}
		}
	} else if onError != nil {
		onError(err)
	}
	ticker := time.NewTicker(cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.cleanupSignal:
		case <-s.configChanged:
			cfg = s.runtimeConfig()
			ticker.Reset(cfg.CleanupInterval)
		}
		cfg = s.runtimeConfig()
		cleanupCtx, cancel := context.WithTimeout(ctx, min(cfg.CleanupInterval, 5*time.Minute))
		_, err := s.Cleanup(cleanupCtx)
		cancel()
		if err != nil && onError != nil {
			onError(err)
		}
	}
}

const (
	cleanupAssetBatchSize  = 200
	cleanupTicketBatchSize = 200
	// cleanupTicketMaxBatchesPerRun 限制单次 Cleanup 调用中过期票据的删除批次数。
	// 超出部分留给后续直接/后台清理继续回收，避免一次调用无界垄断。
	cleanupTicketMaxBatchesPerRun = 1
)

// Cleanup 在跨实例锁保护下删除最旧媒体资产，直到回落到自动清理阈值。
// 同时有界清理过期上传票据。受保护资产被跳过；通过 offset 前进避免整页受保护时死循环。
func (s *Service) Cleanup(ctx context.Context) (int, error) {
	cfg := s.runtimeConfig()
	if s.cleanupLock != nil {
		release, acquired, err := s.cleanupLock.Acquire(ctx, "media:cleanup", 30*time.Minute)
		if err != nil || !acquired {
			return 0, err
		}
		defer release()
	}
	// 过期票据回收：不触碰未过期票据与已登记媒体资产；单次调用有批次数上限。
	if s.tickets != nil {
		for batch := 0; batch < cleanupTicketMaxBatchesPerRun; batch++ {
			n, err := s.tickets.DeleteExpiredUploadTickets(ctx, time.Now().UTC(), cleanupTicketBatchSize)
			if err != nil {
				return 0, err
			}
			if n < int64(cleanupTicketBatchSize) {
				break
			}
		}
	}
	total, err := s.assets.TotalMediaAssetBytes(ctx)
	if err != nil {
		return 0, err
	}
	s.totalBytes.Store(total)
	threshold := cleanupThresholdBytes(cfg)
	if total <= threshold {
		return 0, nil
	}
	deleted := 0
	offset := 0
	for total > threshold {
		values, err := s.assets.ListOldestMediaAssets(ctx, offset, cleanupAssetBatchSize)
		if err != nil {
			return deleted, err
		}
		if len(values) == 0 {
			break
		}
		protected, protErr := s.assets.ListProtectedMediaAssetIDs(ctx)
		if protErr != nil {
			return deleted, protErr
		}
		deletedInBatch := 0
		for _, asset := range values {
			if total <= threshold {
				break
			}
			if _, skip := protected[asset.ID]; skip {
				continue
			}
			if err := s.objects.Delete(ctx, asset.StorageKey); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return deleted, fmt.Errorf("媒体对象缺失，已保留共享元数据: %s: %w", asset.StorageKey, err)
				}
				return deleted, err
			}
			if err := s.assets.DeleteMediaAsset(ctx, asset.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
				return deleted, err
			}
			total = max(0, total-asset.SizeBytes)
			deleted++
			deletedInBatch++
		}
		if deletedInBatch == 0 {
			// 本页无可删资产：前进 offset 以越过受保护前缀，避免无限循环。
			if len(values) < cleanupAssetBatchSize {
				break
			}
			offset += len(values)
			continue
		}
		// 删除后最旧顺序变化，从头部重新扫描。
		offset = 0
	}
	s.totalBytes.Store(total)
	return deleted, nil
}

func (s *Service) runtimeConfig() Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return Config{
		PublicBaseURL: s.publicBaseURL,
		MaxImageBytes: s.maxImageBytes, MaxTotalBytes: s.maxTotalBytes,
		CleanupThresholdPercent: s.cleanupAt, CleanupInterval: s.cleanupEvery,
	}
}

func cleanupThresholdBytes(cfg Config) int64 {
	return cfg.MaxTotalBytes * int64(cfg.CleanupThresholdPercent) / 100
}

func newAssetID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成媒体资源 ID: %w", err)
	}
	return "img_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func supportedImageMIME(value string) bool {
	switch value {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}
