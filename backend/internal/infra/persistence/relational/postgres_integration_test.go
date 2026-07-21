package relational

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const testPostgresAdminDSNEnv = "TEST_POSTGRES_ADMIN_DSN"

func TestMain(m *testing.M) {
	cleanup, err := configureTemporaryPostgresIntegrationDatabase()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configure temporary PostgreSQL integration database:", err)
		os.Exit(1)
	}
	code := m.Run()
	if cleanup != nil {
		if err := cleanup(); err != nil {
			fmt.Fprintln(os.Stderr, "drop temporary PostgreSQL integration database:", err)
			code = 1
		}
	}
	os.Exit(code)
}

func configureTemporaryPostgresIntegrationDatabase() (func() error, error) {
	if os.Getenv("TEST_POSTGRES_DSN") != "" {
		return nil, nil
	}
	adminDSN := os.Getenv(testPostgresAdminDSNEnv)
	if adminDSN == "" {
		return nil, nil
	}
	parsed, err := url.Parse(adminDSN)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("%s must be a PostgreSQL URL", testPostgresAdminDSNEnv)
	}
	decodedQuery, decodeErr := url.QueryUnescape(parsed.RawQuery)
	if decodeErr != nil {
		return nil, fmt.Errorf("decode PostgreSQL URL query: %w", decodeErr)
	}
	parsed.RawQuery = decodedQuery
	adminDSN = parsed.String()
	name := fmt.Sprintf("grok2api_phase0_%d", time.Now().UTC().UnixNano())
	admin, err := gorm.Open(postgres.Open(adminDSN), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	adminSQL, err := admin.DB()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	identifier := quotePostgresTestIdentifier(name)
	if err := admin.WithContext(ctx).Exec("CREATE DATABASE " + identifier).Error; err != nil {
		_ = adminSQL.Close()
		return nil, err
	}
	parsed.Path = "/" + name
	if err := os.Setenv("TEST_POSTGRES_DSN", parsed.String()); err != nil {
		_ = admin.WithContext(ctx).Exec("DROP DATABASE IF EXISTS " + identifier).Error
		_ = adminSQL.Close()
		return nil, err
	}
	return func() error {
		defer adminSQL.Close()
		defer os.Unsetenv("TEST_POSTGRES_DSN")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := admin.WithContext(cleanupCtx).Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = ? AND pid <> pg_backend_pid()", name).Error; err != nil {
			return err
		}
		return admin.WithContext(cleanupCtx).Exec("DROP DATABASE IF EXISTS " + identifier).Error
	}, nil
}

func quotePostgresTestIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func TestPostgresConcurrentSchemaInitializationUsesMigrationLock(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	databases := make([]*Database, 2)
	for index := range databases {
		database, err := OpenPostgres(ctx, dsn, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		databases[index] = database
		defer database.Close()
	}
	start := make(chan struct{})
	errorsCh := make(chan error, len(databases))
	var wait sync.WaitGroup
	for _, database := range databases {
		wait.Add(1)
		go func(value *Database) {
			defer wait.Done()
			<-start
			errorsCh <- value.InitializeSchema(ctx)
		}(database)
	}
	close(start)
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

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

func TestPostgresBillingReservationAndAuditSettlementConcurrency(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	database, err := OpenPostgres(ctx, dsn, 20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkey.Key{Name: "postgres-billing", Prefix: "postgres-billing", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 1_000})
	if err != nil {
		t.Fatal(err)
	}

	const workers = 20
	start := make(chan struct{})
	errorsCh := make(chan error, workers)
	var successes atomic.Int64
	var successMu sync.Mutex
	successfulEventIDs := make([]string, 0, 10)
	var wait sync.WaitGroup
	wait.Add(workers)
	for index := range workers {
		go func(index int) {
			defer wait.Done()
			<-start
			eventID := fmt.Sprintf("evt_postgres_reserve_%04d", index)
			reserved, reserveErr := keys.ReserveBillingUsage(ctx, key.ID, eventID, 100, time.Now().UTC().Add(time.Hour))
			switch {
			case reserveErr == nil && reserved:
				successes.Add(1)
				successMu.Lock()
				successfulEventIDs = append(successfulEventIDs, eventID)
				successMu.Unlock()
			case errors.Is(reserveErr, repository.ErrLimitExceeded):
			default:
				errorsCh <- fmt.Errorf("reservation %d: reserved=%v err=%w", index, reserved, reserveErr)
			}
		}(index)
	}
	close(start)
	wait.Wait()
	close(errorsCh)
	for reserveErr := range errorsCh {
		t.Error(reserveErr)
	}
	if t.Failed() {
		return
	}
	if successes.Load() != 10 {
		t.Fatalf("successful PostgreSQL reservations = %d, want 10", successes.Load())
	}
	stored, err := keys.Get(ctx, key.ID)
	if err != nil || stored.ReservedUsageUSDTicks != 1_000 {
		t.Fatalf("reserved billing state = %#v, err = %v", stored, err)
	}

	audits := NewAuditRepository(database)
	for _, eventID := range successfulEventIDs {
		if err := audits.Create(ctx, audit.Record{EventID: eventID, RequestID: eventID, ClientKeyID: key.ID, ModelRouteID: 1, Provider: "grok_build", Operation: audit.OperationResponses, UsageSource: audit.UsageSourceUpstream, StatusCode: 200, CostInUSDTicks: 100, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	stored, err = keys.Get(ctx, key.ID)
	if err != nil || stored.ReservedUsageUSDTicks != 0 || stored.BilledUsageUSDTicks != 1_000 {
		t.Fatalf("settled billing state = %#v, err = %v", stored, err)
	}

	settlementKey, err := keys.Create(ctx, clientkey.Key{Name: "postgres-settlement", Prefix: "postgres-settlement", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	for index := range 10 {
		now := time.Now().UTC()
		eventID := fmt.Sprintf("evt_postgres_cleanup_settle_%04d", index)
		if reserved, reserveErr := keys.ReserveBillingUsage(ctx, settlementKey.ID, eventID, 10, now.Add(-time.Minute)); reserveErr != nil || !reserved {
			t.Fatalf("settlement reservation %d: reserved=%v err=%v", index, reserved, reserveErr)
		}
		start := make(chan struct{})
		errorsCh := make(chan error, 2)
		go func() {
			<-start
			errorsCh <- audits.Create(ctx, audit.Record{EventID: eventID, RequestID: eventID, ClientKeyID: settlementKey.ID, ModelRouteID: 1, Provider: "grok_build", Operation: audit.OperationResponses, UsageSource: audit.UsageSourceUpstream, StatusCode: 200, CostInUSDTicks: 10, CreatedAt: now})
		}()
		go func() {
			<-start
			_, cleanupErr := keys.CleanupExpiredBillingReservations(ctx, now, 1)
			errorsCh <- cleanupErr
		}()
		close(start)
		for range 2 {
			if concurrentErr := <-errorsCh; concurrentErr != nil {
				t.Fatalf("cleanup and settlement %d: %v", index, concurrentErr)
			}
		}
	}
	stored, err = keys.Get(ctx, settlementKey.ID)
	if err != nil || stored.ReservedUsageUSDTicks != 0 || stored.BilledUsageUSDTicks != 100 {
		t.Fatalf("cleanup and settlement billing state = %#v, err = %v", stored, err)
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
