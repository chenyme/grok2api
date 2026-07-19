package relational

import (
	"context"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
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

// ListMediaAssets 通过字段投影返回符合筛选条件的稳定分页结果。
// 默认仅列出图片，避免管理端图库混入视频资产。
func (r *MediaAssetRepository) ListMediaAssets(ctx context.Context, input repository.MediaAssetListQuery) ([]media.Asset, int64, error) {
	query := r.db.db.WithContext(ctx).Model(&mediaAssetModel{}).Where("kind = ?", "image")
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(id) LIKE ? OR LOWER(kind) LIKE ? OR LOWER(mime_type) LIKE ? OR LOWER(sha256) LIKE ?", pattern, pattern, pattern, pattern)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []mediaAssetModel
	if err := query.Select("id", "kind", "mime_type", "size_bytes", "sha256", "created_at").Order("created_at DESC, id DESC").Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	values := make([]media.Asset, 0, len(rows))
	for _, row := range rows {
		values = append(values, media.Asset{
			ID: row.ID, Kind: row.Kind, StorageKey: row.StorageKey, MIMEType: row.MIMEType,
			SizeBytes: row.SizeBytes, SHA256: row.SHA256, CreatedAt: row.CreatedAt,
		})
	}
	return values, total, nil
}

// SummarizeMediaAssets 通过单次聚合查询返回图片数量和全部媒体（含视频）存储占用。
func (r *MediaAssetRepository) SummarizeMediaAssets(ctx context.Context) (repository.MediaAssetStats, error) {
	var stats repository.MediaAssetStats
	err := r.db.db.WithContext(ctx).Model(&mediaAssetModel{}).
		Select("COALESCE(SUM(CASE WHEN kind = 'image' THEN 1 ELSE 0 END), 0) AS total_images, COALESCE(SUM(size_bytes), 0) AS total_bytes").
		Scan(&stats).Error
	return stats, err
}

func (r *MediaAssetRepository) TotalMediaAssetBytes(ctx context.Context) (int64, error) {
	var total int64
	err := r.db.db.WithContext(ctx).Model(&mediaAssetModel{}).Select("COALESCE(SUM(size_bytes), 0)").Scan(&total).Error
	return total, err
}

func (r *MediaAssetRepository) ListOldestMediaAssets(ctx context.Context, offset, limit int) ([]media.Asset, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	var rows []mediaAssetModel
	if err := r.db.db.WithContext(ctx).Order("created_at ASC, id ASC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
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

// ListProtectedMediaAssetIDs 返回不可清理的资产：进行中视频任务结果、未消费且未过期的上传票据。
func (r *MediaAssetRepository) ListProtectedMediaAssetIDs(ctx context.Context) (map[string]struct{}, error) {
	protected := make(map[string]struct{})
	var jobAssetIDs []string
	if err := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("result_asset_id <> '' AND status IN ?", []string{string(media.StatusQueued), string(media.StatusInProgress)}).
		Pluck("result_asset_id", &jobAssetIDs).Error; err != nil {
		return nil, err
	}
	for _, id := range jobAssetIDs {
		if id != "" {
			protected[id] = struct{}{}
		}
	}
	var ticketAssetIDs []string
	now := time.Now().UTC()
	if err := r.db.db.WithContext(ctx).Model(&mediaUploadTicketModel{}).
		Where("consumed_at IS NULL AND expires_at > ?", now).
		Pluck("asset_id", &ticketAssetIDs).Error; err != nil {
		return nil, err
	}
	for _, id := range ticketAssetIDs {
		if id != "" {
			protected[id] = struct{}{}
		}
	}
	return protected, nil
}

type MediaUploadTicketRepository struct{ db *Database }

func NewMediaUploadTicketRepository(db *Database) *MediaUploadTicketRepository {
	return &MediaUploadTicketRepository{db: db}
}

func (r *MediaUploadTicketRepository) CreateUploadTicket(ctx context.Context, ticket repository.MediaUploadTicket) error {
	row := mediaUploadTicketModel{
		TokenHash: ticket.TokenHash, AssetID: ticket.AssetID, JobID: ticket.JobID,
		MaxBytes: ticket.MaxBytes, AllowedMIME: ticket.AllowedMIME,
		ExpiresAt: ticket.ExpiresAt, ConsumedAt: ticket.ConsumedAt, CreatedAt: ticket.CreatedAt,
	}
	return r.db.db.WithContext(ctx).Create(&row).Error
}

func (r *MediaUploadTicketRepository) GetUploadTicketByHash(ctx context.Context, tokenHash string) (repository.MediaUploadTicket, error) {
	var row mediaUploadTicketModel
	if err := r.db.db.WithContext(ctx).Where("token_hash = ?", tokenHash).First(&row).Error; err != nil {
		return repository.MediaUploadTicket{}, mapError(err)
	}
	return ticketToDomain(row), nil
}

func (r *MediaUploadTicketRepository) ConsumeUploadTicket(ctx context.Context, tokenHash string, now time.Time) (repository.MediaUploadTicket, bool, error) {
	var ticket repository.MediaUploadTicket
	consumed := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&mediaUploadTicketModel{}).
			Where("token_hash = ? AND consumed_at IS NULL AND expires_at > ?", tokenHash, now).
			Update("consumed_at", now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		var row mediaUploadTicketModel
		if err := tx.Where("token_hash = ?", tokenHash).First(&row).Error; err != nil {
			return err
		}
		ticket = ticketToDomain(row)
		consumed = true
		return nil
	})
	if err != nil {
		return repository.MediaUploadTicket{}, false, err
	}
	return ticket, consumed, nil
}

// ReleaseUploadTicket 撤销一次尚未落资产的消费，使同一 token 可再次 PUT。
func (r *MediaUploadTicketRepository) ReleaseUploadTicket(ctx context.Context, tokenHash string) (bool, error) {
	// 使用 map 写入 NULL：GORM Update(column, nil) 可能被忽略。
	result := r.db.db.WithContext(ctx).Model(&mediaUploadTicketModel{}).
		Where("token_hash = ? AND consumed_at IS NOT NULL", tokenHash).
		Updates(map[string]any{"consumed_at": nil})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// DeleteUploadTicketByHash 按 token_hash 精确删除票据；目标不存在时幂等返回 nil。
func (r *MediaUploadTicketRepository) DeleteUploadTicketByHash(ctx context.Context, tokenHash string) error {
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return nil
	}
	return r.db.db.WithContext(ctx).Where("token_hash = ?", tokenHash).Delete(&mediaUploadTicketModel{}).Error
}

// DeleteUploadTicketsByJobID 删除任务关联的上传票据，终止后续延迟上传。
func (r *MediaUploadTicketRepository) DeleteUploadTicketsByJobID(ctx context.Context, jobID string) error {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil
	}
	return r.db.db.WithContext(ctx).Where("job_id = ?", jobID).Delete(&mediaUploadTicketModel{}).Error
}

