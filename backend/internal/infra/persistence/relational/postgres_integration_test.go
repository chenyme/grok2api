package relational

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func TestPostgresRepositoriesIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	verifyPostgresMediaJobInputConstraintUpgrade(t, ctx, database)
	repository := NewAccountRepository(database)
	created, wasCreated, err := repository.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "postgres", SourceKey: "postgres-integration-" + time.Now().UTC().Format("150405.000000"),
		EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil || !wasCreated || created.ID == 0 {
		t.Fatalf("account = %#v, created = %v, err = %v", created, wasCreated, err)
	}
	loaded, err := repository.Get(ctx, created.ID)
	if err != nil || loaded.SourceKey != created.SourceKey {
		t.Fatalf("loaded = %#v, err = %v", loaded, err)
	}
	if err := repository.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	unique := time.Now().UTC().Format("20060102150405.000000000")
	digestBytes := sha256.Sum256([]byte(unique))
	digest := hex.EncodeToString(digestBytes[:])
	identity := "sso_" + digest[:32]
	userID := "postgres-linked-" + unique
	web, _, err := repository.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "postgres-web", SourceKey: "sso:" + digest,
		UserID: userID, EgressIdentity: identity, EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	build, _, err := repository.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "postgres-build", SourceKey: "postgres-build-" + unique,
		UserID: userID, EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	console, _, err := repository.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "postgres-console", SourceKey: "console-sso:" + digest,
		EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReconcileProviderLinks(ctx, web.ID); err != nil {
		t.Fatal(err)
	}
	web, err = repository.Get(ctx, web.ID)
	if err != nil || len(web.LinkedAccounts) != 2 {
		t.Fatalf("postgres linked accounts = %#v, err = %v", web.LinkedAccounts, err)
	}
	otherConsole, _, err := repository.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "postgres-console-conflict", SourceKey: "console-conflict-" + unique,
		EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&webConsoleAccountLinkModel{
		WebAccountID: web.ID, ConsoleAccountID: otherConsole.ID, CreatedAt: time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("postgres web/console one-to-one constraint was not enforced")
	}
	if err := repository.Delete(ctx, web.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{build.ID, console.ID} {
		linked, getErr := repository.Get(ctx, id)
		if getErr != nil {
			t.Fatalf("deleting Web removed linked account %d: %v", id, getErr)
		}
		if len(linked.LinkedAccounts) != 0 {
			t.Fatalf("deleting Web retained links for account %d: %#v", id, linked.LinkedAccounts)
		}
	}
	for _, model := range []any{&accountProviderLinkModel{}, &webConsoleAccountLinkModel{}} {
		var remainingLinks int64
		if err := database.db.WithContext(ctx).Model(model).Where("web_account_id = ?", web.ID).Count(&remainingLinks).Error; err != nil || remainingLinks != 0 {
			t.Fatalf("postgres Web relation cascade model=%T count=%d err=%v", model, remainingLinks, err)
		}
	}
	for _, id := range []uint64{build.ID, console.ID, otherConsole.ID} {
		if err := repository.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
}

func verifyPostgresMediaJobInputConstraintUpgrade(t *testing.T, ctx context.Context, database *Database) {
	t.Helper()
	tx := database.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("ALTER TABLE media_jobs DROP CONSTRAINT IF EXISTS chk_media_jobs_input_json").Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec("ALTER TABLE media_jobs ADD CONSTRAINT chk_media_jobs_input_json CHECK (length(input_json) <= 1048576) NOT VALID").Error; err != nil {
		t.Fatal(err)
	}
	testDatabase := &Database{db: tx, dialect: "postgres"}
	if err := testDatabase.ensureMediaJobInputConstraint(ctx); err != nil {
		t.Fatal(err)
	}
	definition, err := testDatabase.constraintDefinition(ctx, consoleConstraint{model: &mediaJobModel{}, table: "media_jobs", name: "chk_media_jobs_input_json"})
	if err != nil || !strings.Contains(definition, strconv.Itoa(media.MaxInputJSONBytes)) || strings.Contains(definition, "1048576") {
		t.Fatalf("postgres input constraint = %q, err=%v", definition, err)
	}
	if err := testDatabase.ensureMediaJobInputConstraint(ctx); err != nil {
		t.Fatalf("postgres input constraint migration is not idempotent: %v", err)
	}
}
