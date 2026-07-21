package model

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestModelProviderFilterAcceptsOnlyKnownProviders(t *testing.T) {
	for _, value := range []string{"", string(account.ProviderBuild), string(account.ProviderWeb), string(account.ProviderConsole)} {
		if !validProviderFilter(value) {
			t.Fatalf("known provider rejected: %q", value)
		}
	}
	if validProviderFilter("cli") {
		t.Fatal("unsupported provider filter was accepted")
	}
}

func TestSyncAggregatesCapabilitiesFromAllAccounts(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-sync.db"))
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

	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	first, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "basic", SourceKey: "basic", EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "premium", SourceKey: "premium", EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	webAccount, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper, Name: "web-super", SourceKey: "web-super", EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &modelCapabilityAdapter{models: map[uint64][]string{
		first.ID:  {"grok-basic"},
		second.ID: {"grok-basic", "grok-premium"},
	}}
	webAdapter := &modelCapabilityAdapter{provider: account.ProviderWeb, models: map[uint64][]string{
		webAccount.ID: {"grok-chat-fast", "grok-chat-auto"},
	}}
	registry := provider.NewRegistry(adapter, webAdapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	service := NewService(modelRepo, accountRepo, accountService, registry)

	count, err := service.Sync(ctx)
	if err != nil || count != 4 {
		t.Fatalf("sync count = %d, err = %v", count, err)
	}
	if attempts := adapter.attemptCount(); attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
	}
	if attempts := webAdapter.attemptCount(); attempts != 1 {
		t.Fatalf("web attempts = %d", attempts)
	}
	candidates, err := accountRepo.ListRoutingCandidates(ctx, account.ProviderBuild, "grok-premium", "")
	if err != nil {
		t.Fatal(err)
	}
	support := make(map[uint64]bool, len(candidates))
	for _, candidate := range candidates {
		if !candidate.ModelCapabilityKnown {
			t.Fatalf("capability unknown for account %d", candidate.Credential.ID)
		}
		support[candidate.Credential.ID] = candidate.SupportsModel
	}
	if support[first.ID] || !support[second.ID] {
		t.Fatalf("support = %#v", support)
	}
	webCandidates, err := accountRepo.ListRoutingCandidates(ctx, account.ProviderWeb, "grok-chat-auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(webCandidates) != 1 || !webCandidates[0].ModelCapabilityKnown || !webCandidates[0].SupportsModel {
		t.Fatalf("web candidates = %#v", webCandidates)
	}
}

func TestSyncAccountNormalizesBuildVideo15ByBillingSuper(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-build-video15.db"))
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

	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)

	// Super on primary Build: upstream exposes only grok-4.5 and fallback is disabled.
	superPrimary, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "super-primary", SourceKey: "super-primary",
		EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Super with fallback: the upstream catalog includes 1.5 and other models.
	superFallback, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "super-fallback", SourceKey: "super-fallback",
		EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive,
		BuildAPIFallback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Free: Billing has no paid signal, but the upstream catalog exposes 1.5.
	freeAccount, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "free-fallback", SourceKey: "free-fallback",
		EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive,
		BuildAPIFallback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Unknown: no Billing snapshot and the upstream catalog exposes 1.5.
	unknownAccount, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "unknown", SourceKey: "unknown",
		EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Web must not implement capability normalization; persist its catalog unchanged.
	webAccount, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper, Name: "web", SourceKey: "web",
		EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if err := accountRepo.SaveBilling(ctx, account.Billing{AccountID: superPrimary.ID, MonthlyLimit: 100, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := accountRepo.SaveBilling(ctx, account.Billing{AccountID: superFallback.ID, OnDemandCap: 50, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := accountRepo.SaveBilling(ctx, account.Billing{AccountID: freeAccount.ID, Used: 1, PlanName: "free", SyncedAt: now}); err != nil {
		t.Fatal(err)
	}

	const video15 = "grok-imagine-video-1.5"
	buildAdapter := &buildCapabilityNormalizerAdapter{modelCapabilityAdapter: &modelCapabilityAdapter{models: map[uint64][]string{
		superPrimary.ID:   {"grok-4.5"},
		superFallback.ID:  {"grok-4.5", video15, "grok-code-fast-1", video15},
		freeAccount.ID:    {"grok-4.5", video15},
		unknownAccount.ID: {video15, "grok-4.5"},
	}}}
	webAdapter := &modelCapabilityAdapter{provider: account.ProviderWeb, models: map[uint64][]string{
		webAccount.ID: {"grok-chat-fast", "grok-imagine-video"},
	}}
	registry := provider.NewRegistry(buildAdapter, webAdapter)
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), memory.NewStickyStore(), registry, cipher, nil)
	service := NewService(modelRepo, accountRepo, accountService, registry)

	for _, id := range []uint64{superPrimary.ID, superFallback.ID, freeAccount.ID, unknownAccount.ID, webAccount.ID} {
		if _, err := service.SyncAccount(ctx, id); err != nil {
			t.Fatalf("sync account %d: %v", id, err)
		}
	}

	assertSupports := func(accountID uint64, upstream string, want bool) {
		t.Helper()
		candidates, listErr := accountRepo.ListRoutingCandidates(ctx, account.ProviderBuild, upstream, "")
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, candidate := range candidates {
			if candidate.Credential.ID != accountID {
				continue
			}
			if !candidate.ModelCapabilityKnown {
				t.Fatalf("account %d capability unknown for %s", accountID, upstream)
			}
			if candidate.SupportsModel != want {
				t.Fatalf("account %d supports %s = %v, want %v", accountID, upstream, candidate.SupportsModel, want)
			}
			return
		}
		t.Fatalf("account %d missing from candidates for %s", accountID, upstream)
	}

	assertSupports(superPrimary.ID, video15, true)
	assertSupports(superFallback.ID, video15, true)
	assertSupports(freeAccount.ID, video15, false)
	assertSupports(unknownAccount.ID, video15, false)
	assertSupports(superPrimary.ID, "grok-4.5", true)
	assertSupports(superFallback.ID, "grok-code-fast-1", true)
	assertSupports(freeAccount.ID, "grok-4.5", true)

	// Build 1.5 routes default to the video capability.
	route, err := modelRepo.GetByPublicID(ctx, "Build/"+video15)
	if err != nil || route.Capability != modeldomain.CapabilityVideo {
		t.Fatalf("build video route = %#v, err = %v", route, err)
	}
	// Web does not use Build normalization and still supports its catalog models.
	webCandidates, err := accountRepo.ListRoutingCandidates(ctx, account.ProviderWeb, "grok-imagine-video", "")
	if err != nil || len(webCandidates) != 1 || !webCandidates[0].SupportsModel {
		t.Fatalf("web candidates = %#v, err = %v", webCandidates, err)
	}
}

func TestSyncAccountRunsUpstreamDiscoveryConcurrently(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-account-sync.db"))
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

	const accountCount = 10
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	adapter := &modelCapabilityAdapter{
		models:  make(map[uint64][]string, accountCount),
		entered: make(chan struct{}, accountCount),
		release: make(chan struct{}),
	}
	accountIDs := make([]uint64, 0, accountCount)
	for index := range accountCount {
		value, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: fmt.Sprintf("account-%d", index), SourceKey: fmt.Sprintf("account-%d", index),
			EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		accountIDs = append(accountIDs, value.ID)
		adapter.models[value.ID] = []string{"grok-shared"}
	}
	registry := provider.NewRegistry(adapter)
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), memory.NewStickyStore(), registry, cipher, nil)
	service := NewService(modelRepo, accountRepo, accountService, registry)

	results := make(chan error, accountCount)
	for _, accountID := range accountIDs {
		go func() {
			_, syncErr := service.SyncAccount(ctx, accountID)
			results <- syncErr
		}()
	}
	deadline := time.NewTimer(time.Second)
	for range accountCount {
		select {
		case <-adapter.entered:
		case <-deadline.C:
			close(adapter.release)
			t.Fatalf("upstream discovery peak = %d, want %d", adapter.peak.Load(), accountCount)
		}
	}
	deadline.Stop()
	close(adapter.release)
	for range accountCount {
		if syncErr := <-results; syncErr != nil {
			t.Fatal(syncErr)
		}
	}
	if adapter.peak.Load() != accountCount {
		t.Fatalf("upstream discovery peak = %d, want %d", adapter.peak.Load(), accountCount)
	}
}

