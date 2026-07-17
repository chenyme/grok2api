package accountsync

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type billingStub struct {
	mu          sync.Mutex
	hasSnapshot bool
	checks      int
	syncs       int
	checkErr    error
	syncErr     error
}

type accountReaderStub struct {
	provider accountdomain.Provider
	quota    provider.QuotaKind
}

func (s accountReaderStub) Get(context.Context, uint64) (accountapp.View, error) {
	return accountapp.View{Credential: accountdomain.Credential{Provider: s.provider}}, nil
}

func (s accountReaderStub) ProviderDefinition(value accountdomain.Provider) (provider.Definition, bool) {
	quota := provider.QuotaBilling
	if s.quota != "" {
		quota = s.quota
	} else if value == accountdomain.ProviderWeb {
		quota = provider.QuotaRemoteWindow
	} else if value == accountdomain.ProviderConsole {
		quota = provider.QuotaLocalWindow
	}
	return provider.Definition{Provider: value, Quota: quota}, value.IsValid()
}

type quotaStub struct {
	hasSnapshot bool
	checks      int
	syncs       int
}

func (s *quotaStub) HasQuotaWindows(context.Context, uint64) (bool, error) {
	s.checks++
	return s.hasSnapshot, nil
}

func (s *quotaStub) RefreshQuota(context.Context, uint64) ([]accountdomain.QuotaWindow, error) {
	s.syncs++
	return []accountdomain.QuotaWindow{{Mode: "console", Remaining: 20}}, nil
}

func (s *billingStub) HasBillingSnapshot(context.Context, uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks++
	return s.hasSnapshot, s.checkErr
}

func (s *billingStub) RefreshBilling(context.Context, uint64) (accountdomain.Billing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncs++
	return accountdomain.Billing{}, s.syncErr
}

func (s *billingStub) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks, s.syncs
}

type modelStub struct {
	mu          sync.Mutex
	hasSnapshot bool
	checks      int
	syncs       int
	checkErr    error
	syncErr     error
}

func (s *modelStub) HasSuccessfulAccountSync(context.Context, uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks++
	return s.hasSnapshot, s.checkErr
}

func (s *modelStub) SyncAccount(context.Context, uint64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncs++
	return 1, s.syncErr
}

func (s *modelStub) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks, s.syncs
}

func TestSyncAccountSkipsExistingSnapshots(t *testing.T) {
	billing := &billingStub{hasSnapshot: true}
	models := &modelStub{hasSnapshot: true}
	service := NewService(slog.Default(), accountReaderStub{provider: accountdomain.ProviderBuild}, billing, nil, models)

	if err := service.syncAccount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}

	billingChecks, billingSyncs := billing.counts()
	modelChecks, modelSyncs := models.counts()
	if billingChecks != 1 || billingSyncs != 0 || modelChecks != 1 || modelSyncs != 0 {
		t.Fatalf("billing = %d/%d, models = %d/%d", billingChecks, billingSyncs, modelChecks, modelSyncs)
	}
}

func TestSyncAccountFetchesOnlyMissingSnapshots(t *testing.T) {
	billing := &billingStub{hasSnapshot: true}
	models := &modelStub{}
	service := NewService(slog.Default(), accountReaderStub{provider: accountdomain.ProviderBuild}, billing, nil, models)

	if err := service.syncAccount(context.Background(), 7); err != nil {
		t.Fatal(err)
	}

	_, billingSyncs := billing.counts()
	_, modelSyncs := models.counts()
	if billingSyncs != 0 || modelSyncs != 1 {
		t.Fatalf("billing syncs = %d, model syncs = %d", billingSyncs, modelSyncs)
	}
}

func TestSyncAccountRecomputesModelsAfterCreatingBillingSnapshot(t *testing.T) {
	billing := &billingStub{}
	models := &modelStub{hasSnapshot: true}
	service := NewService(slog.Default(), accountReaderStub{provider: accountdomain.ProviderBuild}, billing, nil, models)

	if err := service.syncAccount(context.Background(), 8); err != nil {
		t.Fatal(err)
	}

	_, billingSyncs := billing.counts()
	_, modelSyncs := models.counts()
	if billingSyncs != 1 || modelSyncs != 1 {
		t.Fatalf("billing syncs = %d, model syncs = %d", billingSyncs, modelSyncs)
	}
}

func TestSyncAccountUsesQuotaForConsoleProvider(t *testing.T) {
	billing := &billingStub{}
	quota := &quotaStub{}
	models := &modelStub{hasSnapshot: true}
	service := NewService(slog.Default(), accountReaderStub{provider: accountdomain.ProviderConsole}, billing, quota, models)

	if err := service.syncAccount(context.Background(), 9); err != nil {
		t.Fatal(err)
	}

	billingChecks, billingSyncs := billing.counts()
	if billingChecks != 0 || billingSyncs != 0 || quota.checks != 1 || quota.syncs != 1 {
		t.Fatalf("billing = %d/%d, quota = %d/%d", billingChecks, billingSyncs, quota.checks, quota.syncs)
	}
}

