package relational

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type EgressRepository struct{ db *Database }

func NewEgressRepository(db *Database) *EgressRepository { return &EgressRepository{db: db} }

func (r *EgressRepository) ListEgressNodes(ctx context.Context, scope egress.Scope, sort repository.SortQuery) ([]egress.Node, error) {
	query := r.db.db.WithContext(ctx).Model(&egressNodeModel{})
	if scope != "" {
		query = query.Where("scope = ?", scope)
	}
	var rows []egressNodeModel
	query = applyStableSort(query, sort, map[string]sortSpec{
		"name":      {expression: "LOWER(egress_nodes.name)"},
		"scope":     {expression: "egress_nodes.scope"},
		"proxy":     {expression: "CASE WHEN egress_nodes.encrypted_proxy_url <> '' THEN 0 ELSE 1 END"},
		"clearance": {expression: "CASE WHEN egress_nodes.encrypted_cloudflare_cookie <> '' THEN 0 ELSE 1 END"},
		"health":    {expression: "egress_nodes.health", defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "egress_nodes.scope"}, "egress_nodes.id")
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]egress.Node, 0, len(rows))
	counts, err := r.assignedAccountCounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		value := toEgressDomain(row)
		value.AssignedAccountCount = counts[value.ID]
		values = append(values, value)
	}
	return values, nil
}

func (r *EgressRepository) ListEgressNodePage(ctx context.Context, input repository.EgressNodeListQuery) ([]egress.Node, int64, error) {
	query := r.db.db.WithContext(ctx).Model(&egressNodeModel{})
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		query = query.Where("LOWER(egress_nodes.name) LIKE ?", "%"+strings.ToLower(search)+"%")
	}
	if input.Filter.Scope != "" {
		query = query.Where("egress_nodes.scope = ?", input.Filter.Scope)
	}
	if input.Filter.Enabled != nil {
		query = query.Where("egress_nodes.enabled = ?", *input.Filter.Enabled)
	}
	if input.Filter.ProbeStatus != "" {
		query = query.Where("egress_nodes.probe_status = ?", input.Filter.ProbeStatus)
	}
	switch input.Filter.Assignment {
	case "bound":
		query = query.Where("EXISTS (SELECT 1 FROM provider_accounts account WHERE account.egress_node_id = egress_nodes.id)")
	case "unbound":
		query = query.Where("NOT EXISTS (SELECT 1 FROM provider_accounts account WHERE account.egress_node_id = egress_nodes.id)")
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"name":      {expression: "LOWER(egress_nodes.name)"},
		"scope":     {expression: "egress_nodes.scope"},
		"proxy":     {expression: "CASE WHEN egress_nodes.encrypted_proxy_url <> '' THEN 0 ELSE 1 END"},
		"clearance": {expression: "CASE WHEN egress_nodes.encrypted_cloudflare_cookie <> '' THEN 0 ELSE 1 END"},
		"health":    {expression: "egress_nodes.health", defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "egress_nodes.scope"}, "egress_nodes.id")
	var rows []egressNodeModel
	if err := query.Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	if len(rows) == 0 {
		return []egress.Node{}, total, nil
	}

	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	counts, err := r.assignedAccountCountsForNodes(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	values := make([]egress.Node, 0, len(rows))
	for _, row := range rows {
		value := toEgressDomain(row)
		value.AssignedAccountCount = counts[value.ID]
		values = append(values, value)
	}
	return values, total, nil
}

func (r *EgressRepository) GetEgressNode(ctx context.Context, id uint64) (egress.Node, error) {
	var row egressNodeModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return egress.Node{}, mapError(err)
	}
	// AssignedAccountCount is management-list metadata. Keep point lookups lean:
	// this method is also used by the bound-account inference hot path.
	return toEgressDomain(row), nil
}

func (r *EgressRepository) CreateEgressNode(ctx context.Context, value egress.Node) (egress.Node, error) {
	row := fromEgressDomain(value)
	if err := r.db.db.WithContext(ctx).Create(&row).Error; err != nil {
		return egress.Node{}, mapError(err)
	}
	return toEgressDomain(row), nil
}

func (r *EgressRepository) CreateEgressNodes(ctx context.Context, values []egress.Node) (int, error) {
	if len(values) == 0 {
		return 0, nil
	}
	rows := make([]egressNodeModel, 0, len(values))
	for _, value := range values {
		rows = append(rows, fromEgressDomain(value))
	}
	if err := r.db.db.WithContext(ctx).CreateInBatches(&rows, 100).Error; err != nil {
		return 0, mapError(err)
	}
	return len(rows), nil
}

