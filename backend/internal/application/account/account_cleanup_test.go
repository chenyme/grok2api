package account

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestCleanupAccountsDeletesOnlySelectedCurrentStatuses(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "account-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("cleanup-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	service := NewService(repo, nil, nil, memory.NewStickyStore(), nil, cipher, nil)
	service.now = func() time.Time { return now }

	create := func(name string, providerValue accountdomain.Provider, mutate func(*accountdomain.Credential)) uint64 {
		t.Helper()
		value, _, createErr := repo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: providerValue, AuthType: accountdomain.AuthTypeSSO, Name: name, SourceKey: fmt.Sprintf("cleanup-%s", name),
			EncryptedAccessToken: token, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if mutate != nil {
			mutate(&value)
			value, createErr = repo.Update(ctx, value)
			if createErr != nil {
				t.Fatal(createErr)
			}
		}
		return value.ID
	}

	normalID := create("normal", accountdomain.ProviderBuild, nil)
	disabledID := create("disabled", accountdomain.ProviderBuild, func(value *accountdomain.Credential) { value.Enabled = false })
	invalidID := create("invalid", accountdomain.ProviderBuild, func(value *accountdomain.Credential) { value.AuthStatus = accountdomain.AuthStatusReauthRequired })
	coolingID := create("cooling", accountdomain.ProviderBuild, func(value *accountdomain.Credential) { until := now.Add(time.Hour); value.CooldownUntil = &until })
	expiredCooldownID := create("expired-cooldown", accountdomain.ProviderBuild, func(value *accountdomain.Credential) { until := now.Add(-time.Hour); value.CooldownUntil = &until })
	otherProviderID := create("web-disabled", accountdomain.ProviderWeb, func(value *accountdomain.Credential) { value.Enabled = false })

	deleted, err := service.CleanupAccounts(ctx, accountdomain.ProviderBuild, []CleanupStatus{CleanupStatusDisabled, CleanupStatusReauthRequired, CleanupStatusCooldown, CleanupStatusDisabled})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("deleted = %d", deleted)
	}
	for _, id := range []uint64{disabledID, invalidID, coolingID} {
		if _, err := repo.Get(ctx, id); err == nil {
			t.Fatalf("account %d was not deleted", id)
		}
	}
	for _, id := range []uint64{normalID, expiredCooldownID, otherProviderID} {
		if _, err := repo.Get(ctx, id); err != nil {
			t.Fatalf("account %d should remain: %v", id, err)
		}
	}
}

func TestCleanupAccountsRequiresStatus(t *testing.T) {
	service := NewService(nil, nil, nil, nil, nil, nil, nil)
	if _, err := service.CleanupAccounts(context.Background(), accountdomain.ProviderBuild, nil); err == nil {
		t.Fatal("empty cleanup status unexpectedly succeeded")
	}
}
