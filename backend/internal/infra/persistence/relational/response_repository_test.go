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
	value := inferencedomain.ResponseOwnership{ResponseID: "resp_1", AccountID: accountValue.ID, ClientKeyID: keyValue.ID, Provider: account.ProviderBuild, PromptCacheKey: "cache-key", ReasoningReplayKey: "replay-key", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	if err := repo.Save(ctx, value); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, value.ResponseID, value.ClientKeyID, now)
	if err != nil || got.AccountID != value.AccountID || got.Provider != account.ProviderBuild || got.PromptCacheKey != value.PromptCacheKey || got.ReasoningReplayKey != value.ReasoningReplayKey {
		t.Fatalf("ownership = %#v, err = %v", got, err)
	}
	if _, err := repo.Get(ctx, value.ResponseID, 99, now); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-client lookup err = %v", err)
	}
	if _, err := repo.Get(ctx, value.ResponseID, value.ClientKeyID, now.Add(2*time.Hour)); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("expired lookup err = %v", err)
	}
	deleted, err := repo.DeleteExpired(ctx, now.Add(2*time.Hour), 10, 10)
	if err != nil || deleted.OwnershipDeleted != 1 || deleted.WebStateDeleted != 0 {
		t.Fatalf("deleted = %#v, err = %v", deleted, err)
	}
}

func TestResponseRepositoryDeletesExpiredRowsInBoundedIndependentBatches(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "response-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	accounts := NewAccountRepository(database)
	accountValue, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: "cleanup-owner", SourceKey: "cleanup-owner", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	keyValue, err := NewClientKeyRepository(database).Create(ctx, clientkeydomain.Key{Name: "cleanup-key", Prefix: "cleanup-prefix", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewResponseRepository(database)
	for index := range 3 {
		responseID := "expired-ownership-" + string(rune('a'+index))
		if err := repo.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: responseID, AccountID: accountValue.ID, ClientKeyID: keyValue.ID, Provider: account.ProviderWeb, ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveWebState(ctx, inferencedomain.WebResponseState{ResponseID: "expired-state-" + string(rune('a'+index)), AccountID: accountValue.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: "{}", Status: "completed", ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: "active-ownership", AccountID: accountValue.ID, ClientKeyID: keyValue.ID, Provider: account.ProviderWeb, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWebState(ctx, inferencedomain.WebResponseState{ResponseID: "active-state", AccountID: accountValue.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: "{}", Status: "completed", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	first, err := repo.DeleteExpired(ctx, now, 2, 1)
	if err != nil || first.OwnershipDeleted != 2 || first.WebStateDeleted != 1 || !first.HasMore {
		t.Fatalf("first cleanup = %#v, err = %v", first, err)
	}
	second, err := repo.DeleteExpired(ctx, now, 10, 10)
	if err != nil || second.OwnershipDeleted != 1 || second.WebStateDeleted != 2 || second.HasMore {
		t.Fatalf("second cleanup = %#v, err = %v", second, err)
	}
	if _, err := repo.Get(ctx, "active-ownership", keyValue.ID, now); err != nil {
		t.Fatalf("active ownership was removed: %v", err)
	}
	if _, err := repo.GetWebState(ctx, "active-state", now); err != nil {
		t.Fatalf("active web state was removed: %v", err)
	}
}

func TestResponseOwnershipIdentityMigrationPreservesExistingRows(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "responses-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "legacy-owner", SourceKey: "legacy-owner", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	keyValue, err := NewClientKeyRepository(database).Create(ctx, clientkeydomain.Key{Name: "legacy-owner", Prefix: "legacy-prefix", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewResponseRepository(database)
	value := inferencedomain.ResponseOwnership{ResponseID: "resp_legacy", AccountID: accountValue.ID, ClientKeyID: keyValue.ID, Provider: account.ProviderBuild, PromptCacheKey: "old-cache", ReasoningReplayKey: "old-replay", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	if err := repo.Save(ctx, value); err != nil {
		t.Fatal(err)
	}
	if err := database.withSQLiteForeignKeysDisabled(ctx, func() error {
		statements := []string{
			`CREATE TABLE response_ownership_legacy (
				response_id text PRIMARY KEY,
				account_id integer NOT NULL,
				client_key_id integer NOT NULL,
				provider text NOT NULL,
				expires_at datetime NOT NULL,
				created_at datetime NOT NULL,
				updated_at datetime NOT NULL,
				CONSTRAINT fk_response_ownership_account FOREIGN KEY (account_id) REFERENCES provider_accounts(id) ON UPDATE CASCADE ON DELETE CASCADE,
				CONSTRAINT fk_response_ownership_client_key FOREIGN KEY (client_key_id) REFERENCES client_keys(id) ON UPDATE CASCADE ON DELETE CASCADE
			)`,
			`INSERT INTO response_ownership_legacy (response_id, account_id, client_key_id, provider, expires_at, created_at, updated_at)
			 SELECT response_id, account_id, client_key_id, provider, expires_at, created_at, updated_at FROM response_ownership`,
			`DROP TABLE response_ownership`,
			`ALTER TABLE response_ownership_legacy RENAME TO response_ownership`,
		}
		for _, statement := range statements {
			if err := database.db.WithContext(ctx).Exec(statement).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("upgrade response ownership identity columns: %v", err)
	}
	got, err := repo.Get(ctx, value.ResponseID, value.ClientKeyID, now)
	if err != nil || got.AccountID != value.AccountID || got.Provider != value.Provider || got.PromptCacheKey != "" || got.ReasoningReplayKey != "" {
		t.Fatalf("migrated ownership = %#v, err = %v", got, err)
	}
}
