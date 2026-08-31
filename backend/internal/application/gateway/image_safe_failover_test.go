package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

type safeFailoverImageAdapter struct {
	mu                sync.Mutex
	attempts          []uint64
	preSubmissionNode map[uint64]bool
	ambiguousNode     map[uint64]bool
	unauthorizedNode  map[uint64]bool
	rateLimitedNode   map[uint64]bool
	quotaKind         provider.QuotaKind
	quotaMode         string
}

func (a *safeFailoverImageAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderWeb
}

func (a *safeFailoverImageAdapter) Definition() provider.Definition {
	quotaKind := a.quotaKind
	if quotaKind == "" {
		quotaKind = provider.QuotaLocalWindow
	}
	return provider.Definition{
		Provider:          accountdomain.ProviderWeb,
		ModelNamespace:    accountdomain.ProviderWeb.ModelNamespace(),
		ModelCatalog:      provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{modeldomain.CapabilityImage},
		Quota:             quotaKind,
		Credential:        provider.CredentialSurface{AuthType: accountdomain.AuthTypeSSO},
		Media:             provider.MediaSurface{ImageGeneration: true},
		Inference:         provider.InferencePolicy{Usage: provider.UsageEstimated},
	}
}

func (a *safeFailoverImageAdapter) QuotaMode(_ string) string {
	return a.quotaMode
}

func (a *safeFailoverImageAdapter) GenerateImage(_ context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, request.Credential.ID)
	preSubmission := a.preSubmissionNode[request.Credential.EgressNodeID]
	ambiguous := a.ambiguousNode[request.Credential.EgressNodeID]
	unauthorized := a.unauthorizedNode[request.Credential.EgressNodeID]
	rateLimited := a.rateLimitedNode[request.Credential.EgressNodeID]
	a.mu.Unlock()
	if preSubmission {
		return nil, provider.NewImagePreSubmissionError(errors.New("bound egress is cooling"))
	}
	if ambiguous {
		return nil, errors.New("connection closed after generation write")
	}
	if unauthorized {
		return nil, provider.ErrUnauthorized
	}
	if rateLimited {
		return &provider.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     http.Header{"Content-Type": {"application/json"}, "Retry-After": {"30"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"limited"}`)),
		}, nil
	}
	return &provider.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"created":1,"data":[{"url":"https://example.com/image.jpg"}]}`)),
		QuotaUnits: 1,
	}, nil
}

func (a *safeFailoverImageAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}

type safeFailoverImageFixture struct {
	service     *Service
	accountRepo *relational.AccountRepository
	auditRepo   *relational.AuditRepository
	adapter     *safeFailoverImageAdapter
	key         clientkey.Key
	credentials []accountdomain.Credential
}

func newSafeFailoverImageFixture(t *testing.T, nodeIDs []uint64, preSubmissionNode, ambiguousNode map[uint64]bool) safeFailoverImageFixture {
	return newSafeFailoverImageFixtureWithQuota(
		t,
		nodeIDs,
		preSubmissionNode,
		ambiguousNode,
		provider.QuotaLocalWindow,
		"",
	)
}

