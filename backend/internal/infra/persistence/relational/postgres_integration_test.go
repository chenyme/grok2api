package relational

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
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
}