// DeleteExpiredUploadTickets 删除已过期票据行（已消费或未消费均可）；未过期票据与媒体资产不受影响。
func (r *MediaUploadTicketRepository) DeleteExpiredUploadTickets(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var hashes []string
	if err := r.db.db.WithContext(ctx).Model(&mediaUploadTicketModel{}).
		Where("expires_at < ?", before).
		Order("expires_at ASC").Limit(limit).Pluck("token_hash", &hashes).Error; err != nil {
		return 0, err
	}
	if len(hashes) == 0 {
		return 0, nil
	}
	result := r.db.db.WithContext(ctx).Where("token_hash IN ?", hashes).Delete(&mediaUploadTicketModel{})
	return result.RowsAffected, result.Error
}

func (r *MediaUploadTicketRepository) BindJobResultAsset(ctx context.Context, jobID, assetID string) error {
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("id = ?", jobID).
		Updates(map[string]any{"result_asset_id": assetID, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func ticketToDomain(row mediaUploadTicketModel) repository.MediaUploadTicket {
	return repository.MediaUploadTicket{
		TokenHash: row.TokenHash, AssetID: row.AssetID, JobID: row.JobID,
		MaxBytes: row.MaxBytes, AllowedMIME: row.AllowedMIME,
		ExpiresAt: row.ExpiresAt, ConsumedAt: row.ConsumedAt, CreatedAt: row.CreatedAt,
	}
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

// GetMediaJobsByIDs 返回管理端明确选择的完整视频任务。
func (r *MediaJobRepository) GetMediaJobsByIDs(ctx context.Context, ids []string) ([]media.Job, error) {
	if len(ids) == 0 {
		return []media.Job{}, nil
	}
	var rows []mediaJobModel
	if err := r.db.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]media.Job, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaJobToDomain(row))
	}
	return values, nil
}

func (r *MediaJobRepository) UpdateMediaJob(ctx context.Context, value media.Job) error {
	updates := mediaJobFromDomain(value)
	query := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).Where("id = ?", value.ID)
	if value.ClaimToken != "" {
		query = query.Where("claim_token = ?", value.ClaimToken)
	}
	result := query.Select("request_id", "client_key_name", "account_id", "account_name", "egress_node_id", "egress_node_name", "egress_scope", "egress_mode", "provider", "model", "model_route_id", "upstream_model", "prompt", "seconds", "size", "quality", "status", "progress", "input_json", "upstream_url", "result_asset_id", "content_type", "error_code", "error_message", "lease_until", "claim_token", "updated_at", "completed_at", "usage_recorded_at").Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// DeleteMediaJob 仅删除终态任务，避免管理端与视频 Worker 并发修改同一任务。
func (r *MediaJobRepository) DeleteMediaJob(ctx context.Context, id string) error {
	result := r.db.db.WithContext(ctx).
		Where("id = ? AND status IN ?", id, []media.Status{media.StatusCompleted, media.StatusFailed}).
		Delete(&mediaJobModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// ListMediaJobs 通过固定搜索字段和排序白名单返回稳定分页结果。
func (r *MediaJobRepository) ListMediaJobs(ctx context.Context, input repository.MediaJobListQuery) ([]media.Job, int64, error) {
	query := r.db.db.WithContext(ctx).Model(&mediaJobModel{})
	if input.Filter.Status != "" {
		query = query.Where("status = ?", input.Filter.Status)
	}
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(id) LIKE ? OR LOWER(prompt) LIKE ? OR LOWER(model) LIKE ? OR LOWER(account_name) LIKE ? OR LOWER(client_key_name) LIKE ?", pattern, pattern, pattern, pattern, pattern)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []mediaJobModel
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"prompt":      {expression: "LOWER(prompt)"},
		"model":       {expression: "LOWER(model)"},
		"status":      {expression: "status"},
		"progress":    {expression: "progress", defaultDirection: repository.SortDescending},
		"spec":        {expression: "LOWER(size) || ' ' || LOWER(quality)"},
		"account":     {expression: "LOWER(account_name)"},
		"createdAt":   {expression: "created_at", defaultDirection: repository.SortDescending},
		"completedAt": {expression: "completed_at", nullsLast: true, defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "created_at", defaultDirection: repository.SortDescending}, "id")
	if err := query.Select(
		"id", "client_key_name", "account_name", "model", "prompt", "seconds", "size", "quality",
		"status", "progress", "result_asset_id", "error_message", "created_at", "completed_at",
	).Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	values := make([]media.Job, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaJobToDomain(row))
	}
	return values, total, nil
}

// SummarizeMediaJobs 通过单次条件聚合查询统计全部任务状态。
func (r *MediaJobRepository) SummarizeMediaJobs(ctx context.Context) (repository.MediaJobStats, error) {
	var stats repository.MediaJobStats
	err := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).Select(`
		COUNT(*) AS total_jobs,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS completed,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS failed,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS in_progress,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS queued`,
		media.StatusCompleted, media.StatusFailed, media.StatusInProgress, media.StatusQueued,
	).Scan(&stats).Error
	return stats, err
}

