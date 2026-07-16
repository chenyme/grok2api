package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestGatewayFailsOverBeforeReturningBody(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	first, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "first", SourceKey: "first", EncryptedAccessToken: "one", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 200, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "second", SourceKey: "second", EncryptedAccessToken: "two", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-test"}); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []uint64{first.ID, second.ID} {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, accountID, []string{"grok-test"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{Name: "test-key", Prefix: "test-prefix", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "encrypted-key", Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}

	adapter := &failoverAdapter{firstID: first.ID}
	registry := provider.NewRegistry(adapter)
	cipher := testCipher(t)
	sticky := memory.NewStickyStore()
	concurrency := memory.NewConcurrencyLimiter()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	clientService := clientkeyapp.NewService(nil, nil, nil, 60, 4, nil)
	selector := NewSelector(accountRepo, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientService, registry, selector, responseRepo, 3)
	result, err := service.CreateResponse(ctx, Input{RequestID: "req-1", ClientKey: clientKey, PublicModel: "grok-test", Body: []byte(`{"model":"grok-test"}`), PromptCacheSeed: "claude-session"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{InputTokens: 120, CachedInputTokens: 80, OutputTokens: 30, TotalTokens: 150, ResponseModel: "grok-test-build-free"}, "resp-test", "")
	_ = result.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
	if len(adapter.attempts) != 2 || adapter.attempts[0] != first.ID || adapter.attempts[1] != second.ID {
		t.Fatalf("attempts = %#v", adapter.attempts)
	}
	expectedCacheKey := resolvePromptCacheIdentity(clientKey.ID, account.ProviderBuild, "grok-test", audit.OperationResponses, "", "claude-session")
	if adapter.lastPromptCacheKey != expectedCacheKey {
		t.Fatalf("prompt cache key = %q, want %q", adapter.lastPromptCacheKey, expectedCacheKey)
	}
	observedAccount, err := accountRepo.Get(ctx, second.ID)
	if err != nil || observedAccount.ObservedModel != "grok-test-build-free" {
		t.Fatalf("observed account = %#v, err = %v", observedAccount, err)
	}
	logs, total, err := auditRepo.List(ctx, 0, 10)
	if err != nil || total != 1 || logs[0].AccountID == nil || *logs[0].AccountID != second.ID || logs[0].ClientKeyName != "test-key" || logs[0].ModelPublicID != "grok-test" || logs[0].ModelUpstreamModel != "Build/grok-test" || logs[0].AccountName != "second" || logs[0].CachedInputTokens != 80 || logs[0].AttemptCount != 0 {
		t.Fatalf("audit = %#v, %d, %v", logs, total, err)
	}
	detail, err := auditRepo.Get(ctx, logs[0].ID)
	if err != nil || len(detail.Attempts) != 0 {
		t.Fatalf("audit detail = %#v, err = %v", detail, err)
	}
	ownership, err := responseRepo.Get(ctx, "resp-test", clientKey.ID, time.Now().UTC())
	if err != nil || ownership.AccountID != second.ID {
		t.Fatalf("ownership = %#v, err = %v", ownership, err)
	}

	adapter.resetAttempts()
	continued, err := service.CreateResponse(ctx, Input{RequestID: "req-2", ClientKey: clientKey, PublicModel: "grok-test", PreviousResponseID: "resp-test", Body: []byte(`{"model":"grok-test","previous_response_id":"resp-test"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(continued.Body)
	continued.Finalize(Usage{}, "resp-next", "")
	_ = continued.Body.Close()
	if len(adapter.attempts) != 1 || adapter.attempts[0] != second.ID {
		t.Fatalf("continued attempts = %#v", adapter.attempts)
	}

	adapter.resetAttempts()
	resource, err := service.GetResponse(ctx, ResourceInput{ClientKey: clientKey, ResponseID: "resp-test", RawQuery: "include=reasoning.encrypted_content"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resource.Body)
	resource.Finalize(Usage{}, "", "")
	_ = resource.Body.Close()
	if adapter.lastPath != "/responses/resp-test?include=reasoning.encrypted_content" || adapter.lastMethod != http.MethodGet || len(adapter.attempts) != 1 || adapter.attempts[0] != second.ID {
		t.Fatalf("resource request = %s %s, attempts = %#v", adapter.lastMethod, adapter.lastPath, adapter.attempts)
	}

	deleted, err := service.DeleteResponse(ctx, ResourceInput{ClientKey: clientKey, ResponseID: "resp-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(deleted.Body)
	deleted.Finalize(Usage{}, "", "")
	_ = deleted.Body.Close()
	if _, err := responseRepo.Get(ctx, "resp-test", clientKey.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("deleted ownership err = %v", err)
	}

	adapter.setResourceStatus(http.StatusNotFound)
	missing, err := service.GetResponse(ctx, ResourceInput{ClientKey: clientKey, ResponseID: "resp-next"})
	if err != nil {
		t.Fatal(err)
	}
	_ = missing.Body.Close()
	missing.Finalize(Usage{}, "", "")
	if _, err := responseRepo.Get(ctx, "resp-next", clientKey.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stale ownership err = %v", err)
	}
}

func TestSelectConversationRouteRespectsClientKeyAcrossSharedPublicModel(t *testing.T) {
	registry := provider.NewRegistry(&failoverAdapter{}, statelessConsoleAdapter{})
	service := &Service{
		clientKeys: clientkeyapp.NewService(nil, nil, nil, 60, 4, nil),
		providers:  registry,
	}
	routes := []modeldomain.Route{
		{ID: 10, PublicID: "Build/grok-shared", Provider: account.ProviderBuild, UpstreamModel: "grok-shared"},
		{ID: 20, PublicID: "Console/grok-shared", Provider: account.ProviderConsole, UpstreamModel: "grok-shared"},
	}
	selected, err := service.selectConversationRoute(routes, clientkey.Key{AllowedModels: []uint64{20}}, audit.OperationResponses, "/responses", false, nil)
	if err != nil || selected.ID != 20 {
		t.Fatalf("selected route = %#v, err = %v", selected, err)
	}
}

func TestSelectMediaRouteSkipsSameNamedConversationRoute(t *testing.T) {
	registry := provider.NewRegistry(&failoverAdapter{}, &webImageStreamAdapter{})
	service := &Service{
		clientKeys: clientkeyapp.NewService(nil, nil, nil, 60, 4, nil),
		providers:  registry,
	}
	routes := []modeldomain.Route{
		{ID: 10, PublicID: "Build/grok-shared", Provider: account.ProviderBuild, UpstreamModel: "grok-shared", Capability: modeldomain.CapabilityResponses},
		{ID: 20, PublicID: "Web/grok-shared", Provider: account.ProviderWeb, UpstreamModel: "grok-shared", Capability: modeldomain.CapabilityImage},
	}
	selected, err := service.selectMediaRoute(routes, clientkey.Key{}, modeldomain.CapabilityImage, func(providerValue account.Provider) bool {
		_, ok := registry.ImageGeneration(providerValue)
		return ok
	})
	if err != nil || selected.ID != 20 {
		t.Fatalf("selected route = %#v, err = %v", selected, err)
	}
}

func TestGenerateImageReturnsWhenEveryCredentialRefreshFails(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-credential-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	now := time.Now().UTC()
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
		Name: "expired-image", SourceKey: "expired-image", EncryptedAccessToken: "expired", EncryptedRefreshToken: "refresh",
		ExpiresAt: now.Add(-time.Minute), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: "image-credential-failure", Provider: account.ProviderBuild, UpstreamModel: "image-credential-failure",
		Capability: modeldomain.CapabilityImage, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"image-credential-failure"}, now); err != nil {
		t.Fatal(err)
	}
	adapter := &credentialFailureImageAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)

	_, err = service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-credential-failure", ClientKey: clientkey.Key{ID: 1, Name: "image-key"},
		PublicModel: "image-credential-failure", Prompt: "test", Count: 1, ResponseFormat: "url",
	})
	if !errors.Is(err, ErrNoAvailableAccount) {
		t.Fatalf("error = %v", err)
	}
	if adapter.generationCalls.Load() != 0 {
		t.Fatalf("generation calls = %d", adapter.generationCalls.Load())
	}
}

func TestGatewayDoesNotPersistStatelessConsoleResponses(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-stateless.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "console", SourceKey: "console",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const model = "grok-console-stateless"
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderConsole, []string{model}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "console-key", Prefix: "console", SecretHash: strings.Repeat("c", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := statelessConsoleAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)

	result, err := service.CreateResponse(ctx, Input{RequestID: "req-console", ClientKey: key, PublicModel: model, Body: []byte(`{"model":"grok-console-stateless","input":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "resp-console", "")
	_ = result.Body.Close()
	if _, err := responseRepo.Get(ctx, "resp-console", key.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stateless response ownership err = %v", err)
	}
	if _, err := service.CreateResponse(ctx, Input{RequestID: "req-console-next", ClientKey: key, PublicModel: model, PreviousResponseID: "resp-console", Body: []byte(`{"model":"grok-console-stateless","previous_response_id":"resp-console"}`)}); !errors.Is(err, ErrResponseStateUnsupported) {
		t.Fatalf("previous response error = %v", err)
	}
	if _, err := service.CompactResponse(ctx, Input{RequestID: "req-console-compact", ClientKey: key, PublicModel: model, Body: []byte(`{"model":"grok-console-stateless","input":"hello"}`)}); !errors.Is(err, ErrConversationUnsupported) {
		t.Fatalf("compact response error = %v", err)
	}

	now := time.Now().UTC()
	if err := responseRepo.Save(ctx, inferencedomain.ResponseOwnership{
		ResponseID: "resp-console-stale", AccountID: credential.ID, ClientKeyID: key.ID, Provider: account.ProviderConsole,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetResponse(ctx, ResourceInput{ClientKey: key, ResponseID: "resp-console-stale"}); !errors.Is(err, ErrResponseNotFound) {
		t.Fatalf("stale console resource error = %v", err)
	}
	if _, err := responseRepo.Get(ctx, "resp-console-stale", key.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stale console ownership was not removed: %v", err)
	}
}

func TestParseFreeQuotaExhaustion(t *testing.T) {
	body := []byte(`{"error":{"code":"subscription:free-usage-exhausted","message":"tokens (actual/limit): 1065387/1000000; Usage resets over a rolling 24-hour window"}}`)
	used, limit, exhausted := parseFreeQuotaExhaustion(body)
	if !exhausted || used != 1_065_387 || limit != 1_000_000 {
		t.Fatalf("parsed = %d/%d, exhausted = %v", used, limit, exhausted)
	}
	if _, _, exhausted := parseFreeQuotaExhaustion([]byte(`{"error":"rate limited"}`)); exhausted {
		t.Fatal("ordinary 429 body must not be treated as Free quota exhaustion")
	}
}

func TestGatewayPreservesRepeatedSystemicForbiddenWithoutCoolingAccounts(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "systemic-forbidden.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	credentials := make([]account.Credential, 0, 3)
	for index, name := range []string{"first", "second", "third"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 300 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-systemic"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-systemic"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "systemic-key", Prefix: "systemic", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	adapter := &systemicForbiddenAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	_, err = service.CreateResponse(ctx, Input{
		RequestID: "req-systemic-403", ClientKey: clientKey, PublicModel: "grok-systemic",
		Body: []byte(`{"model":"grok-systemic","input":"hello"}`),
	})
	var upstreamFailure *UpstreamFailure
	if !errors.As(err, &upstreamFailure) || errors.Is(err, ErrNoAvailableAccount) {
		t.Fatalf("error = %T %v", err, err)
	}
	if upstreamFailure.HTTPStatus != http.StatusForbidden || upstreamFailure.Code != "upstream_forbidden" || upstreamFailure.AccountScoped {
		t.Fatalf("upstream failure = %#v", upstreamFailure)
	}
	attempts := adapter.Attempts()
	if len(attempts) != 2 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID {
		t.Fatalf("attempts = %#v", attempts)
	}
	for _, credential := range credentials {
		observed, getErr := accountRepo.Get(ctx, credential.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if observed.FailureCount != 0 || observed.CooldownUntil != nil || observed.AuthStatus != account.AuthStatusActive {
			t.Fatalf("account %d was incorrectly penalized: %#v", credential.ID, observed)
		}
	}
	logs, total, err := auditRepo.List(ctx, 0, 10)
	if err != nil || total != 1 || logs[0].StatusCode != http.StatusForbidden || logs[0].ErrorCode != "upstream_forbidden" || logs[0].AccountID == nil || *logs[0].AccountID != credentials[1].ID {
		t.Fatalf("audit = %#v, total=%d, err=%v", logs, total, err)
	}
}

func TestGatewayRefreshesAndRetriesBuildPermissionDenialOnce(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auth-rescue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "rescue", SourceKey: "rescue",
		EncryptedAccessToken: "access-old", EncryptedRefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-rescue"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-rescue"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "rescue-key", Prefix: "rescue", SecretHash: strings.Repeat("b", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &authRescueAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-rescue", ClientKey: clientKey, PublicModel: "grok-rescue",
		Body: []byte(`{"model":"grok-rescue","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if string(body) != "ok" || adapter.attempts.Load() != 2 || adapter.refreshes.Load() != 1 {
		t.Fatalf("body=%q attempts=%d refreshes=%d", body, adapter.attempts.Load(), adapter.refreshes.Load())
	}
	updated, err := accountRepo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.EncryptedAccessToken != "access-new" || updated.AuthStatus != account.AuthStatusActive || updated.RefreshFailureCount != 0 {
		t.Fatalf("updated credential = %#v", updated)
	}
	if err := accountRepo.UpdateCredentialRefreshFailure(ctx, credential.ID, 1, updated.ExpiresAt, "invalid_grant", true); err != nil {
		t.Fatal(err)
	}
	adapter.rejectAll.Store(true)
	if _, err := service.CreateResponse(ctx, Input{
		RequestID: "req-rejected", ClientKey: clientKey, PublicModel: "grok-rescue",
		Body: []byte(`{"model":"grok-rescue","input":"hello again"}`),
	}); err == nil {
		t.Fatal("rejected access token unexpectedly succeeded")
	}
	rejected, err := accountRepo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.AuthStatus != account.AuthStatusReauthRequired || adapter.refreshes.Load() != 1 {
		t.Fatalf("rejected credential = %#v, refreshes = %d", rejected, adapter.refreshes.Load())
	}
}

func TestWebRateLimitExhaustsOnlyRequestedQuotaMode(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "web-rate-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "web", SourceKey: "web", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accountRepo.SaveQuotaWindows(ctx, credential.ID, account.WebTierSuper, now, []account.QuotaWindow{
		{AccountID: credential.ID, Mode: "fast", Remaining: 3, Total: 20, WindowSeconds: 3600, Source: account.QuotaSourceUpstream},
		{AccountID: credential.ID, Mode: "auto", Remaining: 4, Total: 10, WindowSeconds: 3600, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-web-test"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-web-test"}, now); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{Name: "key", Prefix: "web-key", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "encrypted-key", Enabled: true, RPMLimit: 60, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	adapter := webRateLimitAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	accountService.SetQuotaRecoveryQueue(memory.NewQuotaRecoveryQueue())
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)
	if _, err := service.CreateResponse(ctx, Input{RequestID: "req-web-429", ClientKey: key, PublicModel: "grok-web-test", Body: []byte(`{"model":"grok-web-test"}`)}); err == nil {
		t.Fatal("expected rate-limited request to fail")
	}
	windows, err := accountRepo.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]int{}
	for _, window := range windows[credential.ID] {
		remaining[window.Mode] = window.Remaining
	}
	if remaining["fast"] != 0 || remaining["auto"] != 4 {
		t.Fatalf("quota remaining = %#v", remaining)
	}
	if _, err := accountRepo.GetQuotaRecovery(ctx, credential.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("Web 429 must not create Build quota recovery state: %v", err)
	}
}

func TestImageStreamPropagatesWithoutTouchingChatQuota(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "web-image", SourceKey: "web-image", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accountRepo.SaveQuotaWindows(ctx, credential.ID, account.WebTierSuper, now, []account.QuotaWindow{{
		AccountID: credential.ID, Mode: "fast", Remaining: 3, Total: 10,
		WindowSeconds: 3600, Source: account.QuotaSourceUpstream, SyncedAt: &now,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{
		{PublicID: "grok-imagine-image-quality", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image-quality", Capability: modeldomain.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image", Capability: modeldomain.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-edit", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image-edit", Capability: modeldomain.CapabilityImageEdit, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-imagine-image-quality", "grok-imagine-image", "grok-imagine-image-edit"}, now); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "image-key", Prefix: "image-key", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "encrypted-key",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &webImageStreamAdapter{synced: make(chan string, 1)}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	runQuotaRefreshWorkers(t, accountService)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)

	result, err := service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-stream", ClientKey: key, PublicModel: "grok-imagine-image-quality",
		Prompt: "test", Count: 2, Resolution: "1k", ResponseFormat: "url", Streaming: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "event: image_generation.completed\ndata: {}\n\ndata: [DONE]\n\n" {
		t.Fatalf("stream body = %q", body)
	}
	if !adapter.Streaming() {
		t.Fatal("stream flag was not propagated to the Web image adapter")
	}
	if logs, total, err := auditRepo.List(ctx, 0, 10); err != nil || total != 0 || len(logs) != 0 {
		t.Fatalf("audit persisted before finalization: logs=%#v total=%d err=%v", logs, total, err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()

	logs, total, err := auditRepo.List(ctx, 0, 10)
	if err != nil || total != 1 || len(logs) != 1 {
		t.Fatalf("audit logs=%#v total=%d err=%v", logs, total, err)
	}
	if !logs[0].Streaming || logs[0].Operation != "image" || logs[0].Provider != string(account.ProviderWeb) || logs[0].ErrorCode != "" ||
		logs[0].MediaInputImages != 0 || logs[0].MediaOutputImages != 2 ||
		logs[0].PricingModel != "grok-imagine-image-quality-1k" || logs[0].EstimatedCostInUSDTicks != 1_000_000_000 {
		t.Fatalf("audit = %#v", logs[0])
	}
	windows, err := accountRepo.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(windows[credential.ID]) != 1 || windows[credential.ID][0].Remaining != 3 {
		t.Fatalf("quota windows = %#v", windows[credential.ID])
	}

	liteResult, err := service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-lite", ClientKey: key, PublicModel: "grok-imagine-image",
		Prompt: "test", Count: 1, ResponseFormat: "url",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(liteResult.Body); err != nil {
		t.Fatal(err)
	}
	liteResult.Finalize(Usage{}, "", "")
	_ = liteResult.Body.Close()
	logs, total, err = auditRepo.List(ctx, 0, 10)
	if err != nil || total != 2 || logs[0].RequestID != "req-image-lite" || logs[0].PricingModel != "grok-imagine-image" || logs[0].EstimatedCostInUSDTicks != 200_000_000 {
		t.Fatalf("Lite image pricing audit = %#v, total=%d, err=%v", logs, total, err)
	}
	select {
	case mode := <-adapter.synced:
		if mode != "fast" {
			t.Fatalf("Lite image synced mode = %q", mode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lite image quota refresh")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		windows, err = accountRepo.GetQuotaWindows(ctx, []uint64{credential.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(windows[credential.ID]) == 1 && windows[credential.ID][0].Remaining == 8 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Lite image quota was not refreshed: %#v", windows[credential.ID])
		}
		time.Sleep(10 * time.Millisecond)
	}

	chatResult, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-image-lite-chat", ClientKey: key, PublicModel: "grok-imagine-image",
		Body: []byte(`{"model":"grok-imagine-image","messages":[{"role":"user","content":"draw"}],"image_config":{"n":3}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(chatResult.Body)
	chatResult.Finalize(Usage{}, "resp-image-lite", "")
	_ = chatResult.Body.Close()
	logs, total, err = auditRepo.List(ctx, 0, 10)
	if err != nil || total != 3 || logs[0].RequestID != "req-image-lite-chat" || logs[0].MediaOutputImages != 3 || logs[0].PricingModel != "grok-imagine-image" || logs[0].EstimatedCostInUSDTicks != 600_000_000 {
		t.Fatalf("Lite Chat pricing audit = %#v, total=%d, err=%v", logs, total, err)
	}

	editResult, err := service.EditImage(ctx, ImageEditInput{
		RequestID: "req-image-edit", ClientKey: key, PublicModel: "grok-imagine-image-edit",
		Prompt: "edit", ImageURLs: []string{"data:image/png;base64,a", "data:image/png;base64,b"},
		Count: 3, Resolution: "2k", ResponseFormat: "url",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(editResult.Body)
	editResult.Finalize(Usage{}, "", "")
	_ = editResult.Body.Close()
	logs, total, err = auditRepo.List(ctx, 0, 10)
	if err != nil || total != 4 || logs[0].RequestID != "req-image-edit" || logs[0].MediaInputImages != 2 || logs[0].MediaOutputImages != 3 || logs[0].PricingModel != "grok-imagine-image-edit-2k" || logs[0].EstimatedCostInUSDTicks != 2_300_000_000 {
		t.Fatalf("image edit pricing audit = %#v, total=%d, err=%v", logs, total, err)
	}
	if adapter.EditResolution() != "2k" {
		t.Fatalf("image edit resolution = %q", adapter.EditResolution())
	}

	billingBeforeFailure, err := keyRepo.Get(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	backupCredential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "web-image-backup", SourceKey: "web-image-backup", EncryptedAccessToken: "encrypted-backup", Enabled: true,
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, backupCredential.ID, []string{"grok-imagine-image-quality"}, now); err != nil {
		t.Fatal(err)
	}
	selector.MarkQuotaStateChanged(account.ProviderWeb)
	service.UpdateMaxAttempts(3)
	attemptsBeforeFailure := len(adapter.Attempts())
	adapter.FailWithEgress(infraegress.NewManager(relational.NewEgressRepository(database), testCipher(t)))
	if _, err := service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-failed", ClientKey: key, PublicModel: "grok-imagine-image-quality",
		Prompt: "test", Count: 1, Resolution: "1k", ResponseFormat: "url",
	}); err == nil {
		t.Fatal("expected image transport failure")
	}
	if attempts := adapter.Attempts(); len(attempts) != attemptsBeforeFailure+1 {
		t.Fatalf("image failure switched accounts after generation started: %#v", attempts)
	}
	logs, total, err = auditRepo.List(ctx, 0, 10)
	if err != nil || total != 5 || len(logs) != 5 {
		t.Fatalf("failure audit logs=%#v total=%d err=%v", logs, total, err)
	}
	failureAudit := logs[0]
	if failureAudit.RequestID != "req-image-failed" || failureAudit.StatusCode != http.StatusBadGateway || failureAudit.ErrorCode != "upstream_unavailable" || failureAudit.MediaOutputImages != 0 || failureAudit.EstimatedCostInUSDTicks != 0 || failureAudit.EgressMode != audit.EgressModeDirect || failureAudit.EgressScope != string(egressdomain.ScopeWeb) || failureAudit.EgressNodeName != "direct" {
		t.Fatalf("failure audit = %#v", failureAudit)
	}
	updatedKey, err := keyRepo.Get(ctx, key.ID)
	if err != nil || updatedKey.ReservedUsageUSDTicks != 0 || updatedKey.BilledUsageUSDTicks != billingBeforeFailure.BilledUsageUSDTicks {
		t.Fatalf("failed image billing key = %#v, err = %v", updatedKey, err)
	}
}

func TestSuccessfulWebChatRefreshesCurrentModeQuota(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "chat-quota-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierBasic,
		Name: "web-chat", SourceKey: "web-chat", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accountRepo.SaveQuotaWindows(ctx, credential.ID, account.WebTierBasic, now, []account.QuotaWindow{{
		AccountID: credential.ID, Mode: "fast", Remaining: 3, Total: 20,
		WindowSeconds: 3600, Source: account.QuotaSourceUpstream, SyncedAt: &now,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: "grok-chat-fast", Provider: account.ProviderWeb, UpstreamModel: "grok-chat-fast",
		Capability: modeldomain.CapabilityChat, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-chat-fast"}, now); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "chat-key", Prefix: "chat-key", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "encrypted-key",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &webChatQuotaAdapter{synced: make(chan string, 1)}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	runQuotaRefreshWorkers(t, accountService)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-chat-quota", ClientKey: key, PublicModel: "grok-chat-fast",
		Body: []byte(`{"model":"grok-chat-fast","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(result.Body); err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()

	select {
	case mode := <-adapter.synced:
		if mode != "fast" {
			t.Fatalf("synced mode = %q", mode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-success quota refresh")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		windows, err := accountRepo.GetQuotaWindows(ctx, []uint64{credential.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(windows[credential.ID]) == 1 && windows[credential.ID][0].Remaining == 17 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("quota windows were not refreshed: %#v", windows[credential.ID])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runQuotaRefreshWorkers(t *testing.T, service *accountapp.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.RunWebQuotaRefresh(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("quota refresh workers did not stop")
		}
	})
}

type failoverAdapter struct {
	mu                 sync.Mutex
	firstID            uint64
	attempts           []uint64
	lastMethod         string
	lastPath           string
	lastPromptCacheKey string
	resourceStatus     int
}

type statelessConsoleAdapter struct{}

func (statelessConsoleAdapter) Provider() account.Provider { return account.ProviderConsole }
func (statelessConsoleAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: account.ProviderConsole,
		Conversation: provider.ConversationSurface{
			Responses: true,
		},
	}
}
func (statelessConsoleAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"id":"resp-console","object":"response","status":"completed"}`)),
	}, nil
}

type systemicForbiddenAdapter struct {
	mu       sync.Mutex
	attempts []uint64
}

type authRescueAdapter struct {
	attempts  atomic.Int64
	refreshes atomic.Int64
	rejectAll atomic.Bool
}

func (a *authRescueAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *authRescueAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderBuild)
}
func (a *authRescueAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.attempts.Add(1)
	if a.rejectAll.Load() {
		return &provider.Response{
			StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unauthorized","message":"access token rejected"}}`)),
		}, nil
	}
	if request.Credential.EncryptedAccessToken == "access-old" {
		return &provider.Response{
			StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"permission_denied","message":"Access to the chat endpoint is denied"}}`)),
		}, nil
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}
func (a *authRescueAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	a.refreshes.Add(1)
	return provider.RefreshedCredential{EncryptedAccessToken: "access-new", EncryptedRefreshToken: "refresh-new", ExpiresAt: time.Now().Add(6 * time.Hour)}, nil
}

func (a *systemicForbiddenAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *systemicForbiddenAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderBuild)
}
func (a *systemicForbiddenAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, request.Credential.ID)
	a.mu.Unlock()
	return &provider.Response{
		StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"error":"upstream policy rejected request"}`)),
	}, nil
}
func (a *systemicForbiddenAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}

type webRateLimitAdapter struct{}

type webImageStreamAdapter struct {
	mu             sync.Mutex
	streaming      bool
	editResolution string
	synced         chan string
	failureEgress  *infraegress.Manager
	attempts       []uint64
}

type webChatQuotaAdapter struct {
	synced chan string
}

type credentialFailureImageAdapter struct {
	generationCalls atomic.Int64
}

func (a *credentialFailureImageAdapter) Provider() account.Provider { return account.ProviderBuild }

func (a *credentialFailureImageAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: account.ProviderBuild, ModelNamespace: account.ProviderBuild.ModelNamespace(),
		Credential: provider.CredentialSurface{AuthType: account.AuthTypeOAuth, Refresh: true},
		Media:      provider.MediaSurface{ImageGeneration: true},
	}
}

func (a *credentialFailureImageAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{}, errors.New("simulated credential refresh failure")
}

func (a *credentialFailureImageAdapter) GenerateImage(context.Context, provider.ImageGenerationRequest) (*provider.Response, error) {
	a.generationCalls.Add(1)
	return nil, errors.New("unexpected image generation")
}

func (webRateLimitAdapter) Provider() account.Provider { return account.ProviderWeb }
func (webRateLimitAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderWeb)
}
func (webRateLimitAdapter) QuotaMode(string) string { return "fast" }
func (webRateLimitAdapter) TierOrder(string) []account.WebTier {
	return []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}
}
func (webRateLimitAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	header := make(http.Header)
	header.Set("Retry-After", "3600")
	return &provider.Response{StatusCode: http.StatusTooManyRequests, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":"limited"}`))}, nil
}

func (a *webImageStreamAdapter) Provider() account.Provider { return account.ProviderWeb }
func (a *webImageStreamAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderWeb)
}
func (a *webImageStreamAdapter) QuotaMode(model string) string {
	if model == "grok-imagine-image" {
		return "fast"
	}
	return ""
}
func (a *webImageStreamAdapter) TierOrder(string) []account.WebTier {
	return []account.WebTier{account.WebTierSuper, account.WebTierHeavy}
}
func (a *webImageStreamAdapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.streaming = request.Streaming
	failureEgress := a.failureEgress
	a.attempts = append(a.attempts, request.Credential.ID)
	a.mu.Unlock()
	if failureEgress != nil {
		lease, err := failureEgress.Acquire(ctx, egressdomain.ScopeWeb, "image-failure")
		if err != nil {
			return nil, err
		}
		lease.Release()
		return nil, errors.New("simulated image transport failure")
	}
	body := "event: image_generation.completed\ndata: {}\n\ndata: [DONE]\n\n"
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), QuotaUnits: 1}, nil
}
func (a *webImageStreamAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"id":"resp-image-lite","object":"response"}`)), QuotaUnits: 3,
	}, nil
}
func (a *webImageStreamAdapter) EditImage(_ context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.editResolution = request.Resolution
	a.mu.Unlock()
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"created":1,"data":[{"url":"https://example.com/edit.png"}]}`)), QuotaUnits: request.Count,
	}, nil
}
func (a *webImageStreamAdapter) Streaming() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streaming
}
func (a *webImageStreamAdapter) EditResolution() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.editResolution
}
func (a *webImageStreamAdapter) FailWithEgress(manager *infraegress.Manager) {
	a.mu.Lock()
	a.failureEgress = manager
	a.mu.Unlock()
}
func (a *webImageStreamAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}
func (a *webImageStreamAdapter) SyncQuota(context.Context, account.Credential) (provider.QuotaSnapshot, error) {
	return provider.QuotaSnapshot{}, errors.New("unexpected full quota sync")
}
func (a *webImageStreamAdapter) SyncQuotaMode(_ context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	now := time.Now().UTC()
	a.synced <- mode
	return account.QuotaWindow{
		AccountID: credential.ID, Mode: mode, Remaining: 8, Total: 10,
		WindowSeconds: 3600, SyncedAt: &now, Source: account.QuotaSourceUpstream,
	}, nil
}