func (r *EgressRepository) UpdateEgressNode(ctx context.Context, value egress.Node) (egress.Node, error) {
	row := fromEgressDomain(value)
	result := r.db.db.WithContext(ctx).Save(&row)
	if result.Error != nil {
		return egress.Node{}, mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return egress.Node{}, repository.ErrNotFound
	}
	return toEgressDomain(row), nil
}

func (r *EgressRepository) UpdateEgressNodesEnabled(ctx context.Context, ids []uint64, enabled bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if enabled {
		result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).
			Where("id IN ? AND enabled <> ?", ids, true).
			Updates(map[string]any{"enabled": true, "updated_at": time.Now().UTC()})
		return int(result.RowsAffected), mapError(result.Error)
	}
	var updated int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		config, err := lockEgressOperationsConfig(tx)
		if err != nil {
			return err
		}
		var lockedIDs []uint64
		if err := tx.Model(&egressNodeModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id IN ?", ids).Order("id ASC").Pluck("id", &lockedIDs).Error; err != nil {
			return err
		}
		if configReferencesAnyFallbackNode(config, lockedIDs) {
			return repository.ErrEgressFallbackInUse
		}
		result := tx.Model(&egressNodeModel{}).
			Where("id IN ? AND enabled <> ?", lockedIDs, false).
			Updates(map[string]any{"enabled": false, "updated_at": time.Now().UTC()})
		updated = result.RowsAffected
		return result.Error
	})
	return int(updated), mapError(err)
}

func (r *EgressRepository) UpdateEgressNodeClearance(ctx context.Context, id uint64, encryptedCookie, userAgent, fingerprint, bindingFingerprint string, refreshedAt time.Time) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"encrypted_cloudflare_cookie": encryptedCookie, "user_agent": userAgent,
		"clearance_fingerprint": fingerprint, "clearance_refreshed_at": refreshedAt,
		"clearance_binding_fingerprint": bindingFingerprint,
		"last_error":                    "", "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *EgressRepository) UpdateEgressNodeHealth(ctx context.Context, id uint64, health float64, failureCount int, cooldownUntil *time.Time, lastError string) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"health": health, "failure_count": failureCount, "cooldown_until": cooldownUntil, "last_error": lastError, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *EgressRepository) UpdateEgressNodeLastError(ctx context.Context, id uint64, lastError string) error {
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Updates(map[string]any{
		"last_error": lastError, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// UpdateEgressNodeProbe persists a direct proxy probe. A healthy result also
// clears a transport-only request failure because the proxy has just been
// verified independently; anti-bot and other request failures stay intact.
func (r *EgressRepository) UpdateEgressNodeProbe(ctx context.Context, id uint64, expectedEncryptedProxyURL string, value egress.ProbeResult) error {
	updates := map[string]any{
		"probe_status": value.Status, "last_probed_at": value.TestedAt.UTC(),
		"probe_latency_ms": value.LatencyMS, "exit_ip": value.ExitIP, "probe_error": value.Error, "probe_provider": storedProbeProvider(value.Provider),
		"ipv4_probe_status": normalizedProbeStatus(value.IPv4.Status), "ipv4_last_probed_at": probeTestedAt(value.IPv4),
		"ipv4_probe_latency_ms": value.IPv4.LatencyMS, "ipv4_exit_ip": value.IPv4.ExitIP, "ipv4_probe_error": value.IPv4.Error,
		"ipv6_probe_status": normalizedProbeStatus(value.IPv6.Status), "ipv6_last_probed_at": probeTestedAt(value.IPv6),
		"ipv6_probe_latency_ms": value.IPv6.LatencyMS, "ipv6_exit_ip": value.IPv6.ExitIP, "ipv6_probe_error": value.IPv6.Error,
		"updated_at": time.Now().UTC(),
	}
	if value.Status == egress.ProbeStatusHealthy {
		condition := "last_error = ?"
		updates["health"] = gorm.Expr("CASE WHEN "+condition+" THEN ? ELSE health END", egress.LastErrorTransport, 1)
		updates["failure_count"] = gorm.Expr("CASE WHEN "+condition+" THEN ? ELSE failure_count END", egress.LastErrorTransport, 0)
		updates["cooldown_until"] = gorm.Expr("CASE WHEN "+condition+" THEN NULL ELSE cooldown_until END", egress.LastErrorTransport)
		updates["last_error"] = gorm.Expr("CASE WHEN "+condition+" THEN ? ELSE last_error END", egress.LastErrorTransport, "")
	}
	result := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).
		Where("id = ? AND encrypted_proxy_url = ?", id, expectedEncryptedProxyURL).
		Updates(updates)
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return mapError(err)
		}
		if count == 0 {
			return repository.ErrNotFound
		}
		return repository.ErrConflict
	}
	return nil
}

