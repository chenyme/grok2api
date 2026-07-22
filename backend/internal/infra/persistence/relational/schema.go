package relational

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"gorm.io/gorm"
)

const mediaJobInputMetadataPendingIndex = "CREATE INDEX IF NOT EXISTS idx_media_jobs_input_metadata_pending ON media_jobs(id) WHERE input_image_count IS NULL"

const postgresSchemaMigrationLockID int64 = 0x47524f4b32415049

var schemaModels = []any{
	&adminModel{},
	&adminSessionModel{},
	&egressSubscriptionSourceModel{},
	&egressNodeModel{},
	&egressOperationsConfigModel{},
	&accountModel{},
	&accountCredentialModel{},
	&accountProviderLinkModel{},
	&webConsoleAccountLinkModel{},
	&webAccountProfileModel{},
	&quotaWindowModel{},
	&billingModel{},
	&quotaRecoveryModel{},
	&modelRouteModel{},
	&modelRouteAliasModel{},
	&modelRouteAccountModel{},
	&accountModelCapabilityModel{},
	&accountModelSyncStateModel{},
	&accountModelQuotaBlockModel{},
	&clientKeyModel{},
	&clientKeyModelPermission{},
	&billingReservationModel{},
	&requestAuditModel{},
	&requestAuditAttemptModel{},
	&responseOwnershipModel{},
	&webResponseStateModel{},
	&mediaJobModel{},
	&mediaAssetModel{},
	&mediaUploadTicketModel{},
	&runtimeSettingsModel{},
}

var schemaIndexes = []string{
	"CREATE INDEX IF NOT EXISTS idx_admin_sessions_admin_created ON admin_sessions(admin_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires ON admin_sessions(expires_at)",
	// SQLite 通过重建表修改 CHECK 约束，重建会删除独立存储的 GORM 唯一索引；
	// 在统一索引阶段显式恢复这些数据完整性约束。
	"CREATE UNIQUE INDEX IF NOT EXISTS idx_provider_accounts_identity_key ON provider_accounts(identity_key)",
	"CREATE INDEX IF NOT EXISTS idx_accounts_routing ON provider_accounts(provider, enabled, auth_status, priority DESC, id ASC)",
	"CREATE INDEX IF NOT EXISTS idx_accounts_created_id ON provider_accounts(created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_accounts_auto_clean_reauth ON provider_accounts(auth_status, reauth_marked_at, id)",
	"CREATE INDEX IF NOT EXISTS idx_accounts_auto_clean_reauth_cursor ON provider_accounts(auth_status, enabled, id, reauth_marked_at)",
	"CREATE INDEX IF NOT EXISTS idx_account_credentials_refresh_due ON account_credentials(refresh_due_at, account_id)",
	"CREATE INDEX IF NOT EXISTS idx_quota_windows_due ON account_quota_windows(remaining, reset_at, account_id)",
	"CREATE UNIQUE INDEX IF NOT EXISTS idx_model_routes_public_id ON model_routes(public_id)",
	"CREATE UNIQUE INDEX IF NOT EXISTS uidx_provider_upstream ON model_routes(provider, upstream_model)",
	"CREATE INDEX IF NOT EXISTS idx_model_routes_created_id ON model_routes(created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_model_routes_enabled ON model_routes(enabled, public_id, id)",
	"CREATE INDEX IF NOT EXISTS idx_model_route_aliases_route ON model_route_aliases(model_route_id, alias)",
	"CREATE INDEX IF NOT EXISTS idx_model_route_accounts_account_route ON model_route_accounts(account_id, model_route_id)",
	"CREATE INDEX IF NOT EXISTS idx_account_model_quota_blocks_due ON account_model_quota_blocks(cooldown_until, account_id)",
	"CREATE INDEX IF NOT EXISTS idx_client_keys_created_id ON client_keys(created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_client_keys_status ON client_keys(enabled, expires_at, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_client_key_models_route_key ON client_key_models(model_route_id, client_key_id)",
	"CREATE INDEX IF NOT EXISTS idx_billing_reservations_expiry ON billing_reservations(expires_at, client_key_id)",
	"CREATE INDEX IF NOT EXISTS idx_egress_nodes_scope_health ON egress_nodes(scope, enabled, health DESC, id ASC)",
	"CREATE INDEX IF NOT EXISTS idx_egress_nodes_probe_due ON egress_nodes(enabled, last_probed_at, id)",
	"CREATE INDEX IF NOT EXISTS idx_audits_created_id ON request_audits(created_at DESC, id DESC)",
	"CREATE UNIQUE INDEX IF NOT EXISTS idx_audits_event_id ON request_audits(event_id) WHERE event_id <> ''",
	"CREATE INDEX IF NOT EXISTS idx_audits_account_created_id ON request_audits(account_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_audits_status_created_id ON request_audits(status_code, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_audits_streaming_created_id ON request_audits(streaming, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_audit_attempts_audit_number ON request_audit_attempts(audit_id, number)",
	"CREATE INDEX IF NOT EXISTS idx_response_ownership_expires_id ON response_ownership(expires_at, response_id)",
	"CREATE INDEX IF NOT EXISTS idx_response_ownership_account ON response_ownership(account_id)",
	"CREATE INDEX IF NOT EXISTS idx_response_ownership_client_key ON response_ownership(client_key_id)",
	"CREATE INDEX IF NOT EXISTS idx_web_response_states_expires_id ON web_response_states(expires_at, response_id)",
	"CREATE INDEX IF NOT EXISTS idx_web_response_states_account ON web_response_states(account_id, created_at DESC)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_client_created ON media_jobs(client_key_id, created_at DESC)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_account_status ON media_jobs(account_id, status)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_recovery ON media_jobs(status, lease_until, created_at, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_usage_recovery ON media_jobs(status, usage_recorded_at, completed_at, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_assets_created ON media_assets(created_at DESC, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_assets_kind_created ON media_assets(kind, created_at DESC, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_upload_tickets_expires ON media_upload_tickets(expires_at, consumed_at)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_result_asset ON media_jobs(result_asset_id) WHERE result_asset_id <> ''",
	// Pending input metadata rows only; keeps startup backfill scans off the full table after migration completes.
	mediaJobInputMetadataPendingIndex,
}

