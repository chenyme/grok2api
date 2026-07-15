package relational

import (
	"context"
	"fmt"
	"strings"
)

var schemaModels = []any{
	&adminModel{},
	&adminSessionModel{},
	&accountModel{},
	&accountCredentialModel{},
	&accountProviderLinkModel{},
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
	&runtimeSettingsModel{},
	&egressNodeModel{},
}

var schemaIndexes = []string{
	"CREATE INDEX IF NOT EXISTS idx_admin_sessions_admin_created ON admin_sessions(admin_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires ON admin_sessions(expires_at)",
	// SQLite 通过重建表修改 CHECK 约束，重建会删除独立存储的 GORM 唯一索引；
	// 在统一索引阶段显式恢复这些数据完整性约束。
	"CREATE UNIQUE INDEX IF NOT EXISTS idx_provider_accounts_identity_key ON provider_accounts(identity_key)",
	"CREATE INDEX IF NOT EXISTS idx_accounts_routing ON provider_accounts(provider, enabled, auth_status, priority DESC, id ASC)",
	"CREATE INDEX IF NOT EXISTS idx_accounts_created_id ON provider_accounts(created_at DESC, id DESC)",
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
	"CREATE INDEX IF NOT EXISTS idx_audits_created_id ON request_audits(created_at DESC, id DESC)",
	"CREATE UNIQUE INDEX IF NOT EXISTS idx_audits_event_id ON request_audits(event_id) WHERE event_id <> ''",
	"CREATE INDEX IF NOT EXISTS idx_audits_account_created_id ON request_audits(account_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_audits_status_created_id ON request_audits(status_code, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_audits_streaming_created_id ON request_audits(streaming, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_audit_attempts_audit_number ON request_audit_attempts(audit_id, number)",
	"CREATE INDEX IF NOT EXISTS idx_response_ownership_expires ON response_ownership(expires_at)",
	"CREATE INDEX IF NOT EXISTS idx_response_ownership_account ON response_ownership(account_id)",
	"CREATE INDEX IF NOT EXISTS idx_response_ownership_client_key ON response_ownership(client_key_id)",
	"CREATE INDEX IF NOT EXISTS idx_web_response_states_expires ON web_response_states(expires_at)",
	"CREATE INDEX IF NOT EXISTS idx_web_response_states_account ON web_response_states(account_id, created_at DESC)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_client_created ON media_jobs(client_key_id, created_at DESC)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_recovery ON media_jobs(status, lease_until, created_at, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_usage_recovery ON media_jobs(status, usage_recorded_at, completed_at, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_assets_created ON media_assets(created_at DESC, id)",
}

// InitializeSchema 以当前持久化模型作为首版数据库结构基线。
func (d *Database) InitializeSchema(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	// all 作用域会让 Build 与 Web 共用 UA、健康度和冷却状态，升级时直接移除旧节点。
	if db.Migrator().HasTable(&egressNodeModel{}) {
		if err := db.Where("scope = ?", "all").Delete(&egressNodeModel{}).Error; err != nil {
			return fmt.Errorf("清理旧版所有域出口节点: %w", err)
		}
	}
	if err := db.AutoMigrate(schemaModels...); err != nil {
		return fmt.Errorf("初始化数据库表: %w", err)
	}
	if err := d.ensureConsoleConstraints(ctx); err != nil {
		return fmt.Errorf("迁移 Console 数据库约束: %w", err)
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

type consoleConstraint struct {
	model any
	table string
	name  string
}

func (d *Database) ensureConsoleConstraints(ctx context.Context) error {
	constraints := []consoleConstraint{
		{model: &accountModel{}, table: "provider_accounts", name: "chk_accounts_provider"},
		{model: &modelRouteModel{}, table: "model_routes", name: "chk_model_routes_provider"},
		{model: &requestAuditModel{}, table: "request_audits", name: "chk_request_audits_provider"},
		{model: &responseOwnershipModel{}, table: "response_ownership", name: "chk_response_ownership_provider"},
		{model: &egressNodeModel{}, table: "egress_nodes", name: "chk_egress_nodes_specific_scope"},
	}
	migrate := func() error {
		db := d.db.WithContext(ctx)
		for _, value := range constraints {
			definition, err := d.constraintDefinition(ctx, value)
			if err != nil {
				return err
			}
			if strings.Contains(definition, "grok_console") {
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
