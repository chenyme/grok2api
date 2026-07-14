package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type modelPublicIDMigration struct {
	ID       uint64
	Previous string
	Current  string
}

// ensureCanonicalModelPublicIDs 原位迁移内部路由 ID，保留路由主键和所有下游授权关系。
func (d *Database) ensureCanonicalModelPublicIDs(ctx context.Context) error {
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []modelRouteModel
		if err := tx.Order("id ASC").Find(&rows).Error; err != nil {
			return err
		}
		migrations := make([]modelPublicIDMigration, 0, len(rows))
		for _, row := range rows {
			providerValue := account.Provider(row.Provider)
			publicID, ok := modeldomain.NormalizePublicID(providerValue, row.PublicID)
			if !ok {
				return fmt.Errorf("模型路由 %d 的公开 ID %q 无法规范化到 %s", row.ID, row.PublicID, providerValue)
			}
			if publicID == row.PublicID {
				continue
			}
			migrations = append(migrations, modelPublicIDMigration{ID: row.ID, Previous: row.PublicID, Current: publicID})
		}
		for _, migration := range migrations {
			if err := ensureModelPublicIDNotAlias(tx, migration.Current, migration.ID); err != nil {
				return err
			}
			if err := preserveModelRouteAlias(tx, migration.Previous, migration.ID); err != nil {
				return err
			}
		}
		// 全部先移到不冲突的临时命名空间，允许 A/B 两条路由交换或释放目标名称。
		for _, migration := range migrations {
			temporary := fmt.Sprintf("__grok2api_model_namespace_%d", migration.ID)
			if err := tx.Model(&modelRouteModel{}).Where("id = ?", migration.ID).Update("public_id", temporary).Error; err != nil {
				return mapError(err)
			}
		}
		for _, migration := range migrations {
			if err := tx.Model(&modelRouteModel{}).Where("id = ?", migration.ID).Update("public_id", migration.Current).Error; err != nil {
				return fmt.Errorf("迁移模型路由 %d 到 %q: %w", migration.ID, migration.Current, mapError(err))
			}
		}
		return nil
	})
}

func preserveModelRouteAlias(tx *gorm.DB, alias string, routeID uint64) error {
	alias = strings.TrimSpace(alias)
	if alias == "" || routeID == 0 {
		return nil
	}
	var route modelRouteModel
	if err := tx.Where("public_id = ? AND id <> ?", alias, routeID).First(&route).Error; err == nil {
		return fmt.Errorf("%w: 模型兼容名称 %q 与路由 %d 的规范名称冲突", repository.ErrConflict, alias, route.ID)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var existing modelRouteAliasModel
	err := tx.Where("alias = ?", alias).First(&existing).Error
	if err == nil {
		if existing.ModelRouteID == routeID {
			return nil
		}
		return fmt.Errorf("%w: 模型兼容名称 %q 已绑定路由 %d", repository.ErrConflict, alias, existing.ModelRouteID)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return tx.Create(&modelRouteAliasModel{Alias: alias, ModelRouteID: routeID}).Error
}

func ensureModelPublicIDNotAlias(tx *gorm.DB, publicID string, routeID uint64) error {
	var alias modelRouteAliasModel
	query := tx.Where("alias = ?", publicID)
	if routeID != 0 {
		query = query.Where("model_route_id <> ?", routeID)
	}
	err := query.First(&alias).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: 模型公开 ID %q 已被路由 %d 保留为兼容名称", repository.ErrConflict, publicID, alias.ModelRouteID)
}
