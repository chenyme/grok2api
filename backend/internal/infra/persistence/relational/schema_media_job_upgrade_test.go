package relational

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
)

func TestMediaJobModelTagsAllowBuildVideoProviderAndScope(t *testing.T) {
	modelType := reflect.TypeOf(mediaJobModel{})
	providerField, ok := modelType.FieldByName("Provider")
	if !ok {
		t.Fatal("Provider field missing")
	}
	providerTag := providerField.Tag.Get("gorm")
	if !strings.Contains(providerTag, "chk_media_jobs_provider") ||
		!strings.Contains(providerTag, "grok_web") ||
		!strings.Contains(providerTag, "grok_build") ||
		strings.Contains(providerTag, "grok_console") {
		t.Fatalf("provider tag = %q", providerTag)
	}
	scopeField, ok := modelType.FieldByName("EgressScope")
	if !ok {
		t.Fatal("EgressScope field missing")
	}
	scopeTag := scopeField.Tag.Get("gorm")
	if !strings.Contains(scopeTag, "chk_media_jobs_egress_scope") ||
		!strings.Contains(scopeTag, "''") ||
		!strings.Contains(scopeTag, "grok_web") ||
		!strings.Contains(scopeTag, "grok_build") ||
		strings.Contains(scopeTag, "grok_console") {
		t.Fatalf("egress_scope tag = %q", scopeTag)
	}
}