// InitializeSchema 以当前持久化模型作为首版数据库结构基线。
func (d *Database) InitializeSchema(ctx context.Context) error {
	if d.dialect == "postgres" {
		return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", postgresSchemaMigrationLockID).Error; err != nil {
				return fmt.Errorf("acquire PostgreSQL migration lock: %w", err)
			}
			locked := &Database{db: tx, dialect: d.dialect}
			return locked.initializeSchema(ctx)
		})
	}
	return d.initializeSchema(ctx)
}

func (d *Database) initializeSchema(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	// all 作用域会让 Build 与 Web 共用 UA、健康度和冷却状态，升级时直接移除旧节点。
	if db.Migrator().HasTable(&egressNodeModel{}) {
		if err := db.Where("scope = ?", "all").Delete(&egressNodeModel{}).Error; err != nil {
			return fmt.Errorf("清理旧版所有域出口节点: %w", err)
		}
	}
	autoMigrate := func() error {
		return d.db.WithContext(ctx).AutoMigrate(schemaModels...)
	}
	var migrateErr error
	if d.dialect == "sqlite" {
		// SQLite 修改 CHECK 等表级约束时会重建表。provider_accounts 等父表已被多个
		// 子表引用，必须在固定连接上暂停外键，否则 DROP 旧父表会直接失败。
		migrateErr = d.withSQLiteForeignKeysDisabled(ctx, autoMigrate)
	} else {
		migrateErr = autoMigrate()
	}
	if migrateErr != nil {
		return fmt.Errorf("初始化数据库表: %w", migrateErr)
	}
	if err := d.migrateBuildResponseHeaderTimeout(ctx); err != nil {
		return fmt.Errorf("迁移 Grok Build 响应头超时: %w", err)
	}
	if err := d.ensureConsoleConstraints(ctx); err != nil {
		return fmt.Errorf("迁移 Console 数据库约束: %w", err)
	}
	if err := d.ensureAuditOperationConstraints(ctx); err != nil {
		return fmt.Errorf("迁移请求审计操作约束: %w", err)
	}
	if err := d.ensureMediaJobConstraints(ctx); err != nil {
		return fmt.Errorf("迁移 media job 数据库约束: %w", err)
	}
	if err := d.ensureMediaJobInputConstraint(ctx); err != nil {
		return fmt.Errorf("迁移 media job 输入长度约束: %w", err)
	}
	// Create the pending-metadata partial index before backfill so both first
	// upgrade and subsequent empty scans avoid walking the full media_jobs table.
	if err := d.ensureMediaJobInputMetadataPendingIndex(ctx); err != nil {
		return fmt.Errorf("初始化 media job 输入元数据索引: %w", err)
	}
	if err := d.migrateMediaJobInputMetadata(ctx); err != nil {
		return fmt.Errorf("迁移 media job 输入元数据: %w", err)
	}
	if err := d.ensureMediaJobAccountForeignKey(ctx); err != nil {
		return fmt.Errorf("迁移 media job 账号外键: %w", err)
	}
	if err := d.ensureMediaAssetConstraints(ctx); err != nil {
		return fmt.Errorf("迁移 media asset 数据库约束: %w", err)
	}
	if err := d.ensureClientKeyLimitConstraints(ctx); err != nil {
		return fmt.Errorf("迁移客户端 Key 限额约束: %w", err)
	}
	if err := d.backfillWebEgressIdentities(ctx); err != nil {
		return fmt.Errorf("迁移 Web 出口身份: %w", err)
	}
	if err := d.backfillReauthMarkedAt(ctx); err != nil {
		return fmt.Errorf("迁移 reauth_marked_at: %w", err)
	}
	if err := d.dropRedundantResponseExpiryIndexes(ctx); err != nil {
		return fmt.Errorf("迁移响应过期索引: %w", err)
	}
	for _, statement := range schemaIndexes {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("初始化数据库索引: %w", err)
		}
	}
	if err := d.ensureCanonicalModelPublicIDs(ctx); err != nil {
		return fmt.Errorf("迁移模型 Provider 命名空间: %w", err)
	}
	return nil
}

