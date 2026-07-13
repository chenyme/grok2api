package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type MediaJobRepository struct{ db *Database }

type MediaAssetRepository struct{ db *Database }

func NewMediaJobRepository(db *Database) *MediaJobRepository { return &MediaJobRepository{db: db} }

func NewMediaAssetRepository(db *Database) *MediaAssetRepository {
	return &MediaAssetRepository{db: db}
}

func (r *MediaAssetRepository) CreateMediaAsset(ctx context.Context, value media.Asset) error {
	row := mediaAssetModel{
		ID: value.ID, Kind: value.Kind, StorageKey: value.StorageKey, MIMEType: value.MIMEType,
		SizeBytes: value.SizeBytes, SHA256: value.SHA256, CreatedAt: value.CreatedAt,
	}
	return r.db.db.WithContext(ctx).Create(&row).Error
}

func (r *MediaAssetRepository) GetMediaAsset(ctx context.Context, id string) (media.Asset, error) {
	var row mediaAssetModel
	if err := r.db.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		return media.Asset{}, mapError(err)
	}
	return media.Asset{
		ID: row.ID, Kind: row.Kind, StorageKey: row.StorageKey, MIMEType: row.MIMEType,
		SizeBytes: row.SizeBytes, SHA256: row.SHA256, CreatedAt: row.CreatedAt,
	}, nil
}

func (r *MediaAssetRepository) TotalMediaAssetBytes(ctx context.Context) (int64, error) {
	var total int64
	err := r.db.db.WithContext(ctx).Model(&mediaAssetModel{}).Select("COALESCE(SUM(size_bytes), 0)").Scan(&total).Error
	return total, err
}

func (r *MediaAssetRepository) ListOldestMediaAssets(ctx context.Context, limit int) ([]media.Asset, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var rows []mediaAssetModel
	if err := r.db.db.WithContext(ctx).Order("created_at ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]media.Asset, 0, len(rows))
	for _, row := range rows {
		values = append(values, media.Asset{
			ID: row.ID, Kind: row.Kind, StorageKey: row.StorageKey, MIMEType: row.MIMEType,
			SizeBytes: row.SizeBytes, SHA256: row.SHA256, CreatedAt: row.CreatedAt,
		})
	}
	return values, nil
}

func (r *MediaAssetRepository) DeleteMediaAsset(ctx context.Context, id string) error {
	result := r.db.db.WithContext(ctx).Where("id = ?", id).Delete(&mediaAssetModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *MediaJobRepository) CreateMediaJob(ctx context.Context, value media.Job) error {
	return r.db.db.WithContext(ctx).Create(mediaJobFromDomain(value)).Error
}

func (r *MediaJobRepository) GetMediaJob(ctx context.Context, id string, clientKeyID uint64) (media.Job, error) {
	var row mediaJobModel
	if err := r.db.db.WithContext(ctx).Where("id = ? AND client_key_id = ?", id, clientKeyID).First(&row).Error; err != nil {
		return media.Job{}, mapError(err)
	}
	return mediaJobToDomain(row), nil
}

func (r *MediaJobRepository) UpdateMediaJob(ctx context.Context, value media.Job) error {
	updates := mediaJobFromDomain(value)
	query := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).Where("id = ?", value.ID)
	if value.ClaimToken != "" {
		query = query.Where("claim_token = ?", value.ClaimToken)
	}
	result := query.Select("request_id", "client_key_name", "account_id", "account_name", "provider", "model", "model_route_id", "upstream_model", "prompt", "seconds", "size", "quality", "status", "progress", "input_json", "upstream_url", "content_type", "error_code", "error_message", "lease_until", "claim_token", "updated_at", "completed_at", "usage_recorded_at").Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *MediaJobRepository) ListUnrecordedCompletedMediaJobs(ctx context.Context, limit int) ([]media.Job, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var rows []mediaJobModel
	if err := r.db.db.WithContext(ctx).Where("status = ? AND usage_recorded_at IS NULL", media.StatusCompleted).Order("completed_at ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]media.Job, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaJobToDomain(row))
	}
	return values, nil
}

func (r *MediaJobRepository) MarkMediaJobUsageRecorded(ctx context.Context, id string, recordedAt time.Time) error {
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).Where("id = ? AND status = ? AND usage_recorded_at IS NULL", id, media.StatusCompleted).Update("usage_recorded_at", recordedAt)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).Where("id = ? AND status = ? AND usage_recorded_at IS NOT NULL", id, media.StatusCompleted).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return repository.ErrNotFound
		}
	}
	return nil
}

