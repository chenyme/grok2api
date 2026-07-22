package relational

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ModelRepository struct {
	db       *Database
	observer repository.InvalidationObserver
}

const availableRoutePredicate = `
	EXISTS (
		SELECT 1 FROM provider_accounts account
		WHERE account.provider = model_routes.provider
			AND account.enabled = ?
			AND account.auth_status = ?
			AND (
				EXISTS (
					SELECT 1 FROM model_route_accounts binding
					WHERE binding.model_route_id = model_routes.id
						AND binding.account_id = account.id
				)
				OR (
					NOT EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id)
					AND EXISTS (
						SELECT 1 FROM account_model_capabilities capability
						WHERE capability.account_id = account.id
							AND capability.upstream_model = model_routes.upstream_model
					)
				)
			)
	)
`

// Build Super 共享能力：Billing paid 或 build_super_entitled；仅 grok_build。
const modelAccountBuildSuperPredicate = `(EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = account.id AND ` + accountPaidBillingSignals + `) OR (account.provider = 'grok_build' AND account.build_super_entitled = TRUE))`
const modelPeerBuildSuperPredicate = `(EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = peer.id AND ` + accountPaidBillingSignals + `) OR (peer.provider = 'grok_build' AND peer.build_super_entitled = TRUE))`

const modelSharedPaidBuildSupportSortExpression = `(model_routes.provider = 'grok_build'
	AND ` + modelAccountBuildSuperPredicate + `
	AND EXISTS (
		SELECT 1
		FROM provider_accounts peer
		JOIN account_model_capabilities peer_capability ON peer_capability.account_id = peer.id AND peer_capability.upstream_model = model_routes.upstream_model
		WHERE peer.provider = model_routes.provider
			AND peer.enabled = TRUE
			AND peer.auth_status = 'active'
			AND ` + modelPeerBuildSuperPredicate + `
	))`

const modelSharedPaidBuildSupportAvailabilityExpression = `(route.provider = 'grok_build'
	AND ` + modelAccountBuildSuperPredicate + `
	AND EXISTS (
		SELECT 1
		FROM provider_accounts peer
		JOIN account_model_capabilities peer_capability ON peer_capability.account_id = peer.id AND peer_capability.upstream_model = route.upstream_model
		WHERE peer.provider = route.provider
			AND peer.enabled = TRUE
			AND peer.auth_status = 'active'
			AND ` + modelPeerBuildSuperPredicate + `
	))`

const (
	modelProviderPriorityExpression = "CASE model_routes.provider WHEN 'grok_build' THEN 0 WHEN 'grok_web' THEN 1 WHEN 'grok_console' THEN 2 ELSE 3 END"
	modelSupportSortExpression      = `(SELECT COUNT(*) FROM provider_accounts account WHERE account.provider = model_routes.provider AND account.enabled = TRUE AND account.auth_status = 'active' AND (EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id AND binding.account_id = account.id) OR (NOT EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id) AND (EXISTS (SELECT 1 FROM account_model_capabilities capability WHERE capability.account_id = account.id AND capability.upstream_model = model_routes.upstream_model) OR ` + modelSharedPaidBuildSupportSortExpression + `))))`
	modelSyncedSortExpression       = `(SELECT MAX(sync.last_success_at) FROM provider_accounts account JOIN account_model_sync_states sync ON sync.account_id = account.id WHERE account.provider = model_routes.provider AND account.enabled = TRUE AND account.auth_status = 'active')`
)

func NewModelRepository(db *Database) *ModelRepository { return &ModelRepository{db: db} }

func (r *ModelRepository) SetInvalidationObserver(observer repository.InvalidationObserver) {
	r.observer = observer
}

func (r *ModelRepository) notifyInvalidation(ctx context.Context, event repository.InvalidationEvent) {
	if r.observer != nil {
		r.observer(ctx, event)
	}
}