// migrateBuildResponseHeaderTimeout persists the runtime default for settings
// rows created before the Build response-header timeout became configurable.
func (d *Database) migrateBuildResponseHeaderTimeout(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	for range 4 {
		var row runtimeSettingsModel
		if err := db.Where("key = ?", runtimeSettingsKey).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		var payload runtimeSettingsPayload
		if err := json.Unmarshal([]byte(row.ValueJSON), &payload); err != nil {
			return fmt.Errorf("decode runtime settings: %w", err)
		}
		if payload.Config.ProviderBuild.ResponseHeaderTimeout > 0 {
			return nil
		}
		payload.Config.ProviderBuild.ResponseHeaderTimeout = settingsdomain.DefaultBuildResponseHeaderTimeout
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode runtime settings: %w", err)
		}
		result := db.Model(&runtimeSettingsModel{}).
			Where("key = ? AND revision = ?", row.Key, row.Revision).
			UpdateColumn("value_json", string(encoded))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			return nil
		}
	}
	return errors.New("runtime settings changed repeatedly during migration")
}

func (d *Database) dropRedundantResponseExpiryIndexes(ctx context.Context) error {
	for _, name := range []string{"idx_response_ownership_expires", "idx_web_response_states_expires"} {
		if err := d.db.WithContext(ctx).Exec("DROP INDEX IF EXISTS " + name).Error; err != nil {
			return fmt.Errorf("drop index %s: %w", name, err)
		}
	}
	return nil
}

// backfillReauthMarkedAt 为历史 reauthRequired 账号补齐清理锚点；优先使用 updated_at。
func (d *Database) backfillReauthMarkedAt(ctx context.Context) error {
	return d.db.WithContext(ctx).Exec(`
UPDATE provider_accounts
SET reauth_marked_at = updated_at
WHERE auth_status = ? AND reauth_marked_at IS NULL
`, "reauthRequired").Error
}

type consoleConstraint struct {
	model any
	table string
	name  string
}

func (d *Database) ensureConsoleConstraints(ctx context.Context) error {
	return d.ensureNamedConstraints(ctx, []consoleConstraint{
		{model: &accountModel{}, table: "provider_accounts", name: "chk_accounts_provider"},
		{model: &modelRouteModel{}, table: "model_routes", name: "chk_model_routes_provider"},
		{model: &requestAuditModel{}, table: "request_audits", name: "chk_request_audits_provider"},
		{model: &responseOwnershipModel{}, table: "response_ownership", name: "chk_response_ownership_provider"},
		{model: &egressNodeModel{}, table: "egress_nodes", name: "chk_egress_nodes_specific_scope"},
	}, "grok_console")
}

