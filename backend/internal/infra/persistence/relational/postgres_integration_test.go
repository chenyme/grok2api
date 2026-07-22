package relational

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
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
	batchRecords := make([]audit.Record, 0, len(successfulEventIDs))
	for _, eventID := range successfulEventIDs {
		batchRecords = append(batchRecords, audit.Record{
			EventID: eventID, RequestID: eventID, ClientKeyID: key.ID, ModelRouteID: 1,
			Provider: "grok_build", Operation: audit.OperationResponses, UsageSource: audit.UsageSourceUpstream,
			StatusCode: 200, CostInUSDTicks: 100, CreatedAt: time.Now().UTC(),
			Attempts: []audit.Attempt{{Number: 1, Source: audit.AttemptSourceCredential, Stage: "credential", StartedAt: time.Now().UTC()}},
		})
	}
	if err := audits.CreateBatch(ctx, batchRecords); err != nil {
		t.Fatal(err)
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

func TestPostgresAuditWriterRecoversAfterTerminatedTransaction(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkey.Key{Name: "postgres-terminated-writer", Prefix: "postgres-terminated-writer", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	const eventID = "evt_postgres_terminated_writer_0001"
	if reserved, reserveErr := keys.ReserveBillingUsage(ctx, key.ID, eventID, 100, time.Now().UTC().Add(time.Hour)); reserveErr != nil || !reserved {
		t.Fatalf("reserve billing usage: reserved=%v err=%v", reserved, reserveErr)
	}

	blocker := database.db.WithContext(ctx).Begin()
	if blocker.Error != nil {
		t.Fatal(blocker.Error)
	}
	if err := blocker.Exec("SELECT id FROM client_keys WHERE id = ? FOR UPDATE", key.ID).Error; err != nil {
		t.Fatal(err)
	}

	service := auditapp.NewService(NewAuditRepository(database), slog.New(slog.NewTextHandler(io.Discard, nil)), 32, 16, 100*time.Millisecond)
	service.UpdateWriterConfig(16, 100*time.Millisecond, time.Millisecond)
	service.Start()
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := service.Close(closeCtx); err != nil {
			t.Errorf("close audit service: %v", err)
		}
	}()
	defer blocker.Rollback()
	record := audit.Record{
		EventID: eventID, RequestID: eventID, ClientKeyID: key.ID, ModelRouteID: 1,
		Provider: "grok_build", Operation: audit.OperationResponses, UsageSource: audit.UsageSourceUpstream,
		StatusCode: 200, CostInUSDTicks: 100, CreatedAt: time.Now().UTC(),
		Attempts: []audit.Attempt{{Number: 1, Source: audit.AttemptSourceCredential, Stage: "credential", StartedAt: time.Now().UTC()}},
	}
	result := make(chan error, 1)
	go func() { result <- service.Create(ctx, record) }()

	pid := waitForPostgresLockWaiter(t, ctx, database.db)
	var terminated bool
	if err := database.db.WithContext(ctx).Raw("SELECT pg_terminate_backend(?)", pid).Scan(&terminated).Error; err != nil {
		t.Fatal(err)
	}
	if !terminated {
		t.Fatalf("PostgreSQL backend %d was not terminated", pid)
	}
	if err := blocker.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("acknowledged audit did not recover after connection termination: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("audit writer recovery timed out: %v", ctx.Err())
	}
	assertPostgresAuditSettlement(t, ctx, database, keys, key.ID, eventID, 100, 1)

	if err := NewAuditRepository(database).CreateBatch(ctx, []audit.Record{record}); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	assertPostgresAuditSettlement(t, ctx, database, keys, key.ID, eventID, 100, 1)
}

func TestPostgresAuditBatchRollsBackOnLockTimeoutAndRecovers(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkey.Key{Name: "postgres-lock-timeout", Prefix: "postgres-lock-timeout", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	const eventID = "evt_postgres_lock_timeout_0001"
	if reserved, reserveErr := keys.ReserveBillingUsage(ctx, key.ID, eventID, 100, time.Now().UTC().Add(time.Hour)); reserveErr != nil || !reserved {
		t.Fatalf("reserve billing usage: reserved=%v err=%v", reserved, reserveErr)
	}

	timeoutDSN, err := postgresDSNWithOption(dsn, "-c lock_timeout=150ms")
	if err != nil {
		t.Fatal(err)
	}
	timeoutDatabase, err := OpenPostgres(ctx, timeoutDSN, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer timeoutDatabase.Close()
	timeoutAudits := NewAuditRepository(timeoutDatabase)
	blocker := database.db.WithContext(ctx).Begin()
	if blocker.Error != nil {
		t.Fatal(blocker.Error)
	}
	defer blocker.Rollback()
	if err := blocker.Exec("SELECT id FROM client_keys WHERE id = ? FOR UPDATE", key.ID).Error; err != nil {
		t.Fatal(err)
	}
	record := audit.Record{
		EventID: eventID, RequestID: eventID, ClientKeyID: key.ID, ModelRouteID: 1,
		Provider: "grok_build", Operation: audit.OperationResponses, UsageSource: audit.UsageSourceUpstream,
		StatusCode: 200, CostInUSDTicks: 100, CreatedAt: time.Now().UTC(),
		Attempts: []audit.Attempt{{Number: 1, Source: audit.AttemptSourceUpstreamHTTP, Stage: "response", StartedAt: time.Now().UTC()}},
	}
	if err := timeoutAudits.CreateBatch(ctx, []audit.Record{record}); err == nil {
		t.Fatal("audit batch unexpectedly succeeded while the client key lock was held")
	} else if !strings.Contains(err.Error(), "SQLSTATE 55P03") {
		t.Fatalf("audit batch failed for an unexpected reason: %v", err)
	}
	assertPostgresAuditSettlement(t, ctx, database, keys, key.ID, eventID, 0, 0)
	stored, err := keys.Get(ctx, key.ID)
	if err != nil || stored.ReservedUsageUSDTicks != 100 {
		t.Fatalf("reservation was not preserved after rollback: key=%#v err=%v", stored, err)
	}
	if err := blocker.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	if err := timeoutAudits.CreateBatch(ctx, []audit.Record{record}); err != nil {
		t.Fatalf("retry after lock release: %v", err)
	}
	assertPostgresAuditSettlement(t, ctx, database, keys, key.ID, eventID, 100, 1)
}

func TestPostgresAuditBatchUsesStableClientKeyLockOrder(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	keys := NewClientKeyRepository(database)
	firstKey, err := keys.Create(ctx, clientkey.Key{Name: "postgres-lock-order-first", Prefix: "postgres-lock-order-first", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := keys.Create(ctx, clientkey.Key{Name: "postgres-lock-order-second", Prefix: "postgres-lock-order-second", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	batchOne := []audit.Record{
		postgresBillingAudit("evt_postgres_lock_order_a1", firstKey.ID, now),
		postgresBillingAudit("evt_postgres_lock_order_b1", secondKey.ID, now),
	}
	batchTwo := []audit.Record{
		postgresBillingAudit("evt_postgres_lock_order_b2", secondKey.ID, now),
		postgresBillingAudit("evt_postgres_lock_order_a2", firstKey.ID, now),
	}
	for _, record := range append(append([]audit.Record(nil), batchOne...), batchTwo...) {
		if reserved, reserveErr := keys.ReserveBillingUsage(ctx, record.ClientKeyID, record.EventID, 10, now.Add(time.Hour)); reserveErr != nil || !reserved {
			t.Fatalf("reserve %s: reserved=%v err=%v", record.EventID, reserved, reserveErr)
		}
	}
	blocker := database.db.WithContext(ctx).Begin()
	if blocker.Error != nil {
		t.Fatal(blocker.Error)
	}
	defer blocker.Rollback()
	if err := blocker.Exec("SELECT id FROM client_keys WHERE id = ? FOR UPDATE", firstKey.ID).Error; err != nil {
		t.Fatal(err)
	}
	audits := NewAuditRepository(database)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- audits.CreateBatch(ctx, batchOne) }()
	go func() { <-start; results <- audits.CreateBatch(ctx, batchTwo) }()
	close(start)
	waitForPostgresLockWaiters(t, ctx, database.db, 2)
	if err := blocker.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("opposite-order audit batches should not deadlock: %v", err)
		}
	}
	for _, keyID := range []uint64{firstKey.ID, secondKey.ID} {
		stored, err := keys.Get(ctx, keyID)
		if err != nil || stored.ReservedUsageUSDTicks != 0 || stored.BilledUsageUSDTicks != 20 {
			t.Fatalf("stable lock order settlement key=%d value=%#v err=%v", keyID, stored, err)
		}
	}
}

func waitForPostgresLockWaiter(t *testing.T, ctx context.Context, database *gorm.DB) int {
	t.Helper()
	return waitForPostgresLockWaiters(t, ctx, database, 1)[0]
}

func waitForPostgresLockWaiters(t *testing.T, ctx context.Context, database *gorm.DB, count int) []int {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var rows []postgresPIDRow
		err := database.WithContext(ctx).Raw(`
			SELECT pid
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%client_keys%'
			ORDER BY query_start
		`).Scan(&rows).Error
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) >= count {
			pids := make([]int, count)
			for index := range count {
				pids[index] = int(rows[index].PID)
			}
			return pids
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for PostgreSQL lock waiter: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

type postgresPIDRow struct {
	PID int64 `gorm:"column:pid"`
}

func postgresDSNWithOption(dsn, option string) (string, error) {
	if _, err := url.ParseRequestURI(dsn); err != nil {
		return "", err
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "options=" + url.QueryEscape(option), nil
}

func postgresBillingAudit(eventID string, keyID uint64, createdAt time.Time) audit.Record {
	return audit.Record{
		EventID: eventID, RequestID: eventID, ClientKeyID: keyID, ModelRouteID: 1,
		Provider: "grok_build", Operation: audit.OperationResponses, UsageSource: audit.UsageSourceUpstream,
		StatusCode: 200, CostInUSDTicks: 10, CreatedAt: createdAt,
	}
}

func assertPostgresAuditSettlement(t *testing.T, ctx context.Context, database *Database, keys *ClientKeyRepository, keyID uint64, eventID string, billed int64, attempts int64) {
	t.Helper()
	var row requestAuditModel
	err := database.db.WithContext(ctx).Where("event_id = ?", eventID).Take(&row).Error
	if billed == 0 {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("audit should be absent after rollback: row=%#v err=%v", row, err)
		}
	} else if err != nil {
		t.Fatalf("load settled audit: %v", err)
	}
	var attemptCount int64
	if row.ID != 0 {
		if err := database.db.WithContext(ctx).Model(&requestAuditAttemptModel{}).Where("audit_id = ?", row.ID).Count(&attemptCount).Error; err != nil {
			t.Fatal(err)
		}
	}
	if attemptCount != attempts {
		t.Fatalf("attempt count = %d, want %d", attemptCount, attempts)
	}
	stored, err := keys.Get(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BilledUsageUSDTicks != billed {
		t.Fatalf("billed usage = %d, want %d", stored.BilledUsageUSDTicks, billed)
	}
	if billed > 0 && stored.ReservedUsageUSDTicks != 0 {
		t.Fatalf("reserved usage = %d after settlement", stored.ReservedUsageUSDTicks)
	}
}

func TestPostgresP1RouteLookupAndBoundedResponseCleanup(t *testing.T) {
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

	aliasRoute := modelRouteModel{PublicID: "Build/grok-p1-legacy", Provider: string(account.ProviderBuild), UpstreamModel: "grok-p1-legacy", Capability: "responses", Origin: "manual", Enabled: true}
	directRoute := modelRouteModel{PublicID: "Web/grok-p1", Provider: string(account.ProviderWeb), UpstreamModel: "grok-p1", Capability: "chat", Origin: "manual", Enabled: true}
	if err := database.db.WithContext(ctx).Create(&aliasRoute).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&directRoute).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&modelRouteAliasModel{Alias: "grok-p1", ModelRouteID: aliasRoute.ID, CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	modelRepository := NewModelRepository(database)
	route, err := modelRepository.GetByPublicIDIncludingDisabled(ctx, "grok-p1")
	if err != nil || route.ID != directRoute.ID {
		t.Fatalf("direct route priority = %#v, err = %v", route, err)
	}

	accounts := NewAccountRepository(database)
	accountValue, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: "postgres-p1", SourceKey: "postgres-p1", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewClientKeyRepository(database).Create(ctx, clientkey.Key{Name: "postgres-p1", Prefix: "postgres-p1", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	responses := NewResponseRepository(database)
	now := time.Now().UTC()
	for index := range 3 {
		id := fmt.Sprintf("postgres-p1-%d", index)
		if err := responses.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: "ownership-" + id, AccountID: accountValue.ID, ClientKeyID: key.ID, Provider: account.ProviderWeb, ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := responses.SaveWebState(ctx, inferencedomain.WebResponseState{ResponseID: "state-" + id, AccountID: accountValue.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: "{}", Status: "completed", ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := responses.DeleteExpired(ctx, now, 2, 1)
	if err != nil || first.OwnershipDeleted != 2 || first.WebStateDeleted != 1 || !first.HasMore {
		t.Fatalf("first bounded cleanup = %#v, err = %v", first, err)
	}
	second, err := responses.DeleteExpired(ctx, now, 10, 10)
	if err != nil || second.OwnershipDeleted != 1 || second.WebStateDeleted != 2 || second.HasMore {
		t.Fatalf("second bounded cleanup = %#v, err = %v", second, err)
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