func (r *ModelRepository) List(ctx context.Context, input repository.ModelListQuery) ([]model.Route, int64, error) {
	var total int64
	query := r.db.db.WithContext(ctx).Model(&modelRouteModel{})
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(public_id) LIKE ? OR LOWER(upstream_model) LIKE ?", pattern, pattern)
	}
	if input.Filter.Provider != "" {
		query = query.Where("provider = ?", input.Filter.Provider)
	}
	if input.Filter.Enabled != nil {
		query = query.Where("enabled = ?", *input.Filter.Enabled)
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []modelRouteModel
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"publicId":       {expression: "LOWER(model_routes.public_id)"},
		"upstreamModel":  {expression: "LOWER(model_routes.upstream_model)"},
		"status":         {expression: "CASE WHEN model_routes.enabled = TRUE THEN 0 ELSE 1 END"},
		"provider":       {expression: "model_routes.provider"},
		"accountSupport": {expression: modelSupportSortExpression, defaultDirection: repository.SortDescending},
		"lastSyncedAt":   {expression: modelSyncedSortExpression, nullsLast: true, defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "model_routes.created_at", defaultDirection: repository.SortDescending}, "model_routes.id")
	if err := query.Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, 0, err
	}
	return values, total, nil
}

func (r *ModelRepository) ListEnabled(ctx context.Context) ([]model.Route, error) {
	var rows []modelRouteModel
	if err := r.availableRoutes(r.db.db.WithContext(ctx)).Where("enabled = ?", true).Order("public_id ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, err
	}
	return values, nil
}

// ListConfiguredEnabled 返回所有已启用配置，包括暂时没有可用账号的路由，供 readiness 展示部分故障。
func (r *ModelRepository) ListConfiguredEnabled(ctx context.Context) ([]model.Route, error) {
	var rows []modelRouteModel
	if err := r.db.db.WithContext(ctx).Where("enabled = ?", true).Order("public_id ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, err
	}
	return values, nil
}

func (r *ModelRepository) Get(ctx context.Context, id uint64) (model.Route, error) {
	var row modelRouteModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return model.Route{}, mapError(err)
	}
	values := []model.Route{toModelDomain(row)}
	if err := r.annotateAvailability(ctx, values); err != nil {
		return model.Route{}, err
	}
	return values[0], nil
}

func (r *ModelRepository) GetByPublicID(ctx context.Context, publicID string) (model.Route, error) {
	values, err := r.GetByPublicIDCandidates(ctx, publicID)
	if err != nil {
		return model.Route{}, mapError(err)
	}
	return values[0], nil
}

// GetByPublicIDCandidates 返回同一下游模型名称当前可用的全部来源路由。
// 返回顺序遵循 Build、Web、Console 的稳定 Provider 顺序。
func (r *ModelRepository) GetByPublicIDCandidates(ctx context.Context, publicID string) ([]model.Route, error) {
	db := r.availableRoutes(r.db.db.WithContext(ctx)).Where("enabled = ?", true)
	rows, err := findModelRoutesByPublicID(db, publicID)
	if err != nil {
		return nil, mapError(err)
	}
	return mapModelRows(rows), nil
}

func (r *ModelRepository) GetByPublicIDIncludingDisabled(ctx context.Context, publicID string) (model.Route, error) {
	db := r.db.db.WithContext(ctx)
	rows, err := findModelRoutesByPublicID(db, publicID)
	if err != nil {
		return model.Route{}, mapError(err)
	}
	return toModelDomain(rows[0]), nil
}

func findModelRoutesByPublicID(db *gorm.DB, publicID string) ([]modelRouteModel, error) {
	candidates := model.PublicIDCandidates(publicID)
	alias := strings.TrimSpace(publicID)
	query := db.Session(&gorm.Session{})
	if len(candidates) > 0 {
		query = query.Where(`
			(model_routes.public_id IN ? OR EXISTS (
				SELECT 1 FROM model_route_aliases alias
				WHERE alias.model_route_id = model_routes.id AND alias.alias = ?
			))
		`, candidates, alias).Clauses(clause.OrderBy{Expression: clause.Expr{
			SQL:  "CASE WHEN model_routes.public_id IN ? THEN 0 ELSE 1 END, " + modelProviderPriorityExpression + ", model_routes.id ASC",
			Vars: []any{candidates},
		}})
	} else {
		query = query.Where(`
			EXISTS (
				SELECT 1 FROM model_route_aliases alias
				WHERE alias.model_route_id = model_routes.id AND alias.alias = ?
			)
		`, alias).Order(modelProviderPriorityExpression + ", model_routes.id ASC")
	}
	var rows []modelRouteModel
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return rows, nil
}