// ensureAuditOperationConstraints upgrades existing databases so Codex remote
// compaction can be recorded separately from ordinary Responses requests.
func (d *Database) ensureAuditOperationConstraints(ctx context.Context) error {
	return d.ensureNamedConstraints(ctx, []consoleConstraint{
		{model: &requestAuditModel{}, table: "request_audits", name: "chk_request_audits_operation"},
	}, "compaction")
}

// ensureMediaJobConstraints 将历史仅允许 grok_web 的 media job CHECK 升级到支持 Build 视频。
// AutoMigrate 不会可靠替换已有 PostgreSQL CHECK，因此启动时幂等检测并重建。
func (d *Database) ensureMediaJobConstraints(ctx context.Context) error {
	return d.ensureNamedConstraints(ctx, []consoleConstraint{
		{model: &mediaJobModel{}, table: "media_jobs", name: "chk_media_jobs_provider"},
		{model: &mediaJobModel{}, table: "media_jobs", name: "chk_media_jobs_egress_scope"},
	}, "grok_build")
}

// ensureMediaJobInputConstraint 允许异步视频任务持久化 Base64 首图。
// SQLite 和 PostgreSQL 都不会由 AutoMigrate 可靠替换已有的同名 CHECK。
func (d *Database) ensureMediaJobInputConstraint(ctx context.Context) error {
	if err := d.ensureNamedConstraints(ctx, []consoleConstraint{
		{model: &mediaJobModel{}, table: "media_jobs", name: "chk_media_jobs_input_json"},
	}, strconv.Itoa(media.MaxInputJSONBytes)); err != nil {
		return err
	}
	return d.ensureNamedConstraints(ctx, []consoleConstraint{
		{model: &mediaJobModel{}, table: "media_jobs", name: "chk_media_jobs_input_image_count"},
	}, strconv.Itoa(media.MaxInputImages))
}

// ensureMediaJobInputMetadataPendingIndex accelerates the one-shot input_image_count
// backfill and keeps later startup probes from full-scanning media_jobs after completion.
func (d *Database) ensureMediaJobInputMetadataPendingIndex(ctx context.Context) error {
	if err := d.db.WithContext(ctx).Exec(mediaJobInputMetadataPendingIndex).Error; err != nil {
		return err
	}
	return nil
}

// migrateMediaJobInputMetadata backfills the compact image count introduced
// after InputJSON began accepting large Base64 references. Already-audited
// terminal jobs can discard the raw payload immediately; active and unaudited
// jobs retain it for worker recovery.
func (d *Database) migrateMediaJobInputMetadata(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	terminal := []string{"completed", "failed"}
	if err := db.Model(&mediaJobModel{}).
		Where("status IN ? AND usage_recorded_at IS NOT NULL AND input_json <> ?", terminal, "{}").
		UpdateColumn("input_json", "{}").Error; err != nil {
		return err
	}
	type inputRow struct {
		ID        string
		InputJSON string
	}
	const batchSize = 8
	cursor := ""
	for {
		var rows []inputRow
		query := db.Model(&mediaJobModel{}).
			Select("id", "input_json").
			Where("id > ? AND input_image_count IS NULL", cursor).
			Order("id ASC").Limit(batchSize)
		if err := query.Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			var input struct {
				ImageURLs []string `json:"image_urls"`
			}
			parseErr := json.Unmarshal([]byte(row.InputJSON), &input)
			count := min(len(input.ImageURLs), media.MaxInputImages)
			updates := map[string]any{"input_image_count": count}
			// Historical code treated malformed or empty input as no references.
			// Normalize it once so future startups do not rescan the same row.
			if parseErr != nil || count == 0 {
				updates["input_json"] = "{}"
			}
			if err := db.Model(&mediaJobModel{}).Where("id = ?", row.ID).UpdateColumns(updates).Error; err != nil {
				return err
			}
		}
		cursor = rows[len(rows)-1].ID
	}
}