func newSafeFailoverImageFixtureWithQuota(
	t *testing.T,
	nodeIDs []uint64,
	preSubmissionNode, ambiguousNode map[uint64]bool,
	quotaKind provider.QuotaKind,
	quotaMode string,
) safeFailoverImageFixture {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-safe-failover.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	egressRepo := relational.NewEgressRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	credentials := make([]accountdomain.Credential, 0, len(nodeIDs))
	now := time.Now().UTC()
	createdNodes := make(map[uint64]bool)
	for _, nodeID := range nodeIDs {
		if nodeID == 0 || createdNodes[nodeID] {
			continue
		}
		if _, createErr := egressRepo.CreateEgressNode(ctx, egressdomain.Node{
			ID: nodeID, Name: "safe-image-egress-" + string(rune('a'+len(createdNodes))),
			Scope: egressdomain.ScopeWeb, Enabled: true, Health: 1,
		}); createErr != nil {
			t.Fatal(createErr)
		}
		createdNodes[nodeID] = true
	}
	for index, nodeID := range nodeIDs {
		name := "safe-image-" + string(rune('a'+index))
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, WebTier: accountdomain.WebTierSuper,
			Name: name, SourceKey: name, EncryptedAccessToken: "encrypted-" + name,
			Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 300 - index*100,
			MaxConcurrent: 1, EgressNodeID: nodeID,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if credential.EgressNodeID != nodeID {
			t.Fatalf("credential %d egress node = %d, want %d", credential.ID, credential.EgressNodeID, nodeID)
		}
		credentials = append(credentials, credential)
	}
	if quotaMode != "" {
		for _, credential := range credentials {
			if err := accountRepo.SaveQuotaWindows(ctx, credential.ID, credential.WebTier, now, []accountdomain.QuotaWindow{{
				AccountID: credential.ID,
				Mode:      quotaMode,
				Remaining: 4,
				Total:     4,
				Source:    accountdomain.QuotaSourceUpstream,
				SyncedAt:  &now,
				UpdatedAt: now,
			}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	const model = "grok-image-safe-failover"
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: model, Provider: accountdomain.ProviderWeb, UpstreamModel: model,
		Capability: modeldomain.CapabilityImage, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, now); err != nil {
			t.Fatal(err)
		}
	}
	route, err := modelRepo.GetByPublicID(ctx, model)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := accountRepo.ListRoutingCandidates(ctx, accountdomain.ProviderWeb, route.ID, route.UpstreamModel, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != len(credentials) {
		t.Fatalf("routing candidates = %#v, want %d", candidates, len(credentials))
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "safe-image-key", Prefix: "safe-image", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted-key",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &safeFailoverImageAdapter{
		preSubmissionNode: preSubmissionNode,
		ambiguousNode:     ambiguousNode,
		quotaKind:         quotaKind,
		quotaMode:         quotaMode,
	}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, responseRepo, len(credentials)+1)
	return safeFailoverImageFixture{
		service: service, accountRepo: accountRepo, auditRepo: auditRepo, adapter: adapter, key: key, credentials: credentials,
	}
}

func (f safeFailoverImageFixture) generate(ctx context.Context, requestID string) (*Result, error) {
	return f.service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: requestID, ClientKey: f.key, PublicModel: "grok-image-safe-failover",
		Prompt: "test", Count: 1, ResponseFormat: "url",
	})
}