func (r *ModelRepository) GetByProviderUpstream(ctx context.Context, provider account.Provider, upstreamModel string) (model.Route, error) {
	var row modelRouteModel
	if err := r.availableRoutes(r.db.db.WithContext(ctx)).Where("provider = ? AND upstream_model = ? AND enabled = ?", provider, upstreamModel, true).First(&row).Error; err != nil {
		return model.Route{}, mapError(err)
	}
	return toModelDomain(row), nil
}

func (r *ModelRepository) ReplaceAccountCapabilities(ctx context.Context, accountID uint64, upstreamModels []string, syncedAt time.Time) error {
	unique := make(map[string]struct{}, len(upstreamModels))
	rows := make([]accountModelCapabilityModel, 0, len(upstreamModels))
	for _, value := range upstreamModels {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := unique[value]; ok {
			continue
		}
		unique[value] = struct{}{}
		rows = append(rows, accountModelCapabilityModel{AccountID: accountID, UpstreamModel: value})
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("account_id = ?", accountID).Delete(&accountModelCapabilityModel{}).Error; err != nil {
			return err
		}
		if len(rows) > 0 {
			if err := tx.CreateInBatches(rows, 200).Error; err != nil {
				return err
			}
		}
		state := accountModelSyncStateModel{AccountID: accountID, LastAttemptAt: syncedAt, LastSuccessAt: &syncedAt}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoUpdates: clause.AssignmentColumns([]string{"last_attempt_at", "last_success_at", "last_error"})}).Create(&state).Error
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCapabilityChanged, AccountID: accountID})
	}
	return err
}

func (r *ModelRepository) MarkAccountCapabilitySyncFailed(ctx context.Context, accountID uint64, attemptedAt time.Time, message string) error {
	state := accountModelSyncStateModel{AccountID: accountID, LastAttemptAt: attemptedAt, LastError: truncate(message, 512)}
	return r.db.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_attempt_at", "last_error"}),
	}).Create(&state).Error
}

func (r *ModelRepository) HasSuccessfulAccountSync(ctx context.Context, accountID uint64) (bool, error) {
	var row struct{ AccountID uint64 }
	err := r.db.db.WithContext(ctx).Model(&accountModelSyncStateModel{}).
		Select("account_id").
		Where("account_id = ? AND last_success_at IS NOT NULL", accountID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return row.AccountID > 0, err
}

// ListStaleAccountSyncIDs 返回模型能力快照缺失或过期的启用账号，不扫描已禁用账号。
func (r *ModelRepository) ListStaleAccountSyncIDs(ctx context.Context, before time.Time, limit int) ([]uint64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Joins("LEFT JOIN account_model_sync_states AS sync ON sync.account_id = account.id").
		Where("account.enabled = ? AND account.auth_status = ?", true, account.AuthStatusActive).
		Where("sync.last_success_at IS NULL OR sync.last_success_at < ?", before.UTC()).
		Order("sync.last_success_at ASC, account.id ASC").
		Limit(limit).
		Scan(&ids).Error
	return ids, err
}

func (r *ModelRepository) UpsertDiscovered(ctx context.Context, provider account.Provider, upstreamModels []string) error {
	changed := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []modelRouteModel
		if err := tx.Where("provider = ?", provider).Find(&existing).Error; err != nil {
			return err
		}
		existingUpstream := make(map[string]bool, len(existing))
		publicIDs := make(map[string]bool, len(existing))
		for _, row := range existing {
			if row.Provider == string(provider) {
				existingUpstream[row.UpstreamModel] = true
			}
			publicIDs[row.PublicID] = true
		}
		rows := make([]modelRouteModel, 0, len(upstreamModels))
		for _, upstreamModel := range upstreamModels {
			if existingUpstream[upstreamModel] {
				continue
			}
			localID, capability := discoveredRouteDefaults(provider, upstreamModel)
			publicID, ok := model.NormalizePublicID(provider, localID)
			if !ok {
				return fmt.Errorf("Provider %s 发现了无效模型 ID %q", provider, localID)
			}
			if publicIDs[publicID] {
				return fmt.Errorf("Provider %s 的模型公开 ID %q 冲突", provider, publicID)
			}
			if err := ensureModelPublicIDNotAlias(tx, publicID, 0); err != nil {
				return err
			}
			publicIDs[publicID] = true
			rows = append(rows, modelRouteModel{PublicID: publicID, Provider: string(provider), UpstreamModel: upstreamModel, Capability: string(capability), Origin: string(model.OriginDiscovered), Enabled: true})
		}
		if len(rows) > 0 {
			// 多实例可能同时发现同一上游模型；唯一约束负责最终幂等，避免竞态变成整批失败。
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 200)
			changed = result.Error == nil && result.RowsAffected > 0
			return result.Error
		}
		return nil
	})
	if err == nil && changed {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
	}
	return err
}