func TestSyncAccountUsesDeclaredQuotaPolicyInsteadOfProviderName(t *testing.T) {
	billing := &billingStub{}
	quota := &quotaStub{}
	models := &modelStub{hasSnapshot: true}
	reader := accountReaderStub{provider: accountdomain.ProviderBuild, quota: provider.QuotaRemoteWindow}
	service := NewService(slog.Default(), reader, billing, quota, models)

	if err := service.syncAccount(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	billingChecks, billingSyncs := billing.counts()
	if billingChecks != 0 || billingSyncs != 0 || quota.checks != 1 || quota.syncs != 1 {
		t.Fatalf("billing = %d/%d, quota = %d/%d", billingChecks, billingSyncs, quota.checks, quota.syncs)
	}
}

func TestSyncDeduplicatesAccountsAndWaitsForCompletion(t *testing.T) {
	billing := &billingStub{}
	models := &modelStub{}
	service := NewService(slog.Default(), accountReaderStub{provider: accountdomain.ProviderBuild}, billing, nil, models)
	result := service.Sync(context.Background(), 1, 1, 2, 0)
	if result.Succeeded != 2 || result.Failed != 0 {
		t.Fatalf("result = %#v", result)
	}
	billingChecks, billingSyncs := billing.counts()
	modelChecks, modelSyncs := models.counts()
	if billingChecks != 2 || billingSyncs != 2 || modelChecks != 2 || modelSyncs != 2 {
		t.Fatalf("billing = %d/%d, models = %d/%d", billingChecks, billingSyncs, modelChecks, modelSyncs)
	}
}

func TestSyncStreamStartsBeforeImportCompletesAndDeduplicates(t *testing.T) {
	billing := &billingStub{}
	models := &modelStub{}
	service := NewService(slog.Default(), accountReaderStub{provider: accountdomain.ProviderBuild}, billing, nil, models)
	service.UpdateConcurrency(10)
	input := make(chan uint64)
	done := make(chan Result, 1)
	go func() { done <- service.SyncStream(context.Background(), input) }()

	input <- 1
	deadline := time.Now().Add(time.Second)
	for {
		checks, _ := billing.counts()
		if checks > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream did not start before input closed")
		}
		time.Sleep(time.Millisecond)
	}
	input <- 1
	input <- 2
	close(input)

	result := <-done
	if result.Succeeded != 2 || result.Failed != 0 {
		t.Fatalf("result = %#v", result)
	}
	checks, syncs := billing.counts()
	if checks != 2 || syncs != 2 {
		t.Fatalf("billing = %d/%d", checks, syncs)
	}
}

func TestSyncStreamObservedReportsDeduplicatedCompletion(t *testing.T) {
	service := NewService(slog.Default(), accountReaderStub{provider: accountdomain.ProviderBuild}, &billingStub{}, nil, &modelStub{})
	input := make(chan uint64, 3)
	input <- 1
	input <- 1
	input <- 2
	close(input)
	progress := make([]int, 0, 2)
	totals := make([]int, 0, 2)

	result := service.SyncStreamObserved(context.Background(), input, func(completed, total int) {
		progress = append(progress, completed)
		totals = append(totals, total)
	})
	if result.Succeeded != 2 || len(progress) != 2 || progress[0] != 1 || progress[1] != 2 {
		t.Fatalf("result = %#v, progress = %#v, totals = %#v", result, progress, totals)
	}
	if len(totals) != 2 || totals[0] < progress[0] || totals[1] != 2 {
		t.Fatalf("progress = %#v, totals = %#v", progress, totals)
	}
}

func TestSyncReportsInitialSyncFailure(t *testing.T) {
	billing := &billingStub{syncErr: errors.New("billing unavailable")}
	models := &modelStub{syncErr: errors.New("models unavailable")}
	result := NewService(slog.Default(), accountReaderStub{provider: accountdomain.ProviderBuild}, billing, nil, models).Sync(context.Background(), 9)
	if result.Succeeded != 0 || result.Failed != 1 {
		t.Fatalf("result = %#v", result)
	}
	_, billingSyncs := billing.counts()
	_, modelSyncs := models.counts()
	if billingSyncs != 1 || modelSyncs != 1 {
		t.Fatalf("billing syncs = %d, model syncs = %d", billingSyncs, modelSyncs)
	}
}

func TestInitialSyncDoesNotRepeatUpstreamRequestsForSyncedAccount(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "initial-sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	audits := relational.NewAuditRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "initial", SourceKey: "initial", EncryptedAccessToken: encrypted,
		ExpiresAt: time.Now().UTC().Add(time.Hour), AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &countingAdapter{}
	registry := provider.NewRegistry(adapter)
	accountService := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), memory.NewStickyStore(), registry, cipher, nil)
	modelService := modelapp.NewService(models, accounts, accountService, registry)
	service := NewService(slog.Default(), accountService, accountService, accountService, modelService)

	if err := service.syncAccount(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.syncAccount(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}

	billingCalls, modelCalls := adapter.counts()
	if billingCalls != 1 || modelCalls != 1 {
		t.Fatalf("billing calls = %d, model calls = %d", billingCalls, modelCalls)
	}
}

type countingAdapter struct {
	mu           sync.Mutex
	billingCalls int
	modelCalls   int
}

func (a *countingAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }

func (a *countingAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderBuild, Quota: provider.QuotaBilling,
		Credential: provider.CredentialSurface{Refresh: true},
	}
}

func (a *countingAdapter) ListModels(context.Context, accountdomain.Credential) ([]string, error) {
	a.mu.Lock()
	a.modelCalls++
	a.mu.Unlock()
	return []string{"grok-initial"}, nil
}

func (a *countingAdapter) GetBilling(context.Context, accountdomain.Credential) (accountdomain.Billing, error) {
	a.mu.Lock()
	a.billingCalls++
	a.mu.Unlock()
	return accountdomain.Billing{SyncedAt: time.Now().UTC()}, nil
}

func (a *countingAdapter) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.billingCalls, a.modelCalls
}

func (a *countingAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{StatusCode: http.StatusOK}, nil
}

func (a *countingAdapter) RefreshCredential(context.Context, accountdomain.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{}, nil
}

func (a *countingAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, nil
}

func (a *countingAdapter) PollDeviceAuthorization(context.Context, string) (provider.CredentialSeed, error) {
	return provider.CredentialSeed{}, nil
}

func (a *countingAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return nil, nil
}
func (a *countingAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}