func (r *EgressRepository) ListDueEgressNodes(ctx context.Context, now time.Time, interval time.Duration, limit int) ([]egress.Node, error) {
	if limit < 1 {
		return []egress.Node{}, nil
	}
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	var rows []egressNodeModel
	if err := r.db.db.WithContext(ctx).
		Where("enabled = ? AND encrypted_proxy_url <> '' AND (last_probed_at IS NULL OR last_probed_at <= ?)", true, now.UTC().Add(-interval)).
		Order("last_probed_at ASC NULLS FIRST, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	values := make([]egress.Node, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressDomain(row))
	}
	return values, nil
}

func (r *EgressRepository) ListEgressSources(ctx context.Context) ([]egress.SubscriptionSource, error) {
	var rows []egressSubscriptionSourceModel
	if err := r.db.db.WithContext(ctx).Order("name ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	values := make([]egress.SubscriptionSource, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressSubscriptionSourceDomain(row))
	}
	return values, nil
}

func (r *EgressRepository) ListEgressSourcePage(ctx context.Context, input repository.EgressSourceListQuery) ([]egress.SubscriptionSource, int64, error) {
	query := r.db.db.WithContext(ctx).Model(&egressSubscriptionSourceModel{})
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		query = query.Where("LOWER(egress_subscription_sources.name) LIKE ?", "%"+strings.ToLower(search)+"%")
	}
	if input.Filter.Scope != "" {
		query = query.Where("egress_subscription_sources.scope = ?", input.Filter.Scope)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, mapError(err)
	}
	var rows []egressSubscriptionSourceModel
	if err := query.Order("LOWER(egress_subscription_sources.name) ASC, egress_subscription_sources.id ASC").
		Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, mapError(err)
	}
	values := make([]egress.SubscriptionSource, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressSubscriptionSourceDomain(row))
	}
	return values, total, nil
}

func (r *EgressRepository) ListDueEgressSources(ctx context.Context, now time.Time, limit int) ([]egress.SubscriptionSource, error) {
	if limit < 1 {
		return []egress.SubscriptionSource{}, nil
	}
	var rows []egressSubscriptionSourceModel
	if err := r.db.db.WithContext(ctx).
		Where("enabled = ? AND encrypted_url <> '' AND (next_sync_at IS NULL OR next_sync_at <= ?)", true, now.UTC()).
		Order("next_sync_at ASC NULLS FIRST, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	values := make([]egress.SubscriptionSource, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressSubscriptionSourceDomain(row))
	}
	return values, nil
}

func (r *EgressRepository) GetEgressSource(ctx context.Context, id uint64) (egress.SubscriptionSource, error) {
	var row egressSubscriptionSourceModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return egress.SubscriptionSource{}, mapError(err)
	}
	return toEgressSubscriptionSourceDomain(row), nil
}

func (r *EgressRepository) CreateEgressSource(ctx context.Context, value egress.SubscriptionSource) (egress.SubscriptionSource, error) {
	row := fromEgressSubscriptionSourceDomain(value)
	if err := r.db.db.WithContext(ctx).Create(&row).Error; err != nil {
		return egress.SubscriptionSource{}, mapError(err)
	}
	return toEgressSubscriptionSourceDomain(row), nil
}

func (r *EgressRepository) UpdateEgressSource(ctx context.Context, value egress.SubscriptionSource) (egress.SubscriptionSource, error) {
	row := fromEgressSubscriptionSourceDomain(value)
	result := r.db.db.WithContext(ctx).Save(&row)
	if result.Error != nil {
		return egress.SubscriptionSource{}, mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return egress.SubscriptionSource{}, repository.ErrNotFound
	}
	return toEgressSubscriptionSourceDomain(row), nil
}

// DeleteEgressSource keeps already imported nodes intact. They become normal
// manually managed nodes rather than silently losing proxy configuration.
func (r *EgressRepository) DeleteEgressSource(ctx context.Context, id uint64) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&egressNodeModel{}).Where("source_id = ?", id).Updates(map[string]any{"source_id": nil, "source_key": ""}).Error; err != nil {
			return mapError(err)
		}
		result := tx.Delete(&egressSubscriptionSourceModel{}, id)
		if result.Error != nil {
			return mapError(result.Error)
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	})
}