// ListUnrecordedTerminalMediaJobs 返回尚未完成审计写入的成功或失败任务。
func (r *MediaJobRepository) ListUnrecordedTerminalMediaJobs(ctx context.Context, limit int) ([]media.Job, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var rows []mediaJobModel
	if err := r.db.db.WithContext(ctx).Where("status IN ? AND usage_recorded_at IS NULL", []media.Status{media.StatusCompleted, media.StatusFailed}).Order("completed_at ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]media.Job, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaJobToDomain(row))
	}
	return values, nil
}

func (r *MediaJobRepository) MarkMediaJobUsageRecorded(ctx context.Context, id string, recordedAt time.Time) error {
	terminalStatuses := []media.Status{media.StatusCompleted, media.StatusFailed}
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).Where("id = ? AND status IN ? AND usage_recorded_at IS NULL", id, terminalStatuses).Update("usage_recorded_at", recordedAt)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).Where("id = ? AND status IN ? AND usage_recorded_at IS NOT NULL", id, terminalStatuses).Count(&count).Error; err != nil {
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
		AccountID: mediaJobAccountID(value.AccountID), AccountName: value.AccountName,
		EgressNodeID: value.EgressNodeID, EgressNodeName: value.EgressNodeName, EgressScope: value.EgressScope, EgressMode: value.EgressMode,
		Provider: value.Provider,
		Model:    value.Model, ModelRouteID: value.ModelRouteID, UpstreamModel: value.UpstreamModel,
		Prompt: value.Prompt, Seconds: value.Seconds, Size: value.Size, Quality: value.Quality,
		Status: string(value.Status), Progress: value.Progress, InputJSON: value.InputJSON, UpstreamURL: value.UpstreamURL,
		ResultAssetID: value.ResultAssetID, ContentType: value.ContentType, ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage,
		LeaseUntil: value.LeaseUntil, ClaimToken: value.ClaimToken, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		CompletedAt: value.CompletedAt, UsageRecordedAt: value.UsageRecordedAt,
	}
}