func (r *MediaJobRepository) ListRecoverableMediaJobs(ctx context.Context, limit int) ([]media.Job, error) {
	var rows []mediaJobModel
	now := time.Now().UTC()
	if err := r.db.db.WithContext(ctx).Where("status = ? OR (status = ? AND (lease_until IS NULL OR lease_until <= ?))", media.StatusQueued, media.StatusInProgress, now).Order("created_at ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]media.Job, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaJobToDomain(row))
	}
	return values, nil
}

// TryClaimMediaJob 原子认领新任务或租约已过期的任务，避免多实例重复执行。
func (r *MediaJobRepository) TryClaimMediaJob(ctx context.Context, id string, now, leaseUntil time.Time, claimToken string) (media.Job, bool, error) {
	if claimToken == "" {
		return media.Job{}, false, repository.ErrConflict
	}
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("id = ? AND (status = ? OR (status = ? AND (lease_until IS NULL OR lease_until <= ?)))", id, media.StatusQueued, media.StatusInProgress, now).
		Updates(map[string]any{"status": media.StatusInProgress, "lease_until": leaseUntil, "claim_token": claimToken, "updated_at": now})
	if result.Error != nil {
		return media.Job{}, false, result.Error
	}
	if result.RowsAffected == 0 {
		return media.Job{}, false, nil
	}
	var row mediaJobModel
	if err := r.db.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		return media.Job{}, false, mapError(err)
	}
	return mediaJobToDomain(row), true, nil
}

func mediaJobFromDomain(value media.Job) *mediaJobModel {
	return &mediaJobModel{
		ID: value.ID, RequestID: value.RequestID, ClientKeyID: value.ClientKeyID, ClientKeyName: value.ClientKeyName,
		AccountID: value.AccountID, AccountName: value.AccountName, Provider: value.Provider,
		Model: value.Model, ModelRouteID: value.ModelRouteID, UpstreamModel: value.UpstreamModel,
		Prompt: value.Prompt, Seconds: value.Seconds, Size: value.Size, Quality: value.Quality,
		Status: string(value.Status), Progress: value.Progress, InputJSON: value.InputJSON, UpstreamURL: value.UpstreamURL,
		ContentType: value.ContentType, ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage,
		LeaseUntil: value.LeaseUntil, ClaimToken: value.ClaimToken, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		CompletedAt: value.CompletedAt, UsageRecordedAt: value.UsageRecordedAt,
	}
}

func mediaJobToDomain(row mediaJobModel) media.Job {
	return media.Job{
		ID: row.ID, RequestID: row.RequestID, ClientKeyID: row.ClientKeyID, ClientKeyName: row.ClientKeyName,
		AccountID: row.AccountID, AccountName: row.AccountName, Provider: row.Provider,
		Model: row.Model, ModelRouteID: row.ModelRouteID, UpstreamModel: row.UpstreamModel,
		Prompt: row.Prompt, Seconds: row.Seconds, Size: row.Size, Quality: row.Quality,
		Status: media.Status(row.Status), Progress: row.Progress, InputJSON: row.InputJSON, UpstreamURL: row.UpstreamURL,
		ContentType: row.ContentType, ErrorCode: row.ErrorCode, ErrorMessage: row.ErrorMessage,
		LeaseUntil: row.LeaseUntil, ClaimToken: row.ClaimToken, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		CompletedAt: row.CompletedAt, UsageRecordedAt: row.UsageRecordedAt,
	}
}
