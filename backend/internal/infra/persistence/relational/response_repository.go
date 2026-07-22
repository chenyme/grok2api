package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type ResponseRepository struct{ db *Database }

func NewResponseRepository(db *Database) *ResponseRepository { return &ResponseRepository{db: db} }

func (r *ResponseRepository) Save(ctx context.Context, value inferencedomain.ResponseOwnership) error {
	row := responseOwnershipModel{
		ResponseID: value.ResponseID, AccountID: value.AccountID,
		ClientKeyID: value.ClientKeyID, Provider: string(value.Provider),
		PromptCacheKey: value.PromptCacheKey, ReasoningReplayKey: value.ReasoningReplayKey,
		ExpiresAt: value.ExpiresAt, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	return r.db.db.WithContext(ctx).Save(&row).Error
}

func (r *ResponseRepository) Get(ctx context.Context, responseID string, clientKeyID uint64, now time.Time) (inferencedomain.ResponseOwnership, error) {
	var row responseOwnershipModel
	if err := r.db.db.WithContext(ctx).Where("response_id = ? AND client_key_id = ? AND expires_at > ?", responseID, clientKeyID, now).First(&row).Error; err != nil {
		return inferencedomain.ResponseOwnership{}, mapError(err)
	}
	return inferencedomain.ResponseOwnership{
		ResponseID: row.ResponseID, AccountID: row.AccountID,
		ClientKeyID: row.ClientKeyID, Provider: account.Provider(row.Provider),
		PromptCacheKey: row.PromptCacheKey, ReasoningReplayKey: row.ReasoningReplayKey,
		ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *ResponseRepository) Delete(ctx context.Context, responseID string, clientKeyID uint64) error {
	result := r.db.db.WithContext(ctx).Where("response_id = ? AND client_key_id = ?", responseID, clientKeyID).Delete(&responseOwnershipModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *ResponseRepository) DeleteExpired(ctx context.Context, now time.Time, ownershipLimit, webStateLimit int) (repository.ResponseCleanupResult, error) {
	result := repository.ResponseCleanupResult{}
	if ownershipLimit > 0 {
		deleted, err := r.deleteExpiredOwnership(ctx, now, ownershipLimit)
		if err != nil {
			return result, err
		}
		result.OwnershipDeleted = deleted
		result.HasMore = result.HasMore || deleted >= int64(ownershipLimit)
	}
	if webStateLimit > 0 {
		deleted, err := r.deleteExpiredWebState(ctx, now, webStateLimit)
		if err != nil {
			return result, err
		}
		result.WebStateDeleted = deleted
		result.HasMore = result.HasMore || deleted >= int64(webStateLimit)
	}
	return result, nil
}

func (r *ResponseRepository) deleteExpiredOwnership(ctx context.Context, now time.Time, limit int) (int64, error) {
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ids := tx.Model(&responseOwnershipModel{}).
			Select("response_id").
			Where("expires_at <= ?", now).
			Order("expires_at ASC, response_id ASC").
			Limit(limit)
		result := tx.Where("response_id IN (?)", ids).Delete(&responseOwnershipModel{})
		deleted = result.RowsAffected
		return result.Error
	})
	return deleted, err
}

func (r *ResponseRepository) deleteExpiredWebState(ctx context.Context, now time.Time, limit int) (int64, error) {
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ids := tx.Model(&webResponseStateModel{}).
			Select("response_id").
			Where("expires_at <= ?", now).
			Order("expires_at ASC, response_id ASC").
			Limit(limit)
		result := tx.Where("response_id IN (?)", ids).Delete(&webResponseStateModel{})
		deleted = result.RowsAffected
		return result.Error
	})
	return deleted, err
}

func (r *ResponseRepository) SaveWebState(ctx context.Context, value inferencedomain.WebResponseState) error {
	row := webResponseStateModel{
		ResponseID: value.ResponseID, AccountID: value.AccountID, ConversationID: value.ConversationID,
		UpstreamParentResponseID: value.UpstreamParentResponseID, ResponseJSON: value.ResponseJSON,
		Status: value.Status, ExpiresAt: value.ExpiresAt, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	return r.db.db.WithContext(ctx).Save(&row).Error
}

func (r *ResponseRepository) GetWebState(ctx context.Context, responseID string, now time.Time) (inferencedomain.WebResponseState, error) {
	var row webResponseStateModel
	if err := r.db.db.WithContext(ctx).Where("response_id = ? AND expires_at > ?", responseID, now).First(&row).Error; err != nil {
		return inferencedomain.WebResponseState{}, mapError(err)
	}
	return inferencedomain.WebResponseState{
		ResponseID: row.ResponseID, AccountID: row.AccountID, ConversationID: row.ConversationID,
		UpstreamParentResponseID: row.UpstreamParentResponseID, ResponseJSON: row.ResponseJSON,
		Status: row.Status, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *ResponseRepository) DeleteWebState(ctx context.Context, responseID string) error {
	result := r.db.db.WithContext(ctx).Where("response_id = ?", responseID).Delete(&webResponseStateModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}