func (r *EgressRepository) UpdateEgressSourceSync(ctx context.Context, id uint64, syncedAt, nextSyncAt time.Time, imported int, lastError string) error {
	result := r.db.db.WithContext(ctx).Model(&egressSubscriptionSourceModel{}).Where("id = ?", id).Updates(map[string]any{
		"last_synced_at": syncedAt.UTC(), "next_sync_at": nextSyncAt.UTC(), "last_sync_imported": imported,
		"last_sync_error": lastError, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// UpsertEgressNodesFromSource replaces the active representation of a source
// atomically. Stale nodes are disabled instead of deleted so auto-assigned
// accounts can be moved by the next balancing cycle without touching manual
// bindings.
func (r *EgressRepository) UpsertEgressNodesFromSource(ctx context.Context, sourceID uint64, values []egress.Node) (int, error) {
	if sourceID == 0 {
		return 0, errors.New("subscription source id is required")
	}
	returned := 0
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existingKeys []string
		if err := tx.Model(&egressNodeModel{}).Where("source_id = ?", sourceID).Pluck("source_key", &existingKeys).Error; err != nil {
			return mapError(err)
		}
		existing := make(map[string]struct{}, len(existingKeys))
		for _, key := range existingKeys {
			existing[key] = struct{}{}
		}
		keys := make([]string, 0, len(values))
		for _, value := range values {
			if value.SourceID != sourceID || value.SourceKey == "" {
				return errors.New("invalid subscription node")
			}
			row := fromEgressDomain(value)
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "source_id"}, {Name: "source_key"}},
				DoUpdates: clause.Assignments(map[string]any{
					"name": row.Name, "scope": row.Scope, "enabled": row.Enabled, "proxy_pool": row.ProxyPool,
					"account_capacity": row.AccountCapacity, "encrypted_proxy_url": row.EncryptedProxyURL,
					"updated_at": time.Now().UTC(),
				}),
			}).Create(&row).Error; err != nil {
				return mapError(err)
			}
			keys = append(keys, value.SourceKey)
			if _, found := existing[value.SourceKey]; !found {
				returned++
				existing[value.SourceKey] = struct{}{}
			}
		}
		stale := tx.Model(&egressNodeModel{}).Where("source_id = ?", sourceID)
		if len(keys) > 0 {
			stale = stale.Where("source_key NOT IN ?", keys)
		}
		if err := stale.Updates(map[string]any{
			"enabled": false, "probe_status": string(egress.ProbeStatusUnknown), "probe_error": "subscription entry removed", "probe_provider": "",
			"ipv4_probe_status": string(egress.ProbeStatusUnknown), "ipv4_last_probed_at": nil, "ipv4_probe_latency_ms": 0, "ipv4_exit_ip": "", "ipv4_probe_error": "subscription entry removed",
			"ipv6_probe_status": string(egress.ProbeStatusUnknown), "ipv6_last_probed_at": nil, "ipv6_probe_latency_ms": 0, "ipv6_exit_ip": "", "ipv6_probe_error": "subscription entry removed",
			"updated_at": time.Now().UTC(),
		}).Error; err != nil {
			return mapError(err)
		}
		if err := clearInvalidEgressFallbackNodeReferences(tx); err != nil {
			return err
		}
		return nil
	})
	return returned, err
}

func (r *EgressRepository) GetEgressOperationsConfig(ctx context.Context) (egress.OperationsConfig, error) {
	var row egressOperationsConfigModel
	if err := r.db.db.WithContext(ctx).First(&row, 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return egress.DefaultOperationsConfig(), nil
		}
		return egress.OperationsConfig{}, mapError(err)
	}
	return toEgressOperationsConfigDomain(row), nil
}

func (r *EgressRepository) SaveEgressOperationsConfig(ctx context.Context, value egress.OperationsConfig) (egress.OperationsConfig, error) {
	row := fromEgressOperationsConfigDomain(value)
	row.ID = 1
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := lockEgressOperationsConfig(tx)
		if err != nil {
			return err
		}
		// This schema-only marker is deliberately outside the domain settings.
		// Preserve it across ordinary configuration updates so the legacy proxy
		// migration cannot run again and overwrite an intentionally direct source.
		row.SubscriptionProxyMigrationCompleted = locked.SubscriptionProxyMigrationCompleted
		if err := validateLockedEgressFallbackNodes(tx, row); err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
	})
	if err != nil {
		return egress.OperationsConfig{}, mapError(err)
	}
	return toEgressOperationsConfigDomain(row), nil
}

func lockEgressOperationsConfig(tx *gorm.DB) (egressOperationsConfigModel, error) {
	defaults := fromEgressOperationsConfigDomain(egress.DefaultOperationsConfig())
	defaults.ID = 1
	defaults.UpdatedAt = time.Now().UTC()
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&defaults).Error; err != nil {
		return egressOperationsConfigModel{}, err
	}
	var row egressOperationsConfigModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, 1).Error; err != nil {
		return egressOperationsConfigModel{}, err
	}
	return row, nil
}