// ensureMediaJobAccountForeignKey 让终态视频任务在账号删除后保留快照，
// 同时由应用层阻止删除仍有关联 queued/in_progress 任务的账号。
func (d *Database) ensureMediaJobAccountForeignKey(ctx context.Context) error {
	constraint := consoleConstraint{model: &mediaJobModel{}, table: "media_jobs", name: "fk_media_jobs_account"}
	definition, err := d.constraintDefinition(ctx, constraint)
	if err != nil {
		return err
	}
	if d.dialect == "postgres" {
		db := d.db.WithContext(ctx)
		if err := db.Exec("ALTER TABLE media_jobs ALTER COLUMN account_id DROP NOT NULL").Error; err != nil {
			return err
		}
		if strings.Contains(strings.ToUpper(definition), "ON DELETE SET NULL") {
			return nil
		}
		if err := db.Exec("ALTER TABLE media_jobs DROP CONSTRAINT IF EXISTS fk_media_jobs_account").Error; err != nil {
			return err
		}
		return db.Exec("ALTER TABLE media_jobs ADD CONSTRAINT fk_media_jobs_account FOREIGN KEY (account_id) REFERENCES provider_accounts(id) ON UPDATE CASCADE ON DELETE SET NULL").Error
	}
	if strings.Contains(strings.ToUpper(definition), "ON DELETE SET NULL") {
		return nil
	}
	return d.withSQLiteForeignKeysDisabled(ctx, func() error {
		migrator := d.db.WithContext(ctx).Migrator()
		if err := migrator.AlterColumn(&mediaJobModel{}, "AccountID"); err != nil {
			return err
		}
		if definition != "" {
			if err := migrator.DropConstraint(&mediaJobModel{}, "Account"); err != nil {
				return err
			}
		}
		return migrator.CreateConstraint(&mediaJobModel{}, "Account")
	})
}

// ensureMediaAssetConstraints 升级历史仅允许 image 的媒体资产 CHECK，以支持 video 与更大体积。
func (d *Database) ensureMediaAssetConstraints(ctx context.Context) error {
	if err := d.ensureNamedConstraints(ctx, []consoleConstraint{
		{model: &mediaAssetModel{}, table: "media_assets", name: "chk_media_assets_kind"},
	}, "video"); err != nil {
		return err
	}
	if err := d.ensureNamedConstraints(ctx, []consoleConstraint{
		{model: &mediaAssetModel{}, table: "media_assets", name: "chk_media_assets_mime"},
	}, "video/mp4"); err != nil {
		return err
	}
	return d.ensureNamedConstraints(ctx, []consoleConstraint{
		{model: &mediaAssetModel{}, table: "media_assets", name: "chk_media_assets_size"},
	}, "268435456")
}

// ensureClientKeyLimitConstraints 将历史正数限制升级为允许 0 表示无限制。
// PostgreSQL 不会由 AutoMigrate 可靠替换同名 CHECK，因此需显式检测并重建。
func (d *Database) ensureClientKeyLimitConstraints(ctx context.Context) error {
	constraints := []consoleConstraint{
		{model: &clientKeyModel{}, table: "client_keys", name: "chk_client_keys_rpm"},
		{model: &clientKeyModel{}, table: "client_keys", name: "chk_client_keys_max_concurrent"},
	}
	migrate := func() error {
		db := d.db.WithContext(ctx)
		for _, value := range constraints {
			definition, err := d.constraintDefinition(ctx, value)
			if err != nil {
				return err
			}
			if clientKeyLimitConstraintAllowsZero(definition) {
				continue
			}
			if definition != "" {
				if err := db.Migrator().DropConstraint(value.model, value.name); err != nil {
					return fmt.Errorf("删除旧约束 %s: %w", value.name, err)
				}
			}
			if err := db.Migrator().CreateConstraint(value.model, value.name); err != nil {
				return fmt.Errorf("创建约束 %s: %w", value.name, err)
			}
		}
		return nil
	}
	if d.dialect == "sqlite" {
		return d.withSQLiteForeignKeysDisabled(ctx, migrate)
	}
	return migrate()
}

func clientKeyLimitConstraintAllowsZero(definition string) bool {
	normalized := strings.NewReplacer(" ", "", "\n", "", "\t", "", "\"", "", "`", "", "(", "", ")", "").Replace(strings.ToLower(definition))
	return strings.Contains(normalized, "between0and") || strings.Contains(normalized, ">=0")
}

