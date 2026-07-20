package relational

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ClientKeyRepository struct{ db *Database }

func NewClientKeyRepository(db *Database) *ClientKeyRepository { return &ClientKeyRepository{db: db} }

func (r *ClientKeyRepository) List(ctx context.Context, input repository.ClientKeyListQuery) ([]clientkey.Key, int64, error) {
	var total int64
	query := r.db.db.WithContext(ctx).Model(&clientKeyModel{})
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(name) LIKE ? OR LOWER(prefix) LIKE ?", pattern, pattern)
	}
	switch input.Filter.Status {
	case "active":
		query = query.Where("enabled = ? AND (expires_at IS NULL OR expires_at > ?)", true, input.Filter.Now)
	case "disabled":
		query = query.Where("enabled = ?", false)
	case "expired":
		query = query.Where("enabled = ? AND expires_at IS NOT NULL AND expires_at <= ?", true, input.Filter.Now)
	}
	switch input.Filter.ModelScope {
	case "all":
		query = query.Where("NOT EXISTS (SELECT 1 FROM client_key_models permission WHERE permission.client_key_id = client_keys.id)")
	case "restricted":
		query = query.Where("EXISTS (SELECT 1 FROM client_key_models permission WHERE permission.client_key_id = client_keys.id)")
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []clientKeyModel
	// 列表查询不读取可恢复密文，只有管理员显式复制时才按 ID 加载。
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"name":          {expression: "LOWER(client_keys.name)"},
		"prefix":        {expression: "client_keys.prefix"},
		"status":        {expression: "CASE WHEN client_keys.enabled = FALSE THEN 1 WHEN client_keys.expires_at IS NOT NULL AND client_keys.expires_at <= CURRENT_TIMESTAMP THEN 2 ELSE 0 END"},
		"rpmLimit":      {expression: "client_keys.rpm_limit"},
		"maxConcurrent": {expression: "client_keys.max_concurrent"},
		"billingLimit":  {expression: "client_keys.billing_limit_usd_ticks", defaultDirection: repository.SortDescending},
		"expiresAt":     {expression: "client_keys.expires_at", nullsLast: true, defaultDirection: repository.SortDescending},
		"lastUsedAt":    {expression: "client_keys.last_used_at", nullsLast: true, defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "client_keys.created_at", defaultDirection: repository.SortDescending}, "client_keys.id")
	if err := query.Select("id", "name", "prefix", "enabled", "expires_at", "rpm_limit", "max_concurrent", "billing_limit_usd_ticks", "billed_usage_usd_ticks", "reserved_usage_usd_ticks", "last_used_at", "created_at", "updated_at").Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	permissions, err := r.allowedModelsForKeys(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	out := make([]clientkey.Key, 0, len(rows))
	for _, row := range rows {
		out = append(out, toClientKeyDomain(row, permissions[row.ID]))
	}
	return out, total, nil
}

func (r *ClientKeyRepository) UpdateManyEnabled(ctx context.Context, ids []uint64, enabled bool) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	result := r.db.db.WithContext(ctx).Model(&clientKeyModel{}).Where("id IN ?", ids).Update("enabled", enabled)
	return result.RowsAffected, result.Error
}

func (r *ClientKeyRepository) Create(ctx context.Context, value clientkey.Key) (clientkey.Key, error) {
	row := clientKeyModel{Name: value.Name, Prefix: value.Prefix, SecretHash: value.SecretHash, EncryptedSecret: value.EncryptedSecret, Enabled: value.Enabled, ExpiresAt: value.ExpiresAt, RPMLimit: value.RPMLimit, MaxConcurrent: value.MaxConcurrent, BillingLimitUSDTicks: value.BillingLimitUSDTicks, BilledUsageUSDTicks: value.BilledUsageUSDTicks, ReservedUsageUSDTicks: value.ReservedUsageUSDTicks}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		// GORM 会把带 default tag 的零值替换为数据库默认值；这里显式写回 0，
		// 使其稳定表示该维度无限制。
		unlimited := make(map[string]any, 2)
		if value.RPMLimit == 0 {
			unlimited["rpm_limit"] = 0
			row.RPMLimit = 0
		}
		if value.MaxConcurrent == 0 {
			unlimited["max_concurrent"] = 0
			row.MaxConcurrent = 0
		}
		if len(unlimited) > 0 {
			if err := tx.Model(&row).Updates(unlimited).Error; err != nil {
				return err
			}
		}
		return replacePermissions(tx, row.ID, value.AllowedModels)
	})
	if err != nil {
		return clientkey.Key{}, mapError(err)
	}
	return toClientKeyDomain(row, value.AllowedModels), nil
}

