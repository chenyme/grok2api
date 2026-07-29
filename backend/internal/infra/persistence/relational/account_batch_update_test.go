package relational

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAccountRepositoryUpdateManyChunksLargeAccountPool(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, 2001)
	ids := accountModelIDs(rows)
	maxConcurrent := 3

	updated, err := repo.UpdateMany(ctx, account.ProviderBuild, ids, repository.AccountUpdates{MaxConcurrent: &maxConcurrent})
	if err != nil {
		t.Fatal(err)
	}
	if updated != int64(len(ids)) {
		t.Fatalf("updated = %d, want %d", updated, len(ids))
	}
	var count int64
	if err := database.db.Model(&accountModel{}).Where("max_concurrent = ?", maxConcurrent).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != int64(len(ids)) {
		t.Fatalf("stored count = %d, want %d", count, len(ids))
	}
}

func TestAccountRepositoryUpdateManyRollsBackAllChunks(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, accountUpdateBatchSize+1)
	ids := accountModelIDs(rows)
	failingID := ids[len(ids)-1]
	trigger := fmt.Sprintf(`CREATE TRIGGER fail_batch_account_update BEFORE UPDATE OF max_concurrent ON provider_accounts WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'forced batch failure'); END`, failingID)
	if err := database.db.Exec(trigger).Error; err != nil {
		t.Fatal(err)
	}
	maxConcurrent := 3

	updated, err := repo.UpdateMany(ctx, account.ProviderBuild, ids, repository.AccountUpdates{MaxConcurrent: &maxConcurrent})
	if err == nil || updated != 0 {
		t.Fatalf("updated = %d, error = %v", updated, err)
	}
	var changed int64
	if err := database.db.Model(&accountModel{}).Where("id IN ? AND max_concurrent = ?", ids, maxConcurrent).Count(&changed).Error; err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatalf("transaction left %d changed rows", changed)
	}
}

func TestAccountRepositoryUpdateManyRejectsMixedProviderBeforeWriting(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, accountUpdateBatchSize+1)
	ids := accountModelIDs(rows)
	if err := database.db.Model(&accountModel{}).Where("id = ?", ids[len(ids)-1]).Update("provider", account.ProviderWeb).Error; err != nil {
		t.Fatal(err)
	}
	maxConcurrent := 3

	updated, err := repo.UpdateMany(ctx, account.ProviderBuild, ids, repository.AccountUpdates{MaxConcurrent: &maxConcurrent})
	if !errors.Is(err, repository.ErrAccountPoolMismatch) || updated != 0 {
		t.Fatalf("updated = %d, error = %v", updated, err)
	}
	var changed int64
	if err := database.db.Model(&accountModel{}).Where("id IN ? AND max_concurrent = ?", ids, maxConcurrent).Count(&changed).Error; err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatalf("provider mismatch left %d changed rows", changed)
	}
}

func seedBatchUpdateAccounts(t *testing.T, database *Database, count int) []accountModel {
	t.Helper()
	now := time.Now().UTC()
	rows := make([]accountModel, count)
	for index := range rows {
		rows[index] = accountModel{
			IdentityKey: fmt.Sprintf("%064x", index+1),
			Provider:    string(account.ProviderBuild), Name: fmt.Sprintf("batch-%d", index+1), SourceKey: fmt.Sprintf("batch-%d", index+1),
			Enabled: true, AuthStatus: string(account.AuthStatusActive), Priority: account.DefaultPriority, MaxConcurrent: account.DefaultMaxConcurrent,
			BuildRouteMode: string(account.BuildRouteAuto), CreatedAt: now, UpdatedAt: now,
		}
	}
	if err := database.db.CreateInBatches(&rows, 200).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

func accountModelIDs(rows []accountModel) []uint64 {
	ids := make([]uint64, len(rows))
	for index := range rows {
		ids[index] = rows[index].ID
	}
	return ids
}
