package relational

import (
	"context"
	"os"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestPostgresAccountBatchUpdateBeyondAdminPageLimit(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	database, err := OpenPostgres(ctx, dsn, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	rows := seedBatchUpdateAccounts(t, database, 2001)
	ids := accountModelIDs(rows)
	maxConcurrent := 3

	updated, err := NewAccountRepository(database).UpdateMany(ctx, account.ProviderBuild, ids, repository.AccountUpdates{MaxConcurrent: &maxConcurrent})
	if err != nil {
		t.Fatal(err)
	}
	if updated != int64(len(ids)) {
		t.Fatalf("updated = %d, want %d", updated, len(ids))
	}
	var count int64
	if err := database.db.Model(&accountModel{}).Where("id IN ? AND max_concurrent = ?", ids, maxConcurrent).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != int64(len(ids)) {
		t.Fatalf("stored count = %d, want %d", count, len(ids))
	}
}