type modelCapabilityAdapter struct {
	provider account.Provider
	mu       sync.Mutex
	models   map[uint64][]string
	attempts []uint64
	entered  chan struct{}
	release  chan struct{}
	active   atomic.Int64
	peak     atomic.Int64
}

// buildCapabilityNormalizerAdapter simulates a Build Adapter implementing optional capability normalization.
type buildCapabilityNormalizerAdapter struct {
	*modelCapabilityAdapter
}

func (a *buildCapabilityNormalizerAdapter) NormalizeAccountModelCapabilities(models []string, billing *account.Billing, credential account.Credential) []string {
	// Match cli.Adapter rules: Super (paid or entitlement) ensures 1.5; otherwise remove it exactly. Ignore BuildAPIFallback.
	const video15 = "grok-imagine-video-1.5"
	super := account.IsBuildSuper(credential, billing)
	result := make([]string, 0, len(models)+1)
	seen := make(map[string]struct{}, len(models)+1)
	hasVideo15 := false
	for _, modelName := range models {
		if modelName == "" {
			continue
		}
		if _, exists := seen[modelName]; exists {
			continue
		}
		if modelName == video15 {
			if !super {
				continue
			}
			hasVideo15 = true
		}
		seen[modelName] = struct{}{}
		result = append(result, modelName)
	}
	if super && !hasVideo15 {
		result = append(result, video15)
	}
	return result
}

func (a *modelCapabilityAdapter) Provider() account.Provider {
	if a.provider == "" {
		return account.ProviderBuild
	}
	return a.provider
}
func (a *modelCapabilityAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: a.Provider()}
}
func (a *modelCapabilityAdapter) ListModels(ctx context.Context, credential account.Credential) ([]string, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, credential.ID)
	models := append([]string(nil), a.models[credential.ID]...)
	a.mu.Unlock()
	if a.entered == nil {
		return models, nil
	}
	current := a.active.Add(1)
	defer a.active.Add(-1)
	for {
		peak := a.peak.Load()
		if current <= peak || a.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	a.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-a.release:
		return models, nil
	}
}
func (a *modelCapabilityAdapter) attemptCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.attempts)
}
func (a *modelCapabilityAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{StatusCode: http.StatusOK}, nil
}
func (a *modelCapabilityAdapter) GetBilling(context.Context, account.Credential) (account.Billing, error) {
	return account.Billing{}, nil
}
func (a *modelCapabilityAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{}, nil
}
func (a *modelCapabilityAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, nil
}
func (a *modelCapabilityAdapter) PollDeviceAuthorization(context.Context, string) (provider.CredentialSeed, error) {
	return provider.CredentialSeed{}, nil
}
func (a *modelCapabilityAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return nil, nil
}
func (a *modelCapabilityAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}
