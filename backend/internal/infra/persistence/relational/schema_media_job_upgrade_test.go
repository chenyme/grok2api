package relational

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
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
	inputField, ok := modelType.FieldByName("InputJSON")
	if !ok {
		t.Fatal("InputJSON field missing")
	}
	inputTag := inputField.Tag.Get("gorm")
	limitLiteral := strconv.Itoa(mediadomain.MaxInputJSONBytes)
	if !strings.Contains(inputTag, "chk_media_jobs_input_json") ||
		!strings.Contains(inputTag, limitLiteral) || strings.Contains(inputTag, "1048576") {
		t.Fatalf("input_json tag = %q, want limit %s", inputTag, limitLiteral)
	}
	countField, ok := modelType.FieldByName("InputImageCount")
	if !ok {
		t.Fatal("InputImageCount field missing")
	}
	countTag := countField.Tag.Get("gorm")
	maxImages := strconv.Itoa(mediadomain.MaxInputImages)
	if !strings.Contains(countTag, "chk_media_jobs_input_image_count") ||
		!strings.Contains(countTag, "IS NULL OR input_image_count BETWEEN 0 AND "+maxImages) {
		t.Fatalf("input_image_count tag = %q, want max %s", countTag, maxImages)
	}
}

func TestMediaJobInputMetadataPendingIndexExists(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-input-pending-index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.db.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`,
		"idx_media_jobs_input_metadata_pending",
	).Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("pending metadata index count = %d", count)
	}
	var sql string
	if err := database.db.WithContext(ctx).Raw(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`,
		"idx_media_jobs_input_metadata_pending",
	).Scan(&sql).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(sql), "input_image_count is null") {
		t.Fatalf("pending index sql = %q", sql)
	}
}

func TestInitializeSchemaUpgradesOnlyPreviousMediaJobInputConstraint(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "previous-media-input.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "previous-media-account", SourceKey: "previous-media-account",
		EncryptedAccessToken: testEncryptedToken, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "previous-media-key", Prefix: "previous-media-key", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	existing := testMediaJob("video_previous_input", accountValue.ID, key.ID, mediadomain.StatusQueued, now)
	existing.InputJSON = `{"image_urls":["data:image/png;base64,AAAA"]}`
	if err := NewMediaJobRepository(database).CreateMediaJob(ctx, existing); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Model(&mediaJobModel{}).Where("id = ?", existing.ID).UpdateColumn("input_image_count", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := downgradeOnlyMediaJobInputConstraint(ctx, database); err != nil {
		t.Fatal(err)
	}
	assertMediaJobSQLLacksNewInputLimit(t, database)

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	assertMediaJobSQLContainsBuild(t, database)
	var migrated mediaJobModel
	if err := database.db.WithContext(ctx).Where("id = ?", existing.ID).First(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	if migrated.InputJSON != existing.InputJSON || migrated.InputImageCount == nil || *migrated.InputImageCount != 1 {
		t.Fatalf("migrated input json len=%d count=%v", len(migrated.InputJSON), migrated.InputImageCount)
	}
	large := testMediaJob("video_previous_large", accountValue.ID, key.ID, mediadomain.StatusQueued, now)
	large.InputJSON = `{"image_urls":["` + strings.Repeat("A", (1<<20)+1) + `"]}`
	large.InputImageCount = 1
	if err := NewMediaJobRepository(database).CreateMediaJob(ctx, large); err != nil {
		t.Fatalf("upgraded input constraint rejected large payload: %v", err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Where("id = ?", existing.ID).First(&migrated).Error; err != nil || migrated.InputImageCount == nil || *migrated.InputImageCount != 1 {
		t.Fatalf("idempotent migration lost existing job: %#v, err=%v", migrated, err)
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
		InputJSON: `{"image_urls":["` + strings.Repeat("A", (1<<20)+1) + `"]}`, CreatedAt: now, UpdatedAt: now,
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
				CONSTRAINT chk_media_jobs_input_json CHECK (length(input_json) <= 1048576),
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
	if !strings.Contains(sql, "1048576") {
		t.Fatalf("legacy media_jobs missing old input_json limit: %s", sql)
	}
}

func assertMediaJobSQLLacksNewInputLimit(t *testing.T, database *Database) {
	t.Helper()
	sql := mediaJobsTableSQL(t, database)
	if !strings.Contains(sql, "1048576") || strings.Contains(sql, strconv.Itoa(mediadomain.MaxInputJSONBytes)) {
		t.Fatalf("previous media_jobs input constraint was not installed: %s", sql)
	}
	if !strings.Contains(sql, "grok_build") || !strings.Contains(strings.ToUpper(sql), "ON DELETE SET NULL") {
		t.Fatalf("fixture changed unrelated v3.0.6 constraints: %s", sql)
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
	limitLiteral := strconv.Itoa(mediadomain.MaxInputJSONBytes)
	if !strings.Contains(sql, limitLiteral) || strings.Contains(sql, "1048576") {
		t.Fatalf("media_jobs input_json limit was not upgraded to %s: %s", limitLiteral, sql)
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

func downgradeOnlyMediaJobInputConstraint(ctx context.Context, database *Database) error {
	return database.withSQLiteForeignKeysDisabled(ctx, func() error {
		db := database.db.WithContext(ctx)
		var currentSQL string
		if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", "media_jobs").Scan(&currentSQL).Error; err != nil {
			return err
		}
		createPattern := regexp.MustCompile("(?i)^CREATE TABLE\\s+[`\"]?media_jobs[`\"]?")
		previousSQL := createPattern.ReplaceAllString(currentSQL, "CREATE TABLE media_jobs_previous")
		previousSQL = strings.Replace(previousSQL, strconv.Itoa(mediadomain.MaxInputJSONBytes), "1048576", 1)
		if previousSQL == currentSQL || !strings.Contains(previousSQL, "1048576") {
			return fmt.Errorf("无法构造上一版本 media_jobs schema")
		}
		if err := db.Exec(previousSQL).Error; err != nil {
			return err
		}
		if err := db.Exec("INSERT INTO media_jobs_previous SELECT * FROM media_jobs").Error; err != nil {
			return err
		}
		if err := db.Exec("DROP TABLE media_jobs").Error; err != nil {
			return err
		}
		return db.Exec("ALTER TABLE media_jobs_previous RENAME TO media_jobs").Error
	})
}