func (r *ClientKeyRepository) Get(ctx context.Context, id uint64) (clientkey.Key, error) {
	var row clientKeyModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return clientkey.Key{}, mapError(err)
	}
	models, err := r.allowedModels(ctx, id)
	if err != nil {
		return clientkey.Key{}, err
	}
	return toClientKeyDomain(row, models), nil
}

func (r *ClientKeyRepository) GetByPrefix(ctx context.Context, prefix string) (clientkey.Key, error) {
	var row clientKeyModel
	if err := r.db.db.WithContext(ctx).Where("prefix = ?", prefix).First(&row).Error; err != nil {
		return clientkey.Key{}, mapError(err)
	}
	models, err := r.allowedModels(ctx, row.ID)
	if err != nil {
		return clientkey.Key{}, err
	}
	return toClientKeyDomain(row, models), nil
}

func (r *ClientKeyRepository) Update(ctx context.Context, value clientkey.Key) (clientkey.Key, error) {
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&clientKeyModel{}).Where("id = ?", value.ID).Updates(map[string]any{
			"name": value.Name, "enabled": value.Enabled, "expires_at": value.ExpiresAt,
			"rpm_limit": value.RPMLimit, "max_concurrent": value.MaxConcurrent,
			"billing_limit_usd_ticks": value.BillingLimitUSDTicks, "updated_at": time.Now().UTC(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return replacePermissions(tx, value.ID, value.AllowedModels)
	})
	if err != nil {
		return clientkey.Key{}, mapError(err)
	}
	return r.Get(ctx, value.ID)
}

func (r *ClientKeyRepository) Delete(ctx context.Context, id uint64) error {
	result := r.db.db.WithContext(ctx).Delete(&clientKeyModel{}, id)
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *ClientKeyRepository) DeleteMany(ctx context.Context, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	result := r.db.db.WithContext(ctx).Where("id IN ?", ids).Delete(&clientKeyModel{})
	return result.RowsAffected, result.Error
}

func (r *ClientKeyRepository) Touch(ctx context.Context, id uint64) error {
	now := time.Now().UTC()
	return r.db.db.WithContext(ctx).Model(&clientKeyModel{}).Where("id = ?", id).Update("last_used_at", &now).Error
}

// ReserveBillingUsage 在数据库中原子占用本次请求的最大预计费用。
func (r *ClientKeyRepository) ReserveBillingUsage(ctx context.Context, id uint64, eventID string, amount int64, expiresAt time.Time) (bool, error) {
	if id == 0 || eventID == "" || amount <= 0 {
		return false, repository.ErrConflict
	}
	reserved := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := cleanupExpiredBillingReservations(tx, id, time.Now().UTC()); err != nil {
			return err
		}
		var existing billingReservationModel
		err := tx.Where("event_id = ?", eventID).First(&existing).Error
		if err == nil {
			if existing.ClientKeyID == id && existing.Amount == amount {
				reserved = true
				return nil
			}
			return repository.ErrConflict
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		result := tx.Model(&clientKeyModel{}).
			Where(`id = ? AND billing_limit_usd_ticks > 0 AND ? <= CASE
				WHEN billed_usage_usd_ticks >= billing_limit_usd_ticks THEN 0
				WHEN reserved_usage_usd_ticks >= billing_limit_usd_ticks - billed_usage_usd_ticks THEN 0
				ELSE billing_limit_usd_ticks - billed_usage_usd_ticks - reserved_usage_usd_ticks
			END`, id, amount).
			UpdateColumn("reserved_usage_usd_ticks", gorm.Expr("reserved_usage_usd_ticks + ?", amount))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var key clientKeyModel
			if err := tx.Select("id", "billing_limit_usd_ticks").First(&key, id).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return repository.ErrNotFound
				}
				return err
			}
			if key.BillingLimitUSDTicks == 0 {
				return nil
			}
			return repository.ErrLimitExceeded
		}
		reservation := billingReservationModel{EventID: eventID, ClientKeyID: id, Amount: amount, ExpiresAt: expiresAt, CreatedAt: time.Now().UTC()}
		if err := tx.Create(&reservation).Error; err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if !errors.Is(mapError(err), repository.ErrConflict) {
		return reserved, err
	}
	var existing billingReservationModel
	if lookupErr := r.db.db.WithContext(ctx).Where("event_id = ?", eventID).First(&existing).Error; lookupErr == nil && existing.ClientKeyID == id && existing.Amount == amount {
		return true, nil
	}
	return false, repository.ErrConflict
}

