package imagecapacity

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/buildinfo"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

var testBuildAttestation = buildinfo.Attestation{
	SourceCommit:       "0123456789abcdef0123456789abcdef01234567",
	RuntimeImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	BuildFingerprint:   "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
}

type serviceFixture struct {
	ctx        context.Context
	now        time.Time
	database   *relational.Database
	accounts   *relational.AccountRepository
	models     *relational.ModelRepository
	audits     *relational.AuditRepository
	keys       *relational.ClientKeyRepository
	quotaModes staticQuotaModes
	routing    *staticRoutingPolicy
	service    *Service
	route      modeldomain.Route
	key        clientkeydomain.Key
	eligible   account.Credential
	exhausted  account.Credential
	cooling    account.Credential
	blocked    account.Credential
	policyOut  account.Credential
}

type staticQuotaModes map[string]string

type staticRoutingPolicy struct{ maxAttempts int }

func (p *staticRoutingPolicy) ImageCapacityMaxAttempts() int { return p.maxAttempts }

func (m staticQuotaModes) QuotaMode(_ account.Provider, upstreamModel string) string {
	return m[upstreamModel]
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-capacity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	egressNodes := relational.NewEgressRepository(database)
	egressNode, err := egressNodes.CreateEgressNode(ctx, egressdomain.Node{
		Name: "capacity-egress", Scope: egressdomain.ScopeWeb, Enabled: true, EncryptedProxyURL: "encrypted",
	})
	if err != nil {
		t.Fatal(err)
	}
	models := relational.NewModelRepository(database)
	audits := relational.NewAuditRepository(database)
	keys := relational.NewClientKeyRepository(database)
	now := time.Now().UTC().Truncate(time.Second)
	createAccount := func(name string, tier account.WebTier, cohort string, remaining int, cooldown *time.Time) account.Credential {
		value, _, createErr := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: tier,
			Name: name, SourceKey: name, EncryptedAccessToken: "encrypted-" + name,
			Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
			RoutingCohort: cohort, CooldownUntil: cooldown, EgressNodeID: egressNode.ID,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if err := accounts.SaveQuotaWindows(ctx, value.ID, tier, now, []account.QuotaWindow{{
			AccountID: value.ID, Mode: account.QuotaModeWebImagePro, Remaining: remaining, Total: 4,
			SyncedAt: &now, Source: account.QuotaSourceUpstream,
		}}); err != nil {
			t.Fatal(err)
		}
		return value
	}
	cooldown := now.Add(time.Hour)
	eligible := createAccount("eligible-secret-name", account.WebTierSuper, "stress", 4, nil)
	exhausted := createAccount("exhausted-secret-name", account.WebTierSuper, "stress", 0, nil)
	cooling := createAccount("cooling-secret-name", account.WebTierSuper, "stress", 4, &cooldown)
	blocked := createAccount("blocked-secret-name", account.WebTierSuper, "stress", 4, nil)
	policyOut := createAccount("policy-secret-name", account.WebTierBasic, "stress", 4, nil)
	route, err := models.Create(ctx, modeldomain.Route{
		PublicID: "grok-imagine-image-2.0", Provider: account.ProviderWeb,
		UpstreamModel: "grok-imagine-image-2.0", Capability: modeldomain.CapabilityImage, Enabled: true,
	}, []uint64{eligible.ID, exhausted.ID, cooling.ID, blocked.ID, policyOut.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{
		AccountID: blocked.ID, UpstreamModel: route.UpstreamModel, Reason: "test", CooldownUntil: cooldown,
	}); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	rawKey := security.FormatClientKey("capacity", "0123456789abcdef0123456789abcdef")
	encryptedKey, err := cipher.Encrypt(rawKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := keys.Create(ctx, clientkeydomain.Key{
		Name: "capacity-secret-key-name", Prefix: "capacity", SecretHash: security.HashToken(rawKey), EncryptedSecret: encryptedKey,
		Enabled: true, AllowedModels: []uint64{route.ID}, ProviderScope: clientkeydomain.ProviderScopeWeb,
		TierScope: clientkeydomain.TierScopeSuper, RoutingCohort: "stress",
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := gateway.NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	quotaModes := staticQuotaModes{route.UpstreamModel: account.QuotaModeWebImagePro, "grok-imagine-image": "fast"}
	routing := &staticRoutingPolicy{maxAttempts: 999}
	service := NewService(clientkeyapp.NewService(keys, nil, nil, 60, 4, cipher), modelapp.NewService(models, accounts, nil, nil), quotaModes, selector, routing, audits)
	service.now = func() time.Time { return now }
	service.build = func() (buildinfo.Attestation, error) { return testBuildAttestation, nil }
	return &serviceFixture{
		ctx: ctx, now: now, database: database, accounts: accounts, models: models, audits: audits, keys: keys, quotaModes: quotaModes, routing: routing, service: service,
		route: route, key: key, eligible: eligible, exhausted: exhausted, cooling: cooling, blocked: blocked, policyOut: policyOut,
	}
}

func TestAttestationRefusesNonImageProRouteFromCanonicalResolver(t *testing.T) {
	fixture := newServiceFixture(t)
	lite, err := fixture.models.Create(fixture.ctx, modeldomain.Route{
		PublicID: "grok-imagine-image-lite", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image",
		Capability: modeldomain.CapabilityImage, Enabled: true,
	}, []uint64{fixture.eligible.ID})
	if err != nil {
		t.Fatal(err)
	}
	fixture.key.AllowedModels = append(fixture.key.AllowedModels, lite.ID)
	updated, err := fixture.keys.Update(fixture.ctx, fixture.key)
	if err != nil {
		t.Fatal(err)
	}
	fixture.key = updated
	if _, err := fixture.service.Attest(fixture.ctx, Request{ClientKeyID: fixture.key.ID, RouteID: lite.ID}); !errors.Is(err, ErrRouteNotAttestable) {
		t.Fatalf("non-image_pro route error = %v", err)
	}
}

func TestAttestationUsesCanonicalExplicitRouteAndClientPolicy(t *testing.T) {
	fixture := newServiceFixture(t)
	value, err := fixture.service.Attest(fixture.ctx, Request{ClientKeyID: fixture.key.ID, RouteID: fixture.route.ID})
	if err != nil {
		t.Fatal(err)
	}
	if value.SchemaVersion != SchemaVersion || value.Build != testBuildAttestation || value.ClientKeyFingerprint == "" {
		t.Fatalf("attestation identity = %#v", value)
	}
	if !value.Route.BindingMode || value.Route.ID == "" || value.Route.TopologySHA256 == "" {
		t.Fatalf("route attestation = %#v", value.Route)
	}
	if value.EligibleImageIdentityCount != 1 || value.EligibleImageIdentitySetSHA256 != identitySetSHA256([]uint64{fixture.eligible.ID}) {
		t.Fatalf("eligible capacity = %#v", value)
	}
	if value.Routing.RetryPolicy != RetryPolicy || value.Routing.MaxAttempts != 999 || value.Routing.EligibleEgressCount != 1 || value.Routing.FairnessPolicy != account.ImageProFairnessPolicy {
		t.Fatalf("routing attestation = %#v", value.Routing)
	}
}

func TestAttestationRefusesRoutingBudgetBelowEligibleEgressCount(t *testing.T) {
	fixture := newServiceFixture(t)
	secondNode, err := relational.NewEgressRepository(fixture.database).CreateEgressNode(fixture.ctx, egressdomain.Node{
		Name: "capacity-egress-two", Scope: egressdomain.ScopeWeb, Enabled: true, EncryptedProxyURL: "encrypted-two",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := fixture.accounts.UpsertByIdentity(fixture.ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "eligible-second", SourceKey: "eligible-second", EncryptedAccessToken: "encrypted-second",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
		RoutingCohort: "stress", EgressNodeID: secondNode.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.accounts.SaveQuotaWindows(fixture.ctx, second.ID, account.WebTierSuper, fixture.now, []account.QuotaWindow{{
		AccountID: second.ID, Mode: account.QuotaModeWebImagePro, Remaining: 4, Total: 4,
		SyncedAt: &fixture.now, Source: account.QuotaSourceUpstream,
	}}); err != nil {
		t.Fatal(err)
	}
	fixture.route.BoundAccountIDs = append(fixture.route.BoundAccountIDs, second.ID)
	if _, err := fixture.models.Update(fixture.ctx, fixture.route, &fixture.route.BoundAccountIDs); err != nil {
		t.Fatal(err)
	}
	fixture.routing.maxAttempts = 1
	if _, err := fixture.service.Attest(fixture.ctx, Request{ClientKeyID: fixture.key.ID, RouteID: fixture.route.ID}); !errors.Is(err, ErrRouteNotAttestable) {
		t.Fatalf("insufficient retry budget error = %v", err)
	}
	fixture.routing.maxAttempts = -1
	value, err := fixture.service.Attest(fixture.ctx, Request{ClientKeyID: fixture.key.ID, RouteID: fixture.route.ID})
	if err != nil {
		t.Fatal(err)
	}
	if value.Routing.MaxAttempts != -1 || value.Routing.EligibleEgressCount != 2 {
		t.Fatalf("unlimited routing attestation = %#v", value.Routing)
	}
}

func TestIdentitySetHashContract(t *testing.T) {
	const expected = "08f57490acc94f09309db1580d3e6edc970aec4adb6a7ea99d2745dd4ae2062e"
	if value := identitySetSHA256([]uint64{10, 2, 2, 0}); value != expected {
		t.Fatalf("identity hash = %q", value)
	}
	if eligible, successful := identitySetSHA256([]uint64{2, 10}), identitySetSHA256([]uint64{10, 2}); eligible != successful || eligible != expected {
		t.Fatalf("eligible/successful hash mismatch: %q != %q", eligible, successful)
	}
	if empty := identitySetSHA256(nil); empty != "76389bbb10fae9d0372f236af924c40eed4bf676031d71061538e6323a768aad" {
		t.Fatalf("empty identity hash = %q", empty)
	}
}

func TestRouteTopologyHashContract(t *testing.T) {
	route := modeldomain.Route{
		ID: 7, PublicID: "grok-image", Provider: account.ProviderWeb, UpstreamModel: "grok-image-upstream",
		Capability: modeldomain.CapabilityImage, BoundAccountIDs: []uint64{10, 2, 2, 0},
	}
	const expected = "f027067a6644c0f1096868aa127eb773071e32fd78b9f3b1e562e45e5e515c18"
	if value := routeTopologySHA256(route); value != expected {
		t.Fatalf("route topology hash = %q", value)
	}
	attestation := routeAttestation(route)
	if attestation.ID != "7" || attestation.PublicID != "grok-image" || attestation.UpstreamModel != "grok-image-upstream" || attestation.Capability != "image" || !attestation.BindingMode || attestation.TopologySHA256 != expected {
		t.Fatalf("route attestation = %#v", attestation)
	}
}

func TestAttestationRefusesWildcardRouteAndMissingBuild(t *testing.T) {
	fixture := newServiceFixture(t)
	wildcard, err := fixture.models.Create(fixture.ctx, modeldomain.Route{
		PublicID: "wildcard-image", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image-2.0",
		Capability: modeldomain.CapabilityImage, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Attest(fixture.ctx, Request{ClientKeyID: fixture.key.ID, RouteID: wildcard.ID}); !errors.Is(err, ErrRouteNotAttestable) {
		t.Fatalf("wildcard error = %v", err)
	}
	fixture.service.build = func() (buildinfo.Attestation, error) { return buildinfo.Attestation{}, errors.New("missing") }
	if _, err := fixture.service.Attest(fixture.ctx, Request{ClientKeyID: fixture.key.ID, RouteID: fixture.route.ID}); !errors.Is(err, ErrBuildNotAttestable) {
		t.Fatalf("build error = %v", err)
	}
}

func TestAttestationCoverageExcludesOtherKeysRoutesOperationsFailuresAndMarkers(t *testing.T) {
	fixture := newServiceFixture(t)
	since := fixture.now.Add(-time.Minute)
	// Cross-service CheapAIAPI vector: UUID4 550e8400-e29b-41d4-a716-446655440000.
	const runMarker = "3b36292091cb9b4d9b27cc37"
	otherKey := fixture.key.ID + 100
	otherRoute := fixture.route.ID + 100
	accountID := fixture.eligible.ID
	errorRecord := coverageRecord("evt_capacity_error_code_07", runMarker+":00000000-0000-4000-8000-000000000007", fixture.key.ID, fixture.route.ID, accountID, audit.OperationImage, 200, 1, fixture.now)
	errorRecord.ErrorCode = "upstream_error"
	records := []audit.Record{
		coverageRecord("evt_capacity_success_0001", runMarker+":00000000-0000-4000-8000-000000000001", fixture.key.ID, fixture.route.ID, accountID, audit.OperationImage, 200, 1, fixture.now),
		coverageRecord("evt_capacity_success_0002", runMarker+":00000000-0000-4000-8000-000000000002", fixture.key.ID, fixture.route.ID, accountID, audit.OperationImage, 201, 2, fixture.now),
		coverageRecord("evt_capacity_failure_0003", runMarker+":00000000-0000-4000-8000-000000000003", fixture.key.ID, fixture.route.ID, accountID, audit.OperationImage, 500, 0, fixture.now),
		coverageRecord("evt_capacity_other_key_004", runMarker+":00000000-0000-4000-8000-000000000004", otherKey, fixture.route.ID, accountID, audit.OperationImage, 200, 1, fixture.now),
		coverageRecord("evt_capacity_other_route_5", runMarker+":00000000-0000-4000-8000-000000000005", fixture.key.ID, otherRoute, accountID, audit.OperationImage, 200, 1, fixture.now),
		coverageRecord("evt_capacity_image_edit_006", runMarker+":00000000-0000-4000-8000-000000000006", fixture.key.ID, fixture.route.ID, accountID, audit.OperationImageEdit, 200, 1, fixture.now),
		errorRecord,
		coverageRecord("evt_capacity_before_since_8", runMarker+":00000000-0000-4000-8000-000000000008", fixture.key.ID, fixture.route.ID, accountID, audit.OperationImage, 200, 1, since.Add(-time.Second)),
		coverageRecord("evt_capacity_other_marker_9", "ffffffffffffffffffffffff:00000000-0000-4000-8000-000000000009", fixture.key.ID, fixture.route.ID, accountID, audit.OperationImage, 200, 1, fixture.now),
		coverageRecord("evt_capacity_after_observed_10", runMarker+":00000000-0000-4000-8000-000000000010", fixture.key.ID, fixture.route.ID, accountID, audit.OperationImage, 200, 1, fixture.now.Add(time.Second)),
	}
	for _, record := range records {
		if err := fixture.audits.Create(fixture.ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	value, err := fixture.service.Attest(fixture.ctx, Request{ClientKeyID: fixture.key.ID, RouteID: fixture.route.ID, Since: &since, RunMarker: runMarker})
	if err != nil {
		t.Fatal(err)
	}
	if value.Coverage == nil || value.Coverage.Operation != CoverageOperation || value.Coverage.SelectedSuccessfulIdentityCount != 1 || value.Coverage.TerminalSuccessCount != 2 {
		t.Fatalf("coverage = %#v", value.Coverage)
	}
	if value.Coverage.SelectedSuccessfulIdentitySetSHA256 != identitySetSHA256([]uint64{accountID}) {
		t.Fatalf("coverage hash = %q", value.Coverage.SelectedSuccessfulIdentitySetSHA256)
	}
	if value.Coverage.SelectedSuccessfulIdentitySetSHA256 != value.EligibleImageIdentitySetSHA256 {
		t.Fatalf("same identity set used different hash contracts: %q != %q", value.Coverage.SelectedSuccessfulIdentitySetSHA256, value.EligibleImageIdentitySetSHA256)
	}
}

func TestAttestationRejectsWeakRunMarkers(t *testing.T) {
	fixture := newServiceFixture(t)
	since := fixture.now.Add(-time.Minute)
	for _, marker := range []string{"stressrun", "A1b2c3d4e5f60718293a4b5c", " a1b2c3d4e5f60718293a4b5c"} {
		if _, err := fixture.service.Attest(fixture.ctx, Request{ClientKeyID: fixture.key.ID, RouteID: fixture.route.ID, Since: &since, RunMarker: marker}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("marker %q error = %v", marker, err)
		}
	}
}

func coverageRecord(eventID, requestID string, clientKeyID, routeID, accountID uint64, operation audit.Operation, status, outputs int, createdAt time.Time) audit.Record {
	return audit.Record{
		EventID: eventID, RequestID: requestID, ClientKeyID: clientKeyID, ModelRouteID: routeID,
		Provider: string(account.ProviderWeb), Operation: operation, UsageSource: audit.UsageSourceNone,
		AccountID: &accountID, StatusCode: status, MediaOutputImages: int64(outputs), CreatedAt: createdAt,
	}
}
