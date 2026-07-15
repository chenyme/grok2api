package account

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestBatchRefreshQuotaSupportsWebAndConsole(t *testing.T) {
	for _, providerValue := range []accountdomain.Provider{accountdomain.ProviderWeb, accountdomain.ProviderConsole} {
		t.Run(string(providerValue), func(t *testing.T) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota.db"))
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
			encryptedToken, err := cipher.Encrypt("test-token")
			if err != nil {
				t.Fatal(err)
			}
			repository := relational.NewAccountRepository(database)
			ids := make([]uint64, 0, 2)
			for index := 1; index <= 2; index++ {
				value, _, createErr := repository.UpsertByIdentity(ctx, accountdomain.Credential{
					Provider: providerValue, AuthType: accountdomain.AuthTypeSSO,
					Name: fmt.Sprintf("account-%d", index), SourceKey: fmt.Sprintf("source-%d", index),
					EncryptedAccessToken: encryptedToken, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
				})
				if createErr != nil {
					t.Fatal(createErr)
				}
				ids = append(ids, value.ID)
			}
			adapter := &selectedQuotaAdapter{providerValue: providerValue}
			service := NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, memory.NewLockStore())

			succeeded, failed, err := service.BatchRefreshQuota(ctx, ids)
			if err != nil {
				t.Fatal(err)
			}
			if succeeded != 2 || failed != 0 || adapter.calls.Load() != 2 {
				t.Fatalf("succeeded=%d failed=%d calls=%d", succeeded, failed, adapter.calls.Load())
			}
			windows, err := repository.GetQuotaWindows(ctx, ids)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				if len(windows[id]) != 1 || windows[id][0].Remaining != 7 {
					t.Fatalf("account %d windows = %#v", id, windows[id])
				}
			}
		})
	}
}

type selectedQuotaAdapter struct {
	providerValue accountdomain.Provider
	calls         atomic.Int64
}

func (a *selectedQuotaAdapter) Provider() accountdomain.Provider { return a.providerValue }

func (a *selectedQuotaAdapter) SyncQuota(_ context.Context, _ accountdomain.Credential) (provider.QuotaSnapshot, error) {
	a.calls.Add(1)
	now := time.Now().UTC()
	return provider.QuotaSnapshot{
		Tier: accountdomain.WebTierSuper, SyncedAt: now,
		Windows: []accountdomain.QuotaWindow{{Mode: "default", Remaining: 7, Total: 10, SyncedAt: &now, UpdatedAt: now}},
	}, nil
}

func (a *selectedQuotaAdapter) SyncQuotaMode(_ context.Context, _ accountdomain.Credential, mode string) (accountdomain.QuotaWindow, error) {
	return accountdomain.QuotaWindow{Mode: mode}, nil
}