// CancelBillingReservation 释放尚未进入审计结算的请求预留。
func (r *ClientKeyRepository) CancelBillingReservation(ctx context.Context, eventID string) error {
	if eventID == "" {
		return nil
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var reservation billingReservationModel
		if err := tx.Where("event_id = ?", eventID).First(&reservation).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if err := lockClientKey(tx, reservation.ClientKeyID); err != nil {
			return err
		}
		if err := decrementReservedUsage(tx, reservation.ClientKeyID, reservation.Amount); err != nil {
			return err
		}
		result := tx.Where("event_id = ?", eventID).Delete(&billingReservationModel{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrConflict
		}
		return nil
	})
}

// CleanupExpiredBillingReservations 分批释放进程异常遗留的过期预留。
func (r *ClientKeyRepository) CleanupExpiredBillingReservations(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var keyIDs []uint64
	if err := r.db.db.WithContext(ctx).Model(&billingReservationModel{}).Distinct("client_key_id").Where("expires_at <= ?", now).Limit(limit).Pluck("client_key_id", &keyIDs).Error; err != nil {
		return 0, err
	}
	cleaned := 0
	for _, keyID := range keyIDs {
		err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			before, err := expiredBillingReservationCount(tx, keyID, now)
			if err != nil {
				return err
			}
			if err := cleanupExpiredBillingReservations(tx, keyID, now); err != nil {
				return err
			}
			cleaned += int(before)
			return nil
		})
		if err != nil {
			return cleaned, err
		}
	}
	return cleaned, nil
}

func cleanupExpiredBillingReservations(tx *gorm.DB, keyID uint64, now time.Time) error {
	if err := lockClientKey(tx, keyID); err != nil {
		return err
	}
	var amount int64
	if err := tx.Model(&billingReservationModel{}).Where("client_key_id = ? AND expires_at <= ?", keyID, now).Select("COALESCE(SUM(amount), 0)").Scan(&amount).Error; err != nil || amount <= 0 {
		return err
	}
	result := tx.Where("client_key_id = ? AND expires_at <= ?", keyID, now).Delete(&billingReservationModel{})
	if result.Error != nil || result.RowsAffected == 0 {
		return result.Error
	}
	return decrementReservedUsage(tx, keyID, amount)
}

func lockClientKey(tx *gorm.DB, keyID uint64) error {
	var key clientKeyModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&key, keyID).Error; err != nil {
		return mapError(err)
	}
	return nil
}

func expiredBillingReservationCount(tx *gorm.DB, keyID uint64, now time.Time) (int64, error) {
	var count int64
	err := tx.Model(&billingReservationModel{}).Where("client_key_id = ? AND expires_at <= ?", keyID, now).Count(&count).Error
	return count, err
}

func decrementReservedUsage(tx *gorm.DB, keyID uint64, amount int64) error {
	return tx.Model(&clientKeyModel{}).Where("id = ?", keyID).UpdateColumn(
		"reserved_usage_usd_ticks",
		gorm.Expr("CASE WHEN reserved_usage_usd_ticks <= ? THEN 0 ELSE reserved_usage_usd_ticks - ? END", amount, amount),
	).Error
}

func (r *ClientKeyRepository) allowedModels(ctx context.Context, keyID uint64) ([]uint64, error) {
	var rows []clientKeyModelPermission
	if err := r.db.db.WithContext(ctx).Where("client_key_id = ?", keyID).Order("model_route_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]uint64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ModelRouteID)
	}
	return out, nil
}

func (r *ClientKeyRepository) allowedModelsForKeys(ctx context.Context, keyIDs []uint64) (map[uint64][]uint64, error) {
	result := make(map[uint64][]uint64, len(keyIDs))
	if len(keyIDs) == 0 {
		return result, nil
	}
	var rows []clientKeyModelPermission
	if err := r.db.db.WithContext(ctx).Where("client_key_id IN ?", keyIDs).Order("client_key_id ASC, model_route_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ClientKeyID] = append(result[row.ClientKeyID], row.ModelRouteID)
	}
	return result, nil
}

func replacePermissions(tx *gorm.DB, keyID uint64, modelIDs []uint64) error {
	if err := tx.Where("client_key_id = ?", keyID).Delete(&clientKeyModelPermission{}).Error; err != nil {
		return err
	}
	rows := make([]clientKeyModelPermission, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		rows = append(rows, clientKeyModelPermission{ClientKeyID: keyID, ModelRouteID: modelID})
	}
	if len(rows) > 0 {
		return tx.CreateInBatches(rows, 200).Error
	}
	return nil
}