func configReferencesAnyFallbackNode(config egressOperationsConfigModel, ids []uint64) bool {
	selected := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		selected[id] = struct{}{}
	}
	for _, fallback := range []struct {
		mode   string
		nodeID uint64
	}{
		{config.BuildFallbackMode, config.BuildFallbackNodeID},
		{config.WebFallbackMode, config.WebFallbackNodeID},
		{config.ConsoleFallbackMode, config.ConsoleFallbackNodeID},
		{config.WebAssetFallbackMode, config.WebAssetFallbackNodeID},
		{config.ConsoleAssetFallbackMode, config.ConsoleAssetFallbackNodeID},
	} {
		if egress.FallbackMode(fallback.mode).Normalized() != egress.FallbackModeFixed {
			continue
		}
		if _, exists := selected[fallback.nodeID]; exists {
			return true
		}
	}
	return false
}

func validateLockedEgressFallbackNodes(tx *gorm.DB, config egressOperationsConfigModel) error {
	fallbacks := []struct {
		scope  egress.Scope
		mode   string
		nodeID uint64
	}{
		{egress.ScopeBuild, config.BuildFallbackMode, config.BuildFallbackNodeID},
		{egress.ScopeWeb, config.WebFallbackMode, config.WebFallbackNodeID},
		{egress.ScopeConsole, config.ConsoleFallbackMode, config.ConsoleFallbackNodeID},
		{egress.ScopeWebAsset, config.WebAssetFallbackMode, config.WebAssetFallbackNodeID},
		{egress.ScopeConsoleAsset, config.ConsoleAssetFallbackMode, config.ConsoleAssetFallbackNodeID},
	}
	ids := make([]uint64, 0, len(fallbacks))
	for _, fallback := range fallbacks {
		if egress.FallbackMode(fallback.mode).Normalized() == egress.FallbackModeFixed && fallback.nodeID != 0 {
			ids = append(ids, fallback.nodeID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var rows []egressNodeModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Order("id ASC").Find(&rows).Error; err != nil {
		return err
	}
	byID := make(map[uint64]egressNodeModel, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	for _, fallback := range fallbacks {
		if egress.FallbackMode(fallback.mode).Normalized() != egress.FallbackModeFixed {
			continue
		}
		node, exists := byID[fallback.nodeID]
		if !exists || !node.Enabled || node.ProxyPool || node.EncryptedProxyURL == "" || !egress.SupportsScope(egress.Scope(node.Scope), fallback.scope) {
			return repository.ErrEgressFallbackInUse
		}
	}
	return nil
}

func (r *EgressRepository) DeleteEgressNode(ctx context.Context, id uint64) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&accountModel{}).Where("egress_node_id = ?", id).Updates(map[string]any{
			"egress_node_id": nil, "egress_assignment_mode": "", "egress_assigned_at": nil,
		})
		if result.Error != nil {
			return result.Error
		}
		if err := clearEgressFallbackNodeReferences(tx, []uint64{id}); err != nil {
			return err
		}
		result = tx.Delete(&egressNodeModel{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	})
}

// DeleteEgressNodes deletes a selection atomically. Account bindings are
// removed first so no account can retain a reference to a deleted node.
func (r *EgressRepository) DeleteEgressNodes(ctx context.Context, ids []uint64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		deleted, err = deleteEgressNodeIDs(tx, ids)
		return err
	})
	return int(deleted), mapError(err)
}

func (r *EgressRepository) PreviewUnhealthyEgressNodes(ctx context.Context) (repository.EgressNodeCleanupPreview, error) {
	var result repository.EgressNodeCleanupPreview
	if err := r.unhealthyEgressNodes(ctx).Count(&result.Nodes).Error; err != nil {
		return result, mapError(err)
	}
	if err := r.unhealthyEgressNodes(ctx).Where("source_id IS NOT NULL").Count(&result.SubscriptionManaged).Error; err != nil {
		return result, mapError(err)
	}
	subquery := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Select("id").
		Where("ipv4_probe_status = ? AND ipv6_probe_status = ?", egress.ProbeStatusUnhealthy, egress.ProbeStatusUnhealthy)
	if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("egress_node_id IN (?)", subquery).Count(&result.BoundAccounts).Error; err != nil {
		return result, mapError(err)
	}
	return result, nil
}

func (r *EgressRepository) unhealthyEgressNodes(ctx context.Context) *gorm.DB {
	return r.db.db.WithContext(ctx).Model(&egressNodeModel{}).
		Where("ipv4_probe_status = ? AND ipv6_probe_status = ?", egress.ProbeStatusUnhealthy, egress.ProbeStatusUnhealthy)
}