// ensureNamedConstraints 在约束定义尚未包含 marker 时 drop/recreate；已升级则跳过。
func (d *Database) ensureNamedConstraints(ctx context.Context, constraints []consoleConstraint, marker string) error {
	migrate := func() error {
		db := d.db.WithContext(ctx)
		for _, value := range constraints {
			definition, err := d.constraintDefinition(ctx, value)
			if err != nil {
				return err
			}
			if strings.Contains(definition, marker) {
				continue
			}
			if definition != "" {
				if err := db.Migrator().DropConstraint(value.model, value.name); err != nil {
					return fmt.Errorf("删除旧约束 %s: %w", value.name, err)
				}
			}
			if err := db.Migrator().CreateConstraint(value.model, value.name); err != nil {
				return fmt.Errorf("创建约束 %s: %w", value.name, err)
			}
		}
		return nil
	}
	if d.dialect == "sqlite" {
		return d.withSQLiteForeignKeysDisabled(ctx, migrate)
	}
	return migrate()
}

// withSQLiteForeignKeysDisabled 将会重建父表的约束迁移固定到唯一连接。
// SQLite 的 DROP TABLE 即使只用于改 CHECK，也会执行 ON DELETE CASCADE；因此必须
// 在同一物理连接上临时关闭外键，迁移后再完整校验并恢复。
func (d *Database) withSQLiteForeignKeysDisabled(ctx context.Context, migrate func() error) error {
	sqlDB, err := d.db.DB()
	if err != nil {
		return err
	}
	// OpenSQLite 的正常池大小为 16。收敛为一个连接，确保 PRAGMA 与 GORM 的表重建
	// 使用同一 SQLite 会话；初始化阶段尚未启动业务协程，不会影响请求处理。
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	defer func() {
		sqlDB.SetMaxOpenConns(16)
		sqlDB.SetMaxIdleConns(16)
	}()
	db := d.db.WithContext(ctx)
	if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
		return fmt.Errorf("暂停 SQLite 外键约束: %w", err)
	}
	foreignKeysDisabled := true
	defer func() {
		if foreignKeysDisabled {
			_ = db.Exec("PRAGMA foreign_keys = ON").Error
		}
	}()
	var foreignKeys int
	if err := db.Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error; err != nil {
		return fmt.Errorf("确认 SQLite 外键状态: %w", err)
	}
	if foreignKeys != 0 {
		return fmt.Errorf("暂停 SQLite 外键约束失败")
	}
	migrationErr := migrate()
	if migrationErr == nil {
		var violations []struct {
			Table  string
			RowID  *int64
			Parent string
			FKID   int
		}
		if err := db.Raw("PRAGMA foreign_key_check").Scan(&violations).Error; err != nil {
			migrationErr = fmt.Errorf("校验 SQLite 外键: %w", err)
		} else if len(violations) > 0 {
			migrationErr = fmt.Errorf("SQLite 约束迁移产生 %d 条外键违规", len(violations))
		}
	}
	enableErr := db.Exec("PRAGMA foreign_keys = ON").Error
	if enableErr == nil {
		foreignKeysDisabled = false
	}
	if migrationErr != nil {
		if enableErr != nil {
			return fmt.Errorf("%w；恢复 SQLite 外键失败: %v", migrationErr, enableErr)
		}
		return migrationErr
	}
	if enableErr != nil {
		return fmt.Errorf("恢复 SQLite 外键约束: %w", enableErr)
	}
	return nil
}

func (d *Database) constraintDefinition(ctx context.Context, value consoleConstraint) (string, error) {
	var definition string
	switch d.dialect {
	case "sqlite":
		if err := d.db.WithContext(ctx).Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", value.table).Scan(&definition).Error; err != nil {
			return "", err
		}
		definition = sqliteConstraintDefinition(definition, value.name)
	case "postgres":
		if err := d.db.WithContext(ctx).Raw(`
			SELECT pg_get_constraintdef(constraint_row.oid)
			FROM pg_constraint constraint_row
			JOIN pg_class table_row ON table_row.oid = constraint_row.conrelid
			WHERE table_row.relname = ? AND constraint_row.conname = ?
		`, value.table, value.name).Scan(&definition).Error; err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("不支持的数据库驱动: %s", d.dialect)
	}
	return definition, nil
}

func sqliteConstraintDefinition(tableSQL, name string) string {
	lower := strings.ToLower(tableSQL)
	start := strings.Index(lower, strings.ToLower(name))
	if start < 0 {
		return ""
	}
	definition := tableSQL[start:]
	rest := strings.ToLower(definition[len(name):])
	if next := strings.Index(rest, "constraint "); next >= 0 {
		definition = definition[:len(name)+next]
	}
	return definition
}
