package account

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestWebQuotaRefreshDeduplicatesPerMode(t *testing.T) {
	service := NewService(nil, nil, nil, nil, nil, nil, nil)
	service.QueueWebQuotaRefresh(42, "fast")
	service.QueueWebQuotaRefresh(42, "expert")
	service.QueueWebQuotaRefresh(42, "fast")

	service.quotaRefreshMu.Lock()
	defer service.quotaRefreshMu.Unlock()
	if len(service.quotaRefreshes) != 2 {
		t.Fatalf("refresh states = %#v", service.quotaRefreshes)
	}
	if !service.quotaRefreshes["42:fast"].pending {
		t.Fatal("duplicate fast refresh was not marked pending")
	}
	if service.quotaRefreshes["42:expert"].pending {
		t.Fatal("independent expert refresh was incorrectly marked pending")
	}
	if len(service.quotaRefreshQueue) != 2 {
		t.Fatalf("queued refreshes = %d", len(service.quotaRefreshQueue))
	}
}

func TestRefreshQuotaModeDoesNotTriggerFullProviderSyncForAutoTier(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-mode.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "web-auto", SourceKey: "web-auto", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, WebTier: accountdomain.WebTierAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &quotaCountingAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)

	window, err := service.RefreshQuotaMode(ctx, credential.ID, "fast")
	if err != nil {
		t.Fatal(err)
	}
	if window.Mode != "fast" || adapter.modeCalls.Load() != 1 || adapter.fullCalls.Load() != 0 {
		t.Fatalf("window = %#v, mode calls = %d, full calls = %d", window, adapter.modeCalls.Load(), adapter.fullCalls.Load())
	}
	stored, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebTier != accountdomain.WebTierAuto {
		t.Fatalf("single-mode sync changed tier to %q", stored.WebTier)
	}

	service.QueueQuotaRefresh(credential.ID, "fast")
	service.QueueQuotaRefresh(credential.ID, "fast")
	request := <-service.quotaRefreshQueue
	service.runWebQuotaRefresh(ctx, request)
	if adapter.modeCalls.Load() != 2 || adapter.fullCalls.Load() != 0 {
		t.Fatalf("coalesced mode calls = %d, full calls = %d", adapter.modeCalls.Load(), adapter.fullCalls.Load())
	}
	service.quotaRefreshMu.Lock()
	_, queued := service.quotaRefreshes[request.key]
	service.quotaRefreshMu.Unlock()
	if queued {
		t.Fatal("completed coalesced refresh retained queue state")
	}

	service.refreshLock = deniedQuotaRefreshLock{}
	service.QueueQuotaRefresh(credential.ID, "fast")
	request = <-service.quotaRefreshQueue
	service.runWebQuotaRefresh(ctx, request)
	if adapter.modeCalls.Load() != 2 {
		t.Fatalf("worker without distributed lease made %d mode calls", adapter.modeCalls.Load())
	}
}

type deniedQuotaRefreshLock struct{}

func (deniedQuotaRefreshLock) Acquire(context.Context, string, time.Duration) (func(), bool, error) {
	return nil, false, nil
}

type quotaCountingAdapter struct {
	modeCalls atomic.Int64
	fullCalls atomic.Int64
}

func (a *quotaCountingAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderWeb }

func (a *quotaCountingAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderWeb, ModelNamespace: accountdomain.ProviderWeb.ModelNamespace(),
		Quota: provider.QuotaRemoteWindow, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeSSO},
	}
}

func (a *quotaCountingAdapter) SyncQuota(context.Context, accountdomain.Credential) (provider.QuotaSnapshot, error) {
	a.fullCalls.Add(1)
	return provider.QuotaSnapshot{}, nil
}

func (a *quotaCountingAdapter) SyncQuotaMode(_ context.Context, credential accountdomain.Credential, mode string) (accountdomain.QuotaWindow, error) {
	a.modeCalls.Add(1)
	now := time.Now().UTC()
	resetAt := now.Add(time.Hour)
	return accountdomain.QuotaWindow{
		AccountID: credential.ID, Mode: mode, Remaining: 0, Total: 30,
		WindowSeconds: 3600, ResetAt: &resetAt, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now,
	}, nil
}