func TestImagePreSubmissionFailureSkipsCredentialsOnSameEgressNode(t *testing.T) {
	ctx := context.Background()
	fixture := newSafeFailoverImageFixture(t, []uint64{11, 11, 12}, map[uint64]bool{11: true}, nil)
	result, err := fixture.generate(ctx, "req-image-safe-failover")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	attempts := fixture.adapter.Attempts()
	if len(attempts) != 2 || attempts[0] != fixture.credentials[0].ID || attempts[1] != fixture.credentials[2].ID {
		t.Fatalf("attempts = %#v, want first credential then different egress node", attempts)
	}
	first, err := fixture.accountRepo.Get(ctx, fixture.credentials[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.FailureCount != 0 || first.CooldownUntil != nil {
		t.Fatalf("egress failure changed credential health: %#v", first)
	}
}

func TestImageAllPreSubmissionFailuresReturnExplicitSentinel(t *testing.T) {
	ctx := context.Background()
	fixture := newSafeFailoverImageFixture(t, []uint64{11, 11, 12}, map[uint64]bool{11: true, 12: true}, nil)
	_, err := fixture.generate(ctx, "req-image-not-submitted")
	if !errors.Is(err, ErrUpstreamNotSubmitted) {
		t.Fatalf("error = %v, want ErrUpstreamNotSubmitted", err)
	}
	if attempts := fixture.adapter.Attempts(); len(attempts) != 2 {
		t.Fatalf("attempts = %#v, want one per egress node", attempts)
	}
	logs, total, listErr := fixture.auditRepo.List(ctx, 0, 10)
	if listErr != nil || total != 1 || len(logs) != 1 {
		t.Fatalf("audit logs=%#v total=%d err=%v", logs, total, listErr)
	}
	if logs[0].StatusCode != http.StatusServiceUnavailable || logs[0].ErrorCode != "upstream_not_submitted" || logs[0].Operation != audit.OperationImage {
		t.Fatalf("audit = %#v", logs[0])
	}
}

func TestImageInitialSelectionFailureReturnsExplicitNotSubmittedSentinel(t *testing.T) {
	ctx := context.Background()
	fixture := newSafeFailoverImageFixture(t, []uint64{11}, nil, nil)
	cooldownUntil := time.Now().UTC().Add(time.Minute)
	credential := fixture.credentials[0]
	if err := fixture.accountRepo.UpdateHealth(
		ctx,
		credential.ID,
		credential.Provider,
		1,
		&cooldownUntil,
		"test selector cooling",
		false,
	); err != nil {
		t.Fatal(err)
	}

	_, err := fixture.generate(ctx, "req-image-selection-not-submitted")
	if !errors.Is(err, ErrUpstreamNotSubmitted) {
		t.Fatalf("selection error = %v, want ErrUpstreamNotSubmitted", err)
	}
	if attempts := fixture.adapter.Attempts(); len(attempts) != 0 {
		t.Fatalf("attempts = %#v, want no provider submission", attempts)
	}
}

func TestImageAmbiguousFailureDoesNotFailOverOrClaimNotSubmitted(t *testing.T) {
	ctx := context.Background()
	fixture := newSafeFailoverImageFixture(t, []uint64{11, 12}, nil, map[uint64]bool{11: true})
	_, err := fixture.generate(ctx, "req-image-ambiguous")
	if err == nil || errors.Is(err, ErrUpstreamNotSubmitted) {
		t.Fatalf("ambiguous error = %v", err)
	}
	if attempts := fixture.adapter.Attempts(); len(attempts) != 1 || attempts[0] != fixture.credentials[0].ID {
		t.Fatalf("ambiguous failure attempts = %#v", attempts)
	}
}

func TestImageRemoteWindow429NeverReplaysOnAnotherIdentity(t *testing.T) {
	ctx := context.Background()
	fixture := newSafeFailoverImageFixtureWithQuota(
		t,
		[]uint64{11, 12},
		nil,
		nil,
		provider.QuotaRemoteWindow,
		accountdomain.QuotaModeWebImagePro,
	)
	fixture.adapter.rateLimitedNode = map[uint64]bool{11: true}

	result, err := fixture.generate(ctx, "req-image-remote-window-429")
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", result.StatusCode)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "upstream_rate_limited")
	_ = result.Body.Close()
	if attempts := fixture.adapter.Attempts(); len(attempts) != 1 || attempts[0] != fixture.credentials[0].ID {
		t.Fatalf("remote-window 429 attempts = %#v, want only the first identity", attempts)
	}
}

func TestImageAmbiguousFailureAfterSafeFailoverDoesNotClaimNotSubmitted(t *testing.T) {
	ctx := context.Background()
	fixture := newSafeFailoverImageFixture(
		t,
		[]uint64{11, 12, 13},
		map[uint64]bool{11: true},
		map[uint64]bool{12: true},
	)
	_, err := fixture.generate(ctx, "req-image-safe-then-ambiguous")
	if err == nil || errors.Is(err, ErrUpstreamNotSubmitted) {
		t.Fatalf("mixed failure error = %v", err)
	}
	if attempts := fixture.adapter.Attempts(); len(attempts) != 2 || attempts[0] != fixture.credentials[0].ID || attempts[1] != fixture.credentials[1].ID {
		t.Fatalf("mixed failure attempts = %#v", attempts)
	}
}

func TestImageCredentialRejectionAfterSafeFailureDoesNotClaimNotSubmitted(t *testing.T) {
	ctx := context.Background()
	fixture := newSafeFailoverImageFixture(
		t,
		[]uint64{11, 12},
		map[uint64]bool{11: true},
		nil,
	)
	fixture.adapter.unauthorizedNode = map[uint64]bool{12: true}
	_, err := fixture.generate(ctx, "req-image-safe-then-unauthorized")
	if err == nil || errors.Is(err, ErrUpstreamNotSubmitted) {
		t.Fatalf("mixed credential error = %v", err)
	}
	if attempts := fixture.adapter.Attempts(); len(attempts) != 2 {
		t.Fatalf("mixed credential attempts = %#v, want two", attempts)
	}
}

func TestImageUnboundCredentialDoesNotRetryUnknownPhysicalEgress(t *testing.T) {
	ctx := context.Background()
	fixture := newSafeFailoverImageFixture(
		t,
		[]uint64{0, 0},
		map[uint64]bool{0: true},
		nil,
	)
	_, err := fixture.generate(ctx, "req-image-unbound-egress")
	if !errors.Is(err, ErrUpstreamNotSubmitted) {
		t.Fatalf("unbound error = %v, want ErrUpstreamNotSubmitted", err)
	}
	if attempts := fixture.adapter.Attempts(); len(attempts) != 1 {
		t.Fatalf("unbound attempts = %#v, want no unsafe retry", attempts)
	}
}