func (r *EgressRepository) DeleteUnhealthyEgressNodes(ctx context.Context) ([]uint64, error) {
	ids := make([]uint64, 0)
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&egressNodeModel{}).
			Where("ipv4_probe_status = ? AND ipv6_probe_status = ?", egress.ProbeStatusUnhealthy, egress.ProbeStatusUnhealthy).
			Order("id ASC")
		if tx.Dialector.Name() == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.Pluck("id", &ids).Error; err != nil {
			return err
		}
		_, err := deleteEgressNodeIDs(tx, ids)
		return err
	})
	return ids, mapError(err)
}

func deleteEgressNodeIDs(tx *gorm.DB, ids []uint64) (int64, error) {
	var deleted int64
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		batch := ids[start:end]
		if result := tx.Model(&accountModel{}).Where("egress_node_id IN ?", batch).Updates(map[string]any{
			"egress_node_id": nil, "egress_assignment_mode": "", "egress_assigned_at": nil,
		}); result.Error != nil {
			return deleted, result.Error
		}
		if err := clearEgressFallbackNodeReferences(tx, batch); err != nil {
			return deleted, err
		}
		result := tx.Where("id IN ?", batch).Delete(&egressNodeModel{})
		if result.Error != nil {
			return deleted, result.Error
		}
		deleted += result.RowsAffected
	}
	return deleted, nil
}

func clearEgressFallbackNodeReferences(tx *gorm.DB, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	for _, columns := range [][2]string{
		{"build_fallback_mode", "build_fallback_node_id"},
		{"web_fallback_mode", "web_fallback_node_id"},
		{"console_fallback_mode", "console_fallback_node_id"},
		{"web_asset_fallback_mode", "web_asset_fallback_node_id"},
		{"console_asset_fallback_mode", "console_asset_fallback_node_id"},
	} {
		if err := tx.Model(&egressOperationsConfigModel{}).
			Where("id = ? AND "+columns[1]+" IN ?", 1, ids).
			Updates(map[string]any{columns[0]: string(egress.FallbackModeNone), columns[1]: 0}).Error; err != nil {
			return err
		}
	}
	return nil
}

