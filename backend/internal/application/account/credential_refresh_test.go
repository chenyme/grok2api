package account

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
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

	manual, err := service.ensureCredential(ctx, afterCooldown, true, true, false)
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

func TestEnsureCredentialCollapsesRefreshAcrossServiceInstances(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "credential-refresh-multi-instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAccountRepository(database)
	credential, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "multi-instance", SourceKey: "multi-instance",
		EncryptedAccessToken: "access-0", EncryptedRefreshToken: "refresh-0", ExpiresAt: now.Add(time.Hour),
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &credentialRefreshAdapter{delay: 40 * time.Millisecond}
	registry := provider.NewRegistry(adapter)
	lock := memory.NewLockStore()
	first := NewService(repository, nil, nil, nil, registry, nil, lock)
	second := NewService(repository, nil, nil, nil, registry, nil, lock)
	start := make(chan struct{})
	errors := make(chan error, 2)
	for _, service := range []*Service{first, second} {
		go func(service *Service) {
			<-start
			_, refreshErr := service.EnsureCredential(ctx, credential, true)
			errors <- refreshErr
		}(service)
	}
	close(start)
	for range 2 {
		if err := <-errors; err != nil {
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

func TestCredentialRefreshSchedulerRefreshesOnlyDueAccounts(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return time.Now().UTC() }
	dueAt := now.Add(-time.Minute)
	credential.RefreshDueAt = &dueAt
	credential, err := service.accounts.Update(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	far, _, err := service.accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "far", SourceKey: "far",
		EncryptedAccessToken: "far-access", EncryptedRefreshToken: "far-refresh", ExpiresAt: now.Add(6 * time.Hour),
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.RunCredentialRefresh(runCtx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("credential refresh scheduler did not stop")
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	var updated accountdomain.Credential
	for time.Now().Before(deadline) {
		updated, err = service.accounts.Get(ctx, credential.ID)
		if err == nil && adapter.refreshCount.Load() == 1 && updated.LastRefreshAt != nil && updated.RefreshFailureCount == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if adapter.refreshCount.Load() != 1 {
		t.Fatalf("refresh count = %d", adapter.refreshCount.Load())
	}
	if err != nil {
		t.Fatal(err)
	}
	if updated.RefreshDueAt == nil || !updated.RefreshDueAt.After(time.Now()) || updated.LastRefreshAt == nil || updated.RefreshFailureCount != 0 {
		t.Fatalf("updated credential = %#v", updated)
	}
	farUpdated, err := service.accounts.Get(ctx, far.ID)
	if err != nil {
		t.Fatal(err)
	}
	if farUpdated.EncryptedAccessToken != "far-access" || farUpdated.LastRefreshAt != nil {
		t.Fatalf("far credential was refreshed: %#v", farUpdated)
	}
}

func TestStartupRecoveryPreservesFutureRefreshSchedule(t *testing.T) {
	now := time.Now().UTC()
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return now }
	originalDue := credential.RefreshDueAt

	report, err := service.RecoverCriticalCredentials(context.Background(), 2*time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	if report.CriticalFound != 0 || adapter.refreshCount.Load() != 0 {
		t.Fatalf("report=%#v refreshes=%d", report, adapter.refreshCount.Load())
	}
	stored, err := service.accounts.Get(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if originalDue == nil || stored.RefreshDueAt == nil || !stored.RefreshDueAt.Equal(*originalDue) {
		t.Fatalf("refresh due changed: before=%v after=%v", originalDue, stored.RefreshDueAt)
	}
}

func TestStartupRecoveryRefreshesExpiredCredential(t *testing.T) {
	now := time.Now().UTC()
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return now }
	expired, err := service.accounts.UpdateTokens(context.Background(), credential.ID, credential.EncryptedAccessToken, credential.EncryptedRefreshToken, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if expired.RefreshDueAt == nil || expired.RefreshDueAt.After(now) {
		t.Fatalf("expired refresh due = %v", expired.RefreshDueAt)
	}

	report, err := service.RecoverCriticalCredentials(context.Background(), 2*time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	if report.CriticalFound != 1 || report.Refreshed != 1 || report.Failed != 0 || adapter.refreshCount.Load() != 1 {
		t.Fatalf("report=%#v refreshes=%d", report, adapter.refreshCount.Load())
	}
}

func TestStartupRecoveryRespectsContextBudget(t *testing.T) {
	now := time.Now().UTC()
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return now }
	if _, err := service.accounts.UpdateTokens(context.Background(), credential.ID, credential.EncryptedAccessToken, credential.EncryptedRefreshToken, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	adapter.delay = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := service.RecoverCriticalCredentials(ctx, 2*time.Minute, 100)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("startup recovery exceeded budget: %s", elapsed)
	}
}

func TestCredentialRefreshDueQueryStaysBoundedForLargePool(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	service, _, _ := newCredentialRefreshTestService(t, now)
	values := make([]accountdomain.Credential, 0, 1000)
	for index := range 1000 {
		name := fmt.Sprintf("large-%04d", index)
		values = append(values, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: name, SourceKey: name,
			EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", ExpiresAt: now.Add(time.Minute),
			Enabled: true, AuthStatus: accountdomain.AuthStatusActive, MaxConcurrent: 1,
		})
	}
	if _, err := service.accounts.UpsertManyByIdentity(ctx, values); err != nil {
		t.Fatal(err)
	}
	ids, err := service.accounts.ListDueCredentialRefreshIDs(ctx, now, credentialRefreshBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != credentialRefreshBatchSize {
		t.Fatalf("due batch size = %d", len(ids))
	}
	next, err := service.accounts.NextCredentialRefreshDueAt(ctx)
	if err != nil || next == nil || next.After(now) {
		t.Fatalf("next due = %v, err = %v", next, err)
	}
}

func TestCredentialRefreshFailureDistinguishesTransientAndPermanent(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return now }

	adapter.refreshErr = &provider.CredentialRefreshError{Status: 503, Code: "oauth_unavailable"}
	if _, err := service.EnsureCredential(ctx, credential, true); err == nil {
		t.Fatal("transient refresh unexpectedly succeeded")
	}
	transient, err := service.accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if transient.AuthStatus != accountdomain.AuthStatusActive || transient.RefreshFailureCount != 1 || transient.LastRefreshErrorCode != "oauth_unavailable" || transient.RefreshPermanent || transient.RefreshDueAt == nil || !transient.RefreshDueAt.After(now) {
		t.Fatalf("transient state = %#v", transient)
	}

	service.clearRefreshState(credential.ID)
	adapter.refreshErr = &provider.CredentialRefreshError{Status: 400, Code: "invalid_grant", Permanent: true}
	if _, err := service.EnsureCredential(ctx, transient, true); err == nil {
		t.Fatal("permanent refresh unexpectedly succeeded")
	}
	permanent, err := service.accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if permanent.AuthStatus != accountdomain.AuthStatusActive || permanent.RefreshFailureCount != 2 || permanent.LastRefreshErrorCode != "invalid_grant" || !permanent.RefreshPermanent || permanent.RefreshDueAt == nil || !permanent.RefreshDueAt.Equal(permanent.ExpiresAt) {
		t.Fatalf("permanent with valid token should stay active: %#v", permanent)
	}
	dueIDs, err := service.accounts.ListDueCredentialRefreshIDs(ctx, now, credentialRefreshBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(dueIDs) != 0 {
		t.Fatalf("permanent refresh failure remained immediately due: %#v", dueIDs)
	}
	dueAtExpiry, err := service.accounts.ListDueCredentialRefreshIDs(ctx, permanent.ExpiresAt, credentialRefreshBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(dueAtExpiry) != 1 || dueAtExpiry[0] != credential.ID {
		t.Fatalf("permanent refresh failure was not scheduled at expiry: %#v", dueAtExpiry)
	}
	refreshCount := adapter.refreshCount.Load()
	service.now = func() time.Time { return permanent.ExpiresAt.Add(-time.Minute) }
	usable, err := service.EnsureCredential(ctx, permanent, false)
	if err != nil {
		t.Fatalf("valid access token was rejected after permanent refresh failure: %v", err)
	}
	if usable.EncryptedAccessToken != permanent.EncryptedAccessToken || adapter.refreshCount.Load() != refreshCount {
		t.Fatalf("usable token = %#v, refresh count = %d", usable, adapter.refreshCount.Load())
	}
	service.now = func() time.Time { return now }
	if _, err := service.EnsureCredential(ctx, permanent, true); err == nil {
		t.Fatal("forced retry after permanent failure unexpectedly succeeded")
	}
	permanent, err = service.accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !permanent.RefreshPermanent || permanent.RefreshDueAt == nil || !permanent.RefreshDueAt.Equal(permanent.ExpiresAt) || adapter.refreshCount.Load() != refreshCount {
		t.Fatalf("permanent refresh state retried or changed: %#v, refresh count = %d", permanent, adapter.refreshCount.Load())
	}

	service.clearRefreshState(credential.ID)
	expiredCredential := permanent
	expiredCredential.ExpiresAt = now.Add(-time.Minute)
	if _, err := service.accounts.UpdateTokens(ctx, permanent.ID, permanent.EncryptedAccessToken, permanent.EncryptedRefreshToken, expiredCredential.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	adapter.refreshErr = &provider.CredentialRefreshError{Status: 400, Code: "invalid_grant", Permanent: true}
	expiredState, _ := service.accounts.Get(ctx, credential.ID)
	if expiredState.RefreshPermanent {
		t.Fatalf("token update did not clear permanent refresh failure: %#v", expiredState)
	}
	if _, err := service.EnsureCredential(ctx, expiredState, true); err == nil {
		t.Fatal("permanent refresh with expired token unexpectedly succeeded")
	}
	finalState, err := service.accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalState.AuthStatus != accountdomain.AuthStatusReauthRequired {
		t.Fatalf("permanent with expired token should be reauthRequired: %#v", finalState)
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

func TestBatchRefreshTokensRefreshesOnlySelectedEligibleAccounts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service, refreshable, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return now }
	selected := []uint64{refreshable.ID}
	for _, value := range []accountdomain.Credential{
		{Provider: accountdomain.ProviderBuild, Name: "missing-refresh", SourceKey: "missing-refresh", EncryptedAccessToken: "access", Enabled: true, AuthStatus: accountdomain.AuthStatusActive},
		{Provider: accountdomain.ProviderBuild, Name: "disabled", SourceKey: "disabled", EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", Enabled: true, AuthStatus: accountdomain.AuthStatusActive},
		{Provider: accountdomain.ProviderBuild, Name: "invalid", SourceKey: "invalid", EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", Enabled: true, AuthStatus: accountdomain.AuthStatusActive},
	} {
		created, _, err := service.accounts.UpsertByIdentity(ctx, value)
		if err != nil {
			t.Fatal(err)
		}
		needsUpdate := false
		if value.Name == "disabled" {
			created.Enabled = false
			needsUpdate = true
		}
		if value.Name == "invalid" {
			created.AuthStatus = accountdomain.AuthStatusReauthRequired
			needsUpdate = true
		}
		if needsUpdate {
			created, err = service.accounts.Update(ctx, created)
			if err != nil {
				t.Fatal(err)
			}
		}
		selected = append(selected, created.ID)
	}

	succeeded, failed, skipped, err := service.BatchRefreshTokens(ctx, append(selected, refreshable.ID))
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || failed != 0 || skipped != 3 || adapter.refreshCount.Load() != 1 {
		t.Fatalf("result = %d/%d/%d, refresh count = %d", succeeded, failed, skipped, adapter.refreshCount.Load())
	}
	updated, err := service.accounts.Get(ctx, refreshable.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.EncryptedAccessToken != "access-1" {
		t.Fatalf("refreshed access token = %q", updated.EncryptedAccessToken)
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
	refreshErr   error
}

func (a *credentialRefreshAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderBuild
}

func (a *credentialRefreshAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderBuild, Quota: provider.QuotaBilling,
		Credential: provider.CredentialSurface{
			Refresh: true,
		},
	}
}

func (a *credentialRefreshAdapter) RefreshCredential(ctx context.Context, _ accountdomain.Credential) (provider.RefreshedCredential, error) {
	if a.delay > 0 {
		timer := time.NewTimer(a.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return provider.RefreshedCredential{}, ctx.Err()
		case <-timer.C:
		}
	}
	count := a.refreshCount.Add(1)
	if a.refreshErr != nil {
		return provider.RefreshedCredential{}, a.refreshErr
	}
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