func discoveredRouteDefaults(provider account.Provider, upstreamModel string) (string, model.Capability) {
	switch provider {
	case account.ProviderWeb:
		switch upstreamModel {
		case "grok-imagine-image", "grok-imagine-image-quality":
			return upstreamModel, model.CapabilityImage
		case "imagine-image-edit":
			return "grok-imagine-image-edit", model.CapabilityImageEdit
		case "grok-imagine-video":
			return upstreamModel, model.CapabilityVideo
		default:
			return upstreamModel, model.CapabilityChat
		}
	case account.ProviderBuild:
		if upstreamModel == "grok-imagine-video-1.5" {
			return upstreamModel, model.CapabilityVideo
		}
		return upstreamModel, model.CapabilityResponses
	default:
		return upstreamModel, model.CapabilityResponses
	}
}

func (r *ModelRepository) UpsertRoutes(ctx context.Context, values []model.Route) error {
	providers := make(map[account.Provider]struct{}, len(values))
	for _, value := range values {
		providers[value.Provider] = struct{}{}
	}
	changedProviders := make(map[account.Provider]struct{}, len(providers))
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, original := range values {
			value := original
			publicID, ok := model.NormalizePublicID(value.Provider, value.PublicID)
			if !ok {
				return fmt.Errorf("模型路由目录包含无效公开 ID %q", value.PublicID)
			}
			value.PublicID = publicID
			if strings.TrimSpace(value.PublicID) == "" || strings.TrimSpace(value.UpstreamModel) == "" || value.Provider == "" || value.Capability == "" {
				return fmt.Errorf("模型路由目录包含无效条目")
			}
			var existing modelRouteModel
			err := tx.Where("provider = ? AND upstream_model = ?", value.Provider, value.UpstreamModel).First(&existing).Error
			if err == nil {
				continue
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err := ensureModelPublicIDNotAlias(tx, value.PublicID, 0); err != nil {
				return err
			}
			fallbackOrigin := model.OriginDiscovered
			if value.Provider == account.ProviderWeb {
				fallbackOrigin = model.OriginCatalog
			}
			row := modelRouteModel{PublicID: value.PublicID, Provider: string(value.Provider), UpstreamModel: value.UpstreamModel, Capability: string(value.Capability), Origin: string(normalizeRouteOrigin(value.Origin, fallbackOrigin)), Enabled: value.Enabled}
			if err := tx.Create(&row).Error; err != nil {
				return mapError(err)
			}
			changedProviders[value.Provider] = struct{}{}
		}
		return nil
	})
	if err == nil {
		for provider := range changedProviders {
			r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
		}
	}
	return err
}

