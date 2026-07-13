package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type AdminRepository struct{ db *Database }

func NewAdminRepository(db *Database) *AdminRepository { return &AdminRepository{db: db} }

func (r *AdminRepository) Count(ctx context.Context) (int64, error) {
	var count int64
	err := r.db.db.WithContext(ctx).Model(&adminModel{}).Count(&count).Error
	return count, err
}

func (r *AdminRepository) Create(ctx context.Context, value admin.Admin) (admin.Admin, error) {
	row := adminModel{Username: value.Username, PasswordHash: value.PasswordHash}
	if err := r.db.db.WithContext(ctx).Create(&row).Error; err != nil {
		return admin.Admin{}, mapError(err)
	}
	return toAdminDomain(row), nil
}

func (r *AdminRepository) GetByUsername(ctx context.Context, username string) (admin.Admin, error) {
	var row adminModel
	if err := r.db.db.WithContext(ctx).Where("username = ?", username).First(&row).Error; err != nil {
		return admin.Admin{}, mapError(err)
	}
	return toAdminDomain(row), nil
}

func (r *AdminRepository) GetByID(ctx context.Context, id uint64) (admin.Admin, error) {
	var row adminModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return admin.Admin{}, mapError(err)
	}
	return toAdminDomain(row), nil
}

func (r *AdminRepository) UpdatePasswordAndRevokeSessions(ctx context.Context, id uint64, passwordHash string) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&adminModel{}).Where("id = ?", id).Updates(map[string]any{"password_hash": passwordHash, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return tx.Where("admin_id = ?", id).Delete(&adminSessionModel{}).Error
	})
}

type AdminSessionRepository struct{ db *Database }

const maxAdminSessions = 100

func NewAdminSessionRepository(db *Database) *AdminSessionRepository {
	return &AdminSessionRepository{db: db}
}

func (r *AdminSessionRepository) Create(ctx context.Context, value admin.Session) (admin.Session, error) {
	row := adminSessionModel{AdminID: value.AdminID, RefreshTokenHash: value.RefreshTokenHash, ExpiresAt: value.ExpiresAt, CreatedAt: time.Now().UTC()}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := tx.Where("expires_at <= ?", now).Delete(&adminSessionModel{}).Error; err != nil {
			return err
		}
		var staleIDs []uint64
		if err := tx.Model(&adminSessionModel{}).Where("admin_id = ?", value.AdminID).Order("created_at DESC, id DESC").Offset(maxAdminSessions-1).Pluck("id", &staleIDs).Error; err != nil {
			return err
		}
		if len(staleIDs) > 0 {
			if err := tx.Where("id IN ?", staleIDs).Delete(&adminSessionModel{}).Error; err != nil {
				return err
			}
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return admin.Session{}, mapError(err)
	}
	return toSessionDomain(row), nil
}

func (r *AdminSessionRepository) GetByTokenHash(ctx context.Context, tokenHash string) (admin.Session, error) {
	var row adminSessionModel
	if err := r.db.db.WithContext(ctx).Where("refresh_token_hash = ?", tokenHash).First(&row).Error; err != nil {
		return admin.Session{}, mapError(err)
	}
	return toSessionDomain(row), nil
}

func (r *AdminSessionRepository) GetByID(ctx context.Context, id uint64) (admin.Session, error) {
	var row adminSessionModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return admin.Session{}, mapError(err)
	}
	return toSessionDomain(row), nil
}

func (r *AdminSessionRepository) Rotate(ctx context.Context, id uint64, expectedTokenHash, newTokenHash string, expiresAt time.Time) error {
	now := time.Now().UTC()
	result := r.db.db.WithContext(ctx).
		Model(&adminSessionModel{}).
		Where("id = ? AND refresh_token_hash = ? AND expires_at > ?", id, expectedTokenHash, now).
		Updates(map[string]any{
			"refresh_token_hash": newTokenHash,
			"expires_at":         expiresAt,
			"last_used_at":       &now,
		})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected != 1 {
		return repository.ErrConflict
	}
	return nil
}

func (r *AdminSessionRepository) Revoke(ctx context.Context, id uint64) error {
	return r.db.db.WithContext(ctx).Delete(&adminSessionModel{}, id).Error
}

func (r *AdminSessionRepository) RevokeAllByAdmin(ctx context.Context, adminID uint64) error {
	return r.db.db.WithContext(ctx).Where("admin_id = ?", adminID).Delete(&adminSessionModel{}).Error
}