func mediaJobToDomain(row mediaJobModel) media.Job {
	var accountID uint64
	if row.AccountID != nil {
		accountID = *row.AccountID
	}
	return media.Job{
		ID: row.ID, RequestID: row.RequestID, ClientKeyID: row.ClientKeyID, ClientKeyName: row.ClientKeyName,
		AccountID: accountID, AccountName: row.AccountName,
		EgressNodeID: row.EgressNodeID, EgressNodeName: row.EgressNodeName, EgressScope: row.EgressScope, EgressMode: row.EgressMode,
		Provider: row.Provider,
		Model:    row.Model, ModelRouteID: row.ModelRouteID, UpstreamModel: row.UpstreamModel,
		Prompt: row.Prompt, Seconds: row.Seconds, Size: row.Size, Quality: row.Quality,
		Status: media.Status(row.Status), Progress: row.Progress, InputJSON: row.InputJSON, UpstreamURL: row.UpstreamURL,
		ResultAssetID: row.ResultAssetID, ContentType: row.ContentType, ErrorCode: row.ErrorCode, ErrorMessage: row.ErrorMessage,
		LeaseUntil: row.LeaseUntil, ClaimToken: row.ClaimToken, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		CompletedAt: row.CompletedAt, UsageRecordedAt: row.UsageRecordedAt,
	}
}

func mediaJobAccountID(value uint64) *uint64 {
	if value == 0 {
		return nil
	}
	return &value
}