func (a *webChatQuotaAdapter) Provider() account.Provider { return account.ProviderWeb }
func (a *webChatQuotaAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderWeb)
}
func (a *webChatQuotaAdapter) QuotaMode(string) string { return "fast" }
func (a *webChatQuotaAdapter) TierOrder(string) []account.WebTier {
	return []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}
}
func (a *webChatQuotaAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"chat-response"}`))}, nil
}
func (a *webChatQuotaAdapter) SyncQuota(context.Context, account.Credential) (provider.QuotaSnapshot, error) {
	return provider.QuotaSnapshot{}, errors.New("unexpected full quota sync")
}
func (a *webChatQuotaAdapter) SyncQuotaMode(_ context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	now := time.Now().UTC()
	a.synced <- mode
	return account.QuotaWindow{
		AccountID: credential.ID, Mode: mode, Remaining: 17, Total: 20,
		WindowSeconds: 3600, SyncedAt: &now, Source: account.QuotaSourceUpstream,
	}, nil
}

func (a *failoverAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *failoverAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: account.ProviderBuild,
		Conversation: provider.ConversationSurface{
			Responses: true, StoredResponses: true,
		},
	}
}
func (a *failoverAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, request.Credential.ID)
	a.lastMethod = request.Method
	a.lastPath = request.Path
	a.lastPromptCacheKey = request.PromptCacheKey
	resourceStatus := a.resourceStatus
	a.mu.Unlock()
	status, body := http.StatusOK, "ok"
	if request.Method != http.MethodPost && resourceStatus != 0 {
		status, body = resourceStatus, "missing"
	} else if request.Credential.ID == a.firstID {
		status, body = http.StatusTooManyRequests, "limited"
	}
	return &provider.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (a *failoverAdapter) setResourceStatus(status int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resourceStatus = status
}

func (a *failoverAdapter) resetAttempts() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts = nil
	a.lastMethod = ""
	a.lastPath = ""
	a.lastPromptCacheKey = ""
}
func (a *failoverAdapter) ListModels(context.Context, account.Credential) ([]string, error) {
	return nil, nil
}

func testConversationDefinition(providerValue account.Provider) provider.Definition {
	definition := provider.Definition{
		Provider: providerValue,
		Conversation: provider.ConversationSurface{
			Responses: true, ChatCompletions: true, Messages: true,
		},
		Inference: provider.InferencePolicy{Usage: provider.UsageUpstream},
	}
	if providerValue == account.ProviderBuild {
		definition.Credential.Refresh = true
	}
	if providerValue == account.ProviderWeb {
		definition.Quota = provider.QuotaRemoteWindow
		definition.Inference = provider.InferencePolicy{Usage: provider.UsageEstimated, RetryForbiddenAsEgress: true}
	}
	return definition
}
func (a *failoverAdapter) GetBilling(context.Context, account.Credential) (account.Billing, error) {
	return account.Billing{}, nil
}
func (a *failoverAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{}, nil
}
func (a *failoverAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, nil
}
func (a *failoverAdapter) PollDeviceAuthorization(context.Context, string) (provider.CredentialSeed, error) {
	return provider.CredentialSeed{}, nil
}
func (a *failoverAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return nil, nil
}
func (a *failoverAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}

func testCipher(t *testing.T) *security.Cipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}
