package account

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func TestBatchDeleteByStatusRejectsActive(t *testing.T) {
	t.Parallel()
	service := newBatchDeleteStatusService(t)
	if _, err := service.BatchDeleteByStatus(context.Background(), string(accountdomain.ProviderBuild), "active"); err == nil {
		t.Fatal("expected error for active status")
	}
}

func TestBatchDeleteByStatusDeletesMatchingAccounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo := newBatchDeleteStatusServiceWithRepo(t)

	keep, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "keep-active", SourceKey: "keep-active",
		EncryptedAccessToken: "token", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	drop1, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "drop-reauth-1", SourceKey: "drop-reauth-1",
		EncryptedAccessToken: "token", Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
	})
	if err != nil {
		t.Fatal(err)
	}
	drop2, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "drop-reauth-2", SourceKey: "drop-reauth-2",
		EncryptedAccessToken: "token", Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
	})
	if err != nil {
		t.Fatal(err)
	}
	// other provider should not be deleted
	_, _, err = repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, Name: "web-reauth", SourceKey: "web-reauth",
		EncryptedAccessToken: "token", Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
	})
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := service.BatchDeleteByStatus(ctx, string(accountdomain.ProviderBuild), "reauthRequired")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if _, err := repo.Get(ctx, keep.ID); err != nil {
		t.Fatalf("active account was deleted: %v", err)
	}
	if _, err := repo.Get(ctx, drop1.ID); err == nil {
		t.Fatal("reauth account 1 still exists")
	}
	if _, err := repo.Get(ctx, drop2.ID); err == nil {
		t.Fatal("reauth account 2 still exists")
	}
}

func newBatchDeleteStatusService(t *testing.T) *Service {
	t.Helper()
	service, _ := newBatchDeleteStatusServiceWithRepo(t)
	return service
}

func newBatchDeleteStatusServiceWithRepo(t *testing.T) (*Service, *relational.AccountRepository) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "batch-delete-status.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	service := NewService(repo, nil, nil, nil, nil, nil, nil)
	service.now = func() time.Time { return time.Now().UTC() }
	return service, repo
}