func (r *ModelRepository) ReplaceProviderRoutes(ctx context.Context, provider account.Provider, values []model.Route) error {
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		normalizedValues := make([]model.Route, len(values))
		for index, value := range values {
			publicID, ok := model.NormalizePublicID(provider, value.PublicID)
			if !ok {
				return fmt.Errorf("模型路由目录包含无效公开 ID %q", value.PublicID)
			}
			value.PublicID = publicID
			if strings.TrimSpace(value.PublicID) == "" || strings.TrimSpace(value.UpstreamModel) == "" || value.Provider != provider || value.Capability == "" {
				return fmt.Errorf("模型路由目录包含无效条目")
			}
			normalizedValues[index] = value
		}
		values = normalizedValues
		var existing []modelRouteModel
		if err := tx.Where("provider = ?", provider).Find(&existing).Error; err != nil {
			return err
		}

		byUpstream := make(map[string]modelRouteModel, len(existing))
		byPublicID := make(map[string]modelRouteModel, len(existing))
		for _, row := range existing {
			byUpstream[row.UpstreamModel] = row
			byPublicID[row.PublicID] = row
		}
		matched := make(map[int]modelRouteModel, len(values))
		usedIDs := make(map[uint64]bool, len(values))
		for index, value := range values {
			row, ok := byUpstream[value.UpstreamModel]
			if !ok || usedIDs[row.ID] {
				row, ok = byPublicID[value.PublicID]
			}
			if ok && !usedIDs[row.ID] {
				matched[index] = row
				usedIDs[row.ID] = true
			}
		}
		for _, row := range existing {
			if usedIDs[row.ID] || row.Origin == string(model.OriginManual) {
				continue
			}
			if err := tx.Delete(&modelRouteModel{}, row.ID).Error; err != nil {
				return err
			}
		}
		for index, value := range values {
			routeID := uint64(0)
			if row, ok := matched[index]; ok {
				routeID = row.ID
			}
			if err := ensureModelPublicIDNotAlias(tx, value.PublicID, routeID); err != nil {
				return err
			}
		}
		// Both identifiers are unique. Temporary values allow public IDs or upstream
		// identifiers to be swapped while stable route IDs and key permissions survive.
		for index, row := range matched {
			if row.PublicID != values[index].PublicID {
				if err := preserveModelRouteAlias(tx, row.PublicID, row.ID); err != nil {
					return err
				}
			}
		}
		for _, row := range matched {
			updates := map[string]any{
				"public_id":      fmt.Sprintf("__grok2api_reconcile_%d", row.ID),
				"upstream_model": fmt.Sprintf("__grok2api_upstream_reconcile_%d", row.ID),
			}
			if err := tx.Model(&modelRouteModel{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
				return mapError(err)
			}
		}
		for index, value := range values {
			updates := map[string]any{
				"public_id":      value.PublicID,
				"upstream_model": value.UpstreamModel,
				"capability":     value.Capability,
				"origin":         model.OriginCatalog,
			}
			if row, ok := matched[index]; ok {
				if err := tx.Model(&modelRouteModel{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
					return mapError(err)
				}
				if row.UpstreamModel != value.UpstreamModel {
					if err := renameAccountModelCapability(tx, provider, row.UpstreamModel, value.UpstreamModel); err != nil {
						return err
					}
				}
				continue
			}
			row := modelRouteModel{PublicID: value.PublicID, Provider: string(provider), UpstreamModel: value.UpstreamModel, Capability: string(value.Capability), Origin: string(model.OriginCatalog), Enabled: value.Enabled}
			if err := tx.Create(&row).Error; err != nil {
				return mapError(err)
			}
		}
		return nil
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
	}
	return err
}

func renameAccountModelCapability(tx *gorm.DB, provider account.Provider, oldModel, newModel string) error {
	providerAccounts := tx.Model(&accountModel{}).Select("id").Where("provider = ?", provider)
	duplicates := tx.Model(&accountModelCapabilityModel{}).
		Select("account_id").
		Where("upstream_model = ? AND account_id IN (?)", newModel, providerAccounts)
	if err := tx.Where("upstream_model = ? AND account_id IN (?) AND account_id IN (?)", oldModel, providerAccounts, duplicates).
		Delete(&accountModelCapabilityModel{}).Error; err != nil {
		return err
	}
	return tx.Model(&accountModelCapabilityModel{}).
		Where("upstream_model = ? AND account_id IN (?)", oldModel, providerAccounts).
		Update("upstream_model", newModel).Error
}

func (r *ModelRepository) Create(ctx context.Context, value model.Route, accountIDs []uint64) (model.Route, error) {
	publicID, ok := model.NormalizePublicID(value.Provider, value.PublicID)
	if !ok {
		return model.Route{}, fmt.Errorf("模型路由公开 ID 无效")
	}
	value.PublicID = publicID
	row := modelRouteModel{
		PublicID: value.PublicID, Provider: string(value.Provider), UpstreamModel: value.UpstreamModel,
		Capability: string(value.Capability), Origin: string(model.OriginManual), Enabled: value.Enabled,
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureModelPublicIDNotAlias(tx, value.PublicID, 0); err != nil {
			return err
		}
		if err := tx.Create(&row).Error; err != nil {
			return mapError(err)
		}
		if err := replaceModelRouteAccounts(tx, row.ID, accountIDs); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return model.Route{}, err
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: value.Provider, UpstreamModel: value.UpstreamModel})
	if len(accountIDs) > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationModelBindingChanged, Provider: value.Provider, UpstreamModel: value.UpstreamModel})
	}
	return r.Get(ctx, row.ID)
}