func TestInitializeSchemaUpgradesMediaJobChecksForBuild(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-media-jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// 先建立当前完整 schema，再将 media_jobs 回退到仅允许 Web 的旧 CHECK。
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recreateLegacyMediaJobsTable(ctx, database); err != nil {
		t.Fatal(err)
	}
	assertMediaJobSQLLacksBuild(t, database)

	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider:             accountdomain.ProviderBuild,
		AuthType:             accountdomain.AuthTypeOAuth,
		Name:                 "build-media-account",
		SourceKey:            "build-media-account",
		EncryptedAccessToken: testEncryptedToken,
		AuthStatus:           accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{
		Name: "build-media-key", Prefix: "build-media-key", SecretHash: testSecretHash,
		EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}

	// 旧约束下 Build provider 必须失败。
	now := time.Now().UTC()
	legacyJob := mediadomain.Job{
		ID: "video_build_legacy_blocked", RequestID: "request-build-legacy",
		ClientKeyID: key.ID, ClientKeyName: key.Name,
		AccountID: accountValue.ID, AccountName: accountValue.Name,
		Provider: "grok_build", Model: "grok-imagine-video-1.5", ModelRouteID: 1,
		UpstreamModel: "grok-imagine-video-1.5", Prompt: "test", Seconds: 6,
		Size: "16:9", Quality: "720p", Status: mediadomain.StatusQueued,
		InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}
	jobs := NewMediaJobRepository(database)
	if err := jobs.CreateMediaJob(ctx, legacyJob); err == nil {
		t.Fatal("legacy schema unexpectedly accepted grok_build media job")
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	assertMediaJobSQLContainsBuild(t, database)

	// 升级后 Build job 与 grok_build egress scope 均可写入。
	legacyJob.EgressScope = "grok_build"
	if err := jobs.CreateMediaJob(ctx, legacyJob); err != nil {
		t.Fatalf("upgraded schema rejected build media job: %v", err)
	}
	webJob := mediadomain.Job{
		ID: "video_web_still_ok", RequestID: "request-web-ok",
		ClientKeyID: key.ID, ClientKeyName: key.Name,
		AccountID: accountValue.ID, AccountName: accountValue.Name,
		Provider: "grok_web", Model: "grok-imagine-video", ModelRouteID: 2,
		UpstreamModel: "grok-imagine-video", Prompt: "web", Seconds: 6,
		Size: "16:9", Quality: "720p", Status: mediadomain.StatusQueued,
		EgressScope: "grok_web", InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := jobs.CreateMediaJob(ctx, webJob); err != nil {
		t.Fatalf("web media job regression: %v", err)
	}

	// 非法 provider / scope 仍应被拒绝。
	invalidProvider := legacyJob
	invalidProvider.ID = "video_console_blocked"
	invalidProvider.RequestID = "request-console-blocked"
	invalidProvider.Provider = "grok_console"
	if err := jobs.CreateMediaJob(ctx, invalidProvider); err == nil {
		t.Fatal("console provider was accepted for media jobs")
	}
	invalidScope := legacyJob
	invalidScope.ID = "video_scope_blocked"
	invalidScope.RequestID = "request-scope-blocked"
	invalidScope.EgressScope = "grok_console"
	if err := jobs.CreateMediaJob(ctx, invalidScope); err == nil {
		t.Fatal("console egress scope was accepted for media jobs")
	}

	// 重复迁移幂等，且已有 Build 任务仍在。
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	assertMediaJobSQLContainsBuild(t, database)
	stored, err := jobs.GetMediaJob(ctx, legacyJob.ID, key.ID)
	if err != nil || stored.Provider != "grok_build" || stored.EgressScope != "grok_build" {
		t.Fatalf("stored build job = %#v, err = %v", stored, err)
	}
}

func recreateLegacyMediaJobsTable(ctx context.Context, database *Database) error {
	return database.withSQLiteForeignKeysDisabled(ctx, func() error {
		db := database.db.WithContext(ctx)
		if err := db.Exec("DROP TABLE IF EXISTS media_jobs").Error; err != nil {
			return err
		}
		// 仅保留升级测试所需字段，CHECK 复刻生产旧定义。
		return db.Exec(`
			CREATE TABLE media_jobs (
				id text PRIMARY KEY,
				request_id text NOT NULL,
				client_key_id integer NOT NULL,
				client_key_name text NOT NULL DEFAULT '',
				account_id integer NOT NULL,
				account_name text NOT NULL DEFAULT '',
				egress_node_id integer,
				egress_node_name text NOT NULL DEFAULT '',
				egress_scope text NOT NULL DEFAULT '',
				egress_mode text NOT NULL DEFAULT '',
				provider text NOT NULL,
				model text NOT NULL,
				model_route_id integer NOT NULL,
				upstream_model text NOT NULL,
				prompt text NOT NULL,
				seconds integer NOT NULL,
				size text NOT NULL,
				quality text NOT NULL,
				status text NOT NULL,
				progress integer NOT NULL DEFAULT 0,
				input_json text NOT NULL DEFAULT '{}',
				upstream_url text NOT NULL DEFAULT '',
				content_type text NOT NULL DEFAULT '',
				error_code text NOT NULL DEFAULT '',
				error_message text NOT NULL DEFAULT '',
				lease_until datetime,
				claim_token text NOT NULL DEFAULT '',
				created_at datetime NOT NULL,
				updated_at datetime NOT NULL,
				completed_at datetime,
				usage_recorded_at datetime,
				CONSTRAINT chk_media_jobs_provider CHECK (provider IN ('grok_web')),
				CONSTRAINT chk_media_jobs_egress_scope CHECK (egress_scope IN ('','grok_web')),
				CONSTRAINT fk_media_jobs_account FOREIGN KEY (account_id) REFERENCES provider_accounts(id) ON UPDATE CASCADE ON DELETE RESTRICT,
				CONSTRAINT fk_media_jobs_client_key FOREIGN KEY (client_key_id) REFERENCES client_keys(id) ON UPDATE CASCADE ON DELETE RESTRICT
			)
		`).Error
	})
}

func assertMediaJobSQLLacksBuild(t *testing.T, database *Database) {
	t.Helper()
	sql := mediaJobsTableSQL(t, database)
	if strings.Contains(sql, "grok_build") {
		t.Fatalf("legacy media_jobs unexpectedly contains grok_build: %s", sql)
	}
	if !strings.Contains(sql, "grok_web") {
		t.Fatalf("legacy media_jobs missing grok_web: %s", sql)
	}
}

func assertMediaJobSQLContainsBuild(t *testing.T, database *Database) {
	t.Helper()
	sql := mediaJobsTableSQL(t, database)
	if !strings.Contains(sql, "grok_build") {
		t.Fatalf("media_jobs was not upgraded with grok_build: %s", sql)
	}
	if strings.Contains(sql, "grok_console") {
		t.Fatalf("media_jobs unexpectedly allows console: %s", sql)
	}
	if !strings.Contains(strings.ToUpper(sql), "ON DELETE SET NULL") {
		t.Fatalf("media_jobs account history is not detached on account delete: %s", sql)
	}
}

func mediaJobsTableSQL(t *testing.T, database *Database) string {
	t.Helper()
	var sql string
	if err := database.db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", "media_jobs").Scan(&sql).Error; err != nil {
		t.Fatal(err)
	}
	return sql
}
