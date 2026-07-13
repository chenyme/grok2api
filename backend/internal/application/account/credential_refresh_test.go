package account

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestEnsureCredentialReusesRotatedTokenAndThrottlesForcedRefresh(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return now }

	first, err := service.EnsureCredential(ctx, credential, true)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.refreshCount.Load() != 1 || first.EncryptedAccessToken != "access-1" {
		t.Fatalf("first refresh = %#v, count = %d", first, adapter.refreshCount.Load())
	}

	fromStaleRequest, err := service.EnsureCredential(ctx, credential, true)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.refreshCount.Load() != 1 || fromStaleRequest.EncryptedAccessToken != first.EncryptedAccessToken {
		t.Fatalf("stale request caused another refresh: count = %d", adapter.refreshCount.Load())
	}

	duringCooldown, err := service.EnsureCredential(ctx, first, true)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.refreshCount.Load() != 1 || duringCooldown.EncryptedAccessToken != first.EncryptedAccessToken {
		t.Fatalf("forced refresh cooldown failed: count = %d", adapter.refreshCount.Load())
	}

	now = now.Add(forcedRefreshMinInterval + time.Second)
	afterCooldown, err := service.EnsureCredential(ctx, first, true)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.refreshCount.Load() != 2 || afterCooldown.EncryptedAccessToken != "access-2" {
		t.Fatalf("refresh after cooldown = %#v, count = %d", afterCooldown, adapter.refreshCount.Load())
	}

	manual, err := service.ensureCredential(ctx, afterCooldown, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.refreshCount.Load() != 3 || manual.EncryptedAccessToken != "access-3" {
		t.Fatalf("manual refresh did not bypass cooldown: count = %d", adapter.refreshCount.Load())
	}
}

func TestEnsureCredentialCollapsesConcurrentForcedRefreshes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return now }
	adapter.delay = 30 * time.Millisecond

	const callers = 20
	start := make(chan struct{})
	errors := make(chan error, callers)
	var workers sync.WaitGroup
	workers.Add(callers)
	for range callers {
		go func() {
			defer workers.Done()
			<-start
			value, err := service.EnsureCredential(ctx, credential, true)
			if err == nil && value.EncryptedAccessToken != "access-1" {
				err = fmt.Errorf("access token = %q", value.EncryptedAccessToken)
			}
			errors <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if adapter.refreshCount.Load() != 1 {
		t.Fatalf("refresh count = %d", adapter.refreshCount.Load())
	}
}

func TestEnsureCredentialRefreshesWhenAccessTokenIsMissing(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return now }
	credential, err := service.accounts.UpdateTokens(ctx, credential.ID, "", "refresh-only", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	refreshed, err := service.EnsureCredential(ctx, credential, false)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.refreshCount.Load() != 1 || refreshed.EncryptedAccessToken != "access-1" {
		t.Fatalf("refresh-only credential was not refreshed: %#v, count = %d", refreshed, adapter.refreshCount.Load())
	}
}

func TestRefreshAllTokensSkipsUnrefreshableAccounts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	service, _, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return now }
	for _, value := range []accountdomain.Credential{
		{Provider: accountdomain.ProviderBuild, Name: "refreshable-2", SourceKey: "refreshable-2", EncryptedAccessToken: "access-2", EncryptedRefreshToken: "refresh-2", ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive},
		{Provider: accountdomain.ProviderBuild, Name: "not-refreshable", SourceKey: "not-refreshable", EncryptedAccessToken: "access-3", ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive},
	} {
		if _, _, err := service.accounts.UpsertByIdentity(ctx, value); err != nil {
			t.Fatal(err)
		}
	}

	progress := make([][2]int, 0, 3)
	succeeded, failed, skipped, err := service.RefreshAllTokensWithProgress(ctx, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 2 || failed != 0 || skipped != 1 || adapter.refreshCount.Load() != 2 {
		t.Fatalf("result = %d/%d/%d, refresh count = %d", succeeded, failed, skipped, adapter.refreshCount.Load())
	}
	if len(progress) != 3 || progress[0] != [2]int{0, 2} || progress[1] != [2]int{1, 2} || progress[2] != [2]int{2, 2} {
		t.Fatalf("progress = %#v", progress)
	}
}

func TestRefreshBillingCollapsesConcurrentRequests(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	adapter.billingDelay = 30 * time.Millisecond
	const callers = 20
	start := make(chan struct{})
	errors := make(chan error, callers)
	var workers sync.WaitGroup
	workers.Add(callers)
	for range callers {
		go func() {
			defer workers.Done()
			<-start
			_, err := service.RefreshBilling(ctx, credential.ID)
			errors <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if adapter.billingCount.Load() != 1 {
		t.Fatalf("billing count = %d", adapter.billingCount.Load())
	}
}

func newCredentialRefreshTestService(t *testing.T, now time.Time) (*Service, accountdomain.Credential, *credentialRefreshAdapter) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "credential-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAccountRepository(database)
	credential, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider:              accountdomain.ProviderBuild,
		Name:                  "refresh-test",
		SourceKey:             "refresh-test",
		EncryptedAccessToken:  "access-0",
		EncryptedRefreshToken: "refresh-0",
		ExpiresAt:             now.Add(time.Hour),
		Enabled:               true,
		AuthStatus:            accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &credentialRefreshAdapter{}
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	return service, credential, adapter
}

type credentialRefreshAdapter struct {
	refreshCount atomic.Int64
	billingCount atomic.Int64
	delay        time.Duration
	billingDelay time.Duration
	billing      accountdomain.Billing
	billingErr   error
}

func (a *credentialRefreshAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderBuild
}

func (a *credentialRefreshAdapter) RefreshCredential(context.Context, accountdomain.Credential) (provider.RefreshedCredential, error) {
	if a.delay > 0 {
		time.Sleep(a.delay)
	}
	count := a.refreshCount.Add(1)
	return provider.RefreshedCredential{EncryptedAccessToken: fmt.Sprintf("access-%d", count), EncryptedRefreshToken: fmt.Sprintf("refresh-%d", count), ExpiresAt: time.Now().UTC().Add(time.Hour)}, nil
}

func (a *credentialRefreshAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return nil, nil
}

func (a *credentialRefreshAdapter) ListModels(context.Context, accountdomain.Credential) ([]string, error) {
	return nil, nil
}

func (a *credentialRefreshAdapter) GetBilling(context.Context, accountdomain.Credential) (accountdomain.Billing, error) {
	if a.billingDelay > 0 {
		time.Sleep(a.billingDelay)
	}
	a.billingCount.Add(1)
	return a.billing, a.billingErr
}

func (a *credentialRefreshAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, nil
}

func (a *credentialRefreshAdapter) PollDeviceAuthorization(context.Context, string) (provider.CredentialSeed, error) {
	return provider.CredentialSeed{}, nil
}

func (a *credentialRefreshAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return nil, nil
}
func (a *credentialRefreshAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}
