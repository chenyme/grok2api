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

func (r *ResponseRepository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ownership := tx.Where("expires_at <= ?", now).Delete(&responseOwnershipModel{})
		if ownership.Error != nil {
			return ownership.Error
		}
		states := tx.Where("expires_at <= ?", now).Delete(&webResponseStateModel{})
		deleted = ownership.RowsAffected + states.RowsAffected
		return states.Error
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