func (r *ModelRepository) Update(ctx context.Context, value model.Route, accountIDs *[]uint64) (model.Route, error) {
	var storedProvider account.Provider
	var storedUpstreamModel string
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing modelRouteModel
		if err := tx.Where("id = ?", value.ID).First(&existing).Error; err != nil {
			return mapError(err)
		}
		storedProvider = account.Provider(existing.Provider)
		storedUpstreamModel = existing.UpstreamModel
		publicID, ok := model.NormalizePublicID(storedProvider, value.PublicID)
		if !ok {
			return fmt.Errorf("模型路由公开 ID 无效")
		}
		value.PublicID = publicID
		if err := ensureModelPublicIDNotAlias(tx, value.PublicID, existing.ID); err != nil {
			return err
		}
		if existing.PublicID != value.PublicID {
			if err := preserveModelRouteAlias(tx, existing.PublicID, existing.ID); err != nil {
				return err
			}
		}
		result := tx.Model(&modelRouteModel{}).Where("id = ?", value.ID).Updates(map[string]any{
			"public_id": value.PublicID,
			"enabled":   value.Enabled,
		})
		if result.Error != nil {
			return mapError(result.Error)
		}
		if result.RowsAffected == 0 {
			var count int64
			if err := tx.Model(&modelRouteModel{}).Where("id = ?", value.ID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return repository.ErrNotFound
			}
		}
		if accountIDs != nil {
			return replaceModelRouteAccounts(tx, value.ID, *accountIDs)
		}
		return nil
	})
	if err != nil {
		return model.Route{}, err
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: storedProvider, UpstreamModel: storedUpstreamModel})
	if accountIDs != nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationModelBindingChanged, Provider: storedProvider, UpstreamModel: storedUpstreamModel})
	}
	return r.Get(ctx, value.ID)
}

func (r *ModelRepository) Delete(ctx context.Context, id uint64) error {
	var existing modelRouteModel
	if err := r.db.db.WithContext(ctx).First(&existing, id).Error; err != nil {
		return mapError(err)
	}
	result := r.db.db.WithContext(ctx).Delete(&modelRouteModel{}, id)
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: account.Provider(existing.Provider), UpstreamModel: existing.UpstreamModel})
	return nil
}

func (r *ModelRepository) DeleteMany(ctx context.Context, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var existing []modelRouteModel
	if err := r.db.db.WithContext(ctx).Where("id IN ?", ids).Find(&existing).Error; err != nil {
		return 0, err
	}
	result := r.db.db.WithContext(ctx).Where("id IN ?", ids).Delete(&modelRouteModel{})
	if result.Error == nil && result.RowsAffected > 0 {
		providers := make(map[account.Provider]struct{}, len(existing))
		for _, row := range existing {
			providers[account.Provider(row.Provider)] = struct{}{}
		}
		for provider := range providers {
			r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
		}
	}
	return result.RowsAffected, mapError(result.Error)
}

func replaceModelRouteAccounts(tx *gorm.DB, routeID uint64, accountIDs []uint64) error {
	if err := tx.Where("model_route_id = ?", routeID).Delete(&modelRouteAccountModel{}).Error; err != nil {
		return err
	}
	if len(accountIDs) == 0 {
		return nil
	}
	rows := make([]modelRouteAccountModel, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		rows = append(rows, modelRouteAccountModel{ModelRouteID: routeID, AccountID: accountID})
	}
	return tx.CreateInBatches(rows, 200).Error
}

func (r *ModelRepository) UpdateManyEnabled(ctx context.Context, ids []uint64, enabled bool) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	providers := make(map[account.Provider]struct{})
	var updated int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []modelRouteModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Order("id ASC").Find(&existing).Error; err != nil {
			return err
		}
		for _, row := range existing {
			providers[account.Provider(row.Provider)] = struct{}{}
		}
		result := tx.Model(&modelRouteModel{}).Where("id IN ? AND enabled <> ?", ids, enabled).Update("enabled", enabled)
		updated = result.RowsAffected
		return result.Error
	})
	if err == nil && updated > 0 {
		for provider := range providers {
			r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
		}
	}
	return updated, err
}

