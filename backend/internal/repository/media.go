package repository

import (
	"context"
	"io"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

type MediaJobRepository interface {
	CreateMediaJob(ctx context.Context, value media.Job) error
	GetMediaJob(ctx context.Context, id string, clientKeyID uint64) (media.Job, error)
	UpdateMediaJob(ctx context.Context, value media.Job) error
	ListRecoverableMediaJobs(ctx context.Context, limit int) ([]media.Job, error)
	ListUnrecordedCompletedMediaJobs(ctx context.Context, limit int) ([]media.Job, error)
	TryClaimMediaJob(ctx context.Context, id string, now, leaseUntil time.Time, claimToken string) (media.Job, bool, error)
	MarkMediaJobUsageRecorded(ctx context.Context, id string, recordedAt time.Time) error
}

// MediaAssetRepository 定义媒体资源元数据持久化能力。
type MediaAssetRepository interface {
	CreateMediaAsset(ctx context.Context, value media.Asset) error
	GetMediaAsset(ctx context.Context, id string) (media.Asset, error)
	TotalMediaAssetBytes(ctx context.Context) (int64, error)
	ListOldestMediaAssets(ctx context.Context, limit int) ([]media.Asset, error)
	DeleteMediaAsset(ctx context.Context, id string) error
}

// MediaObjectStorage 定义媒体二进制对象的存取边界。
type MediaObjectStorage interface {
	SaveImage(ctx context.Context, id, mimeType string, data []byte) (string, error)
	Open(ctx context.Context, storageKey string) (io.ReadCloser, error)
	Delete(ctx context.Context, storageKey string) error
}
