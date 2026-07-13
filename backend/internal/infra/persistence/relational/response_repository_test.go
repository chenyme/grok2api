package relational

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestResponseRepositoryScopesOwnershipByClientAndExpiry(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "owner", SourceKey: "owner", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	keyValue, err := NewClientKeyRepository(database).Create(ctx, clientkeydomain.Key{Name: "owner", Prefix: "owner-prefix", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewResponseRepository(database)
	value := inferencedomain.ResponseOwnership{ResponseID: "resp_1", AccountID: accountValue.ID, ClientKeyID: keyValue.ID, Provider: account.ProviderBuild, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	if err := repo.Save(ctx, value); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, value.ResponseID, value.ClientKeyID, now)
	if err != nil || got.AccountID != value.AccountID || got.Provider != account.ProviderBuild {
		t.Fatalf("ownership = %#v, err = %v", got, err)
	}
	if _, err := repo.Get(ctx, value.ResponseID, 99, now); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-client lookup err = %v", err)
	}
	if _, err := repo.Get(ctx, value.ResponseID, value.ClientKeyID, now.Add(2*time.Hour)); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("expired lookup err = %v", err)
	}
	deleted, err := repo.DeleteExpired(ctx, now.Add(2*time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("deleted = %d, err = %v", deleted, err)
	}
}