func (r *ModelRepository) availableRoutes(query *gorm.DB) *gorm.DB {
	return query.Where(availableRoutePredicate, true, account.AuthStatusActive)
}

func (r *ModelRepository) annotateAvailability(ctx context.Context, values []model.Route) error {
	if len(values) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	type availabilityRow struct {
		RouteID           uint64
		SupportedAccounts int
		SyncedAccounts    int
		TotalAccounts     int
		LastSyncedUnix    sql.NullInt64
	}
	var rows []availabilityRow
	lastSyncedExpression := "MAX(unixepoch(sync.last_success_at))"
	if r.db.dialect == "postgres" {
		lastSyncedExpression = "CAST(MAX(EXTRACT(EPOCH FROM sync.last_success_at)) AS BIGINT)"
	}
	err := r.db.db.WithContext(ctx).Raw(fmt.Sprintf(`
		SELECT route.id AS route_id,
			CASE WHEN COUNT(DISTINCT binding.account_id) > 0
				THEN COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? AND binding.account_id IS NOT NULL THEN account.id END)
				ELSE COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? AND (capability.account_id IS NOT NULL OR `+modelSharedPaidBuildSupportAvailabilityExpression+`) THEN account.id END)
			END AS supported_accounts,
			CASE WHEN COUNT(DISTINCT binding.account_id) > 0
				THEN COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? AND binding.account_id IS NOT NULL AND sync.last_success_at IS NOT NULL THEN account.id END)
				ELSE COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? AND sync.last_success_at IS NOT NULL THEN account.id END)
			END AS synced_accounts,
			CASE WHEN COUNT(DISTINCT binding.account_id) > 0
				THEN COUNT(DISTINCT binding.account_id)
				ELSE COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? THEN account.id END)
			END AS total_accounts,
			%s AS last_synced_unix
		FROM model_routes route
		LEFT JOIN provider_accounts account ON account.provider = route.provider
		LEFT JOIN model_route_accounts binding ON binding.model_route_id = route.id AND binding.account_id = account.id
		LEFT JOIN account_model_sync_states sync ON sync.account_id = account.id
		LEFT JOIN account_model_capabilities capability ON capability.account_id = account.id AND capability.upstream_model = route.upstream_model
		WHERE route.id IN ?
		GROUP BY route.id
	`, lastSyncedExpression), account.AuthStatusActive, account.AuthStatusActive, account.AuthStatusActive, account.AuthStatusActive, account.AuthStatusActive, ids).Scan(&rows).Error
	if err != nil {
		return err
	}
	var bindings []modelRouteAccountModel
	if err := r.db.db.WithContext(ctx).Where("model_route_id IN ?", ids).Order("model_route_id ASC, account_id ASC").Find(&bindings).Error; err != nil {
		return err
	}
	boundByRoute := make(map[uint64][]uint64, len(values))
	for _, binding := range bindings {
		boundByRoute[binding.ModelRouteID] = append(boundByRoute[binding.ModelRouteID], binding.AccountID)
	}
	byID := make(map[uint64]availabilityRow, len(rows))
	for _, row := range rows {
		byID[row.RouteID] = row
	}
	for index := range values {
		row := byID[values[index].ID]
		values[index].SupportedAccounts = row.SupportedAccounts
		values[index].SyncedAccounts = row.SyncedAccounts
		values[index].TotalAccounts = row.TotalAccounts
		values[index].BoundAccountIDs = boundByRoute[values[index].ID]
		if row.LastSyncedUnix.Valid {
			lastSyncedAt := time.Unix(row.LastSyncedUnix.Int64, 0).UTC()
			values[index].LastSyncedAt = &lastSyncedAt
		}
	}
	return nil
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func mapModelRows(rows []modelRouteModel) []model.Route {
	out := make([]model.Route, 0, len(rows))
	for _, row := range rows {
		out = append(out, toModelDomain(row))
	}
	return out
}

func normalizeRouteOrigin(value, fallback model.Origin) model.Origin {
	if value == model.OriginCatalog || value == model.OriginDiscovered || value == model.OriginManual {
		return value
	}
	return fallback
}