func clearInvalidEgressFallbackNodeReferences(tx *gorm.DB) error {
	var config egressOperationsConfigModel
	if err := tx.First(&config, 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	for _, fallback := range []struct {
		scope                egress.Scope
		mode                 string
		nodeID               uint64
		modeColumn, idColumn string
	}{
		{egress.ScopeBuild, config.BuildFallbackMode, config.BuildFallbackNodeID, "build_fallback_mode", "build_fallback_node_id"},
		{egress.ScopeWeb, config.WebFallbackMode, config.WebFallbackNodeID, "web_fallback_mode", "web_fallback_node_id"},
		{egress.ScopeConsole, config.ConsoleFallbackMode, config.ConsoleFallbackNodeID, "console_fallback_mode", "console_fallback_node_id"},
		{egress.ScopeWebAsset, config.WebAssetFallbackMode, config.WebAssetFallbackNodeID, "web_asset_fallback_mode", "web_asset_fallback_node_id"},
		{egress.ScopeConsoleAsset, config.ConsoleAssetFallbackMode, config.ConsoleAssetFallbackNodeID, "console_asset_fallback_mode", "console_asset_fallback_node_id"},
	} {
		if egress.FallbackMode(fallback.mode).Normalized() != egress.FallbackModeFixed {
			continue
		}
		var node egressNodeModel
		err := tx.First(&node, fallback.nodeID).Error
		valid := err == nil && node.Enabled && !node.ProxyPool && node.EncryptedProxyURL != "" && egress.SupportsScope(egress.Scope(node.Scope), fallback.scope)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if valid {
			continue
		}
		if err := tx.Model(&egressOperationsConfigModel{}).Where("id = ?", 1).
			Updates(map[string]any{fallback.modeColumn: string(egress.FallbackModeNone), fallback.idColumn: 0}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (r *EgressRepository) assignedAccountCounts(ctx context.Context) (map[uint64]int, error) {
	return r.assignedAccountCountsForNodes(ctx, nil)
}

func (r *EgressRepository) assignedAccountCountsForNodes(ctx context.Context, nodeIDs []uint64) (map[uint64]int, error) {
	type row struct {
		NodeID uint64
		Count  int
	}
	var rows []row
	query := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Select("egress_node_id AS node_id, COUNT(*) AS count").
		Where("egress_node_id IS NOT NULL")
	if nodeIDs != nil {
		if len(nodeIDs) == 0 {
			return map[uint64]int{}, nil
		}
		query = query.Where("egress_node_id IN ?", nodeIDs)
	}
	if err := query.Group("egress_node_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	result := make(map[uint64]int, len(rows))
	for _, row := range rows {
		result[row.NodeID] = row.Count
	}
	return result, nil
}

func toEgressDomain(row egressNodeModel) egress.Node {
	return egress.Node{
		ID: row.ID, Name: row.Name, Scope: egress.Scope(row.Scope), Enabled: row.Enabled, ProxyPool: row.ProxyPool,
		SourceID: valueEgressNodeID(row.SourceID), SourceKey: row.SourceKey, AccountCapacity: row.AccountCapacity,
		EncryptedProxyURL: row.EncryptedProxyURL, UserAgent: row.UserAgent, EncryptedCloudflareCookie: row.EncryptedCloudflareCookie,
		ClearanceRefreshedAt: row.ClearanceRefreshedAt, ClearanceFingerprint: row.ClearanceFingerprint,
		ClearanceBindingFingerprint: row.ClearanceBindingFingerprint,
		Health:                      row.Health, FailureCount: row.FailureCount, CooldownUntil: row.CooldownUntil, LastError: row.LastError,
		ProbeStatus: egress.ProbeStatus(row.ProbeStatus), LastProbedAt: row.LastProbedAt, ProbeLatencyMS: row.ProbeLatencyMS, ExitIP: row.ExitIP, ProbeError: row.ProbeError,
		ProbeProvider: storedProbeProvider(egress.ProbeProvider(row.ProbeProvider)),
		IPv4Probe:     probeFamilyFromRow(row.IPv4ProbeStatus, row.IPv4LastProbedAt, row.IPv4ProbeLatencyMS, row.IPv4ExitIP, row.IPv4ProbeError),
		IPv6Probe:     probeFamilyFromRow(row.IPv6ProbeStatus, row.IPv6LastProbedAt, row.IPv6ProbeLatencyMS, row.IPv6ExitIP, row.IPv6ProbeError),
		CreatedAt:     row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func fromEgressDomain(value egress.Node) egressNodeModel {
	health := value.Health
	if health == 0 && value.ID == 0 {
		health = 1
	}
	probeStatus := value.ProbeStatus
	if !probeStatus.IsValid() {
		probeStatus = egress.ProbeStatusUnknown
	}
	return egressNodeModel{
		ID: value.ID, Name: value.Name, Scope: string(value.Scope), Enabled: value.Enabled, ProxyPool: value.ProxyPool,
		SourceID: egressNodeID(value.SourceID), SourceKey: value.SourceKey, AccountCapacity: value.AccountCapacity,
		EncryptedProxyURL: value.EncryptedProxyURL, UserAgent: value.UserAgent, EncryptedCloudflareCookie: value.EncryptedCloudflareCookie,
		ClearanceRefreshedAt: value.ClearanceRefreshedAt, ClearanceFingerprint: value.ClearanceFingerprint,
		ClearanceBindingFingerprint: value.ClearanceBindingFingerprint,
		Health:                      health, FailureCount: value.FailureCount, CooldownUntil: value.CooldownUntil, LastError: value.LastError,
		ProbeStatus: string(probeStatus), LastProbedAt: value.LastProbedAt, ProbeLatencyMS: value.ProbeLatencyMS, ExitIP: value.ExitIP, ProbeError: value.ProbeError,
		ProbeProvider:   string(storedProbeProvider(value.ProbeProvider)),
		IPv4ProbeStatus: string(normalizedProbeStatus(value.IPv4Probe.Status)), IPv4LastProbedAt: probeTestedAt(value.IPv4Probe), IPv4ProbeLatencyMS: value.IPv4Probe.LatencyMS, IPv4ExitIP: value.IPv4Probe.ExitIP, IPv4ProbeError: value.IPv4Probe.Error,
		IPv6ProbeStatus: string(normalizedProbeStatus(value.IPv6Probe.Status)), IPv6LastProbedAt: probeTestedAt(value.IPv6Probe), IPv6ProbeLatencyMS: value.IPv6Probe.LatencyMS, IPv6ExitIP: value.IPv6Probe.ExitIP, IPv6ProbeError: value.IPv6Probe.Error,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func normalizedProbeStatus(value egress.ProbeStatus) egress.ProbeStatus {
	if value.IsValid() {
		return value
	}
	return egress.ProbeStatusUnknown
}

func storedProbeProvider(value egress.ProbeProvider) egress.ProbeProvider {
	if value.IsValid() {
		return value
	}
	return ""
}

func probeTestedAt(value egress.ProbeFamilyResult) *time.Time {
	if value.TestedAt.IsZero() {
		return nil
	}
	testedAt := value.TestedAt.UTC()
	return &testedAt
}

func probeFamilyFromRow(status string, testedAt *time.Time, latencyMS int, exitIP, probeError string) egress.ProbeFamilyResult {
	result := egress.ProbeFamilyResult{
		Status: normalizedProbeStatus(egress.ProbeStatus(status)), LatencyMS: latencyMS, ExitIP: exitIP, Error: probeError,
	}
	if testedAt != nil {
		result.TestedAt = testedAt.UTC()
	}
	return result
}

func toEgressSubscriptionSourceDomain(row egressSubscriptionSourceModel) egress.SubscriptionSource {
	return egress.SubscriptionSource{
		ID: row.ID, Name: row.Name, Scope: egress.Scope(row.Scope), Enabled: row.Enabled, EncryptedURL: row.EncryptedURL, EncryptedProxyURL: row.EncryptedProxyURL,
		RefreshIntervalSeconds: row.RefreshIntervalSeconds, DefaultAccountCapacity: row.DefaultAccountCapacity,
		LastSyncedAt: row.LastSyncedAt, NextSyncAt: row.NextSyncAt, LastSyncImported: row.LastSyncImported, LastSyncError: row.LastSyncError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func fromEgressSubscriptionSourceDomain(value egress.SubscriptionSource) egressSubscriptionSourceModel {
	return egressSubscriptionSourceModel{
		ID: value.ID, Name: value.Name, Scope: string(value.Scope), Enabled: value.Enabled, EncryptedURL: value.EncryptedURL, EncryptedProxyURL: value.EncryptedProxyURL,
		RefreshIntervalSeconds: value.RefreshIntervalSeconds, DefaultAccountCapacity: value.DefaultAccountCapacity,
		LastSyncedAt: value.LastSyncedAt, NextSyncAt: value.NextSyncAt, LastSyncImported: value.LastSyncImported, LastSyncError: value.LastSyncError,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func toEgressOperationsConfigDomain(row egressOperationsConfigModel) egress.OperationsConfig {
	return egress.OperationsConfig{
		ProbeProvider:        egress.ProbeProvider(row.ProbeProvider).Normalized(),
		ProbeIntervalSeconds: row.ProbeIntervalSeconds, AutoAssignEnabled: row.AutoAssignEnabled, AutoBalanceEnabled: row.AutoBalanceEnabled,
		AssignmentIntervalSeconds: row.AssignmentIntervalSeconds,
		Fallbacks: map[egress.Scope]egress.FallbackConfig{
			egress.ScopeBuild:        {Mode: egress.FallbackMode(row.BuildFallbackMode).Normalized(), NodeID: row.BuildFallbackNodeID},
			egress.ScopeWeb:          {Mode: egress.FallbackMode(row.WebFallbackMode).Normalized(), NodeID: row.WebFallbackNodeID},
			egress.ScopeConsole:      {Mode: egress.FallbackMode(row.ConsoleFallbackMode).Normalized(), NodeID: row.ConsoleFallbackNodeID},
			egress.ScopeWebAsset:     {Mode: egress.FallbackMode(row.WebAssetFallbackMode).Normalized(), NodeID: row.WebAssetFallbackNodeID},
			egress.ScopeConsoleAsset: {Mode: egress.FallbackMode(row.ConsoleAssetFallbackMode).Normalized(), NodeID: row.ConsoleAssetFallbackNodeID},
		},
		UpdatedAt: row.UpdatedAt,
	}
}

func fromEgressOperationsConfigDomain(value egress.OperationsConfig) egressOperationsConfigModel {
	buildFallback := value.FallbackFor(egress.ScopeBuild)
	webFallback := value.FallbackFor(egress.ScopeWeb)
	consoleFallback := value.FallbackFor(egress.ScopeConsole)
	webAssetFallback := value.FallbackFor(egress.ScopeWebAsset)
	consoleAssetFallback := value.FallbackFor(egress.ScopeConsoleAsset)
	return egressOperationsConfigModel{
		ID: 1, ProbeProvider: string(value.ProbeProvider.Normalized()), ProbeIntervalSeconds: value.ProbeIntervalSeconds, AutoAssignEnabled: value.AutoAssignEnabled,
		AutoBalanceEnabled: value.AutoBalanceEnabled, AssignmentIntervalSeconds: value.AssignmentIntervalSeconds,
		BuildFallbackMode: string(buildFallback.Mode), BuildFallbackNodeID: buildFallback.NodeID,
		WebFallbackMode: string(webFallback.Mode), WebFallbackNodeID: webFallback.NodeID,
		ConsoleFallbackMode: string(consoleFallback.Mode), ConsoleFallbackNodeID: consoleFallback.NodeID,
		WebAssetFallbackMode: string(webAssetFallback.Mode), WebAssetFallbackNodeID: webAssetFallback.NodeID,
		ConsoleAssetFallbackMode: string(consoleAssetFallback.Mode), ConsoleAssetFallbackNodeID: consoleAssetFallback.NodeID,
		UpdatedAt: value.UpdatedAt,
	}
}
