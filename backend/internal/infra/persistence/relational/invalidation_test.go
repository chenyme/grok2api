package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRoutingMutationsNotifyAfterCommit(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "invalidation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	models := NewModelRepository(database)
	var events []repository.InvalidationEvent
	observe := func(_ context.Context, event repository.InvalidationEvent) {
		events = append(events, event)
	}
	accounts.SetInvalidationObserver(observe)
	models.SetInvalidationObserver(observe)

	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build", SourceKey: "build", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: credential.ID, MonthlyLimit: 100, SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, credential.ID, []string{"model-a"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := models.Create(ctx, model.Route{
		PublicID: "model-a", Provider: account.ProviderBuild, UpstreamModel: "model-a", Capability: model.CapabilityResponses, Enabled: true,
	}, []uint64{credential.ID}); err != nil {
		t.Fatal(err)
	}

	want := []repository.InvalidationKind{
		repository.InvalidationAccountStateChanged,
		repository.InvalidationAccountBillingChanged,
		repository.InvalidationAccountCapabilityChanged,
		repository.InvalidationRouteChanged,
		repository.InvalidationModelBindingChanged,
	}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for index, kind := range want {
		if events[index].Kind != kind || !events[index].Valid() {
			t.Fatalf("event %d = %#v, want %s", index, events[index], kind)
		}
	}
	events = events[:0]
	if err := models.UpsertDiscovered(ctx, account.ProviderBuild, []string{"model-a"}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != repository.InvalidationRouteChanged {
		t.Fatalf("managed route creation events = %#v", events)
	}
	events = events[:0]
	if err := models.UpsertDiscovered(ctx, account.ProviderBuild, []string{"model-a"}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("unchanged discovered routes emitted invalidation: %#v", events)
	}

	before := len(events)
	credential.ID = 999999
	if _, err := accounts.Update(ctx, credential); err == nil {
		t.Fatal("missing account update should fail")
	}
	if len(events) != before {
		t.Fatalf("failed transaction emitted invalidation: %#v", events[before:])
	}
}

func TestAccountHealthMutationPublishesPreciseInvalidation(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "health-invalidation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "health", SourceKey: "health", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []repository.InvalidationEvent
	accounts.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) {
		events = append(events, event)
	})
	cooldownUntil := time.Now().UTC().Add(time.Minute)
	if err := accounts.UpdateHealth(ctx, credential.ID, account.ProviderWeb, 2, &cooldownUntil, "wrong provider", false); err != repository.ErrNotFound {
		t.Fatalf("mismatched provider error = %v, want not found", err)
	}
	if len(events) != 0 {
		t.Fatalf("mismatched provider emitted invalidation: %#v", events)
	}
	if err := accounts.UpdateHealth(ctx, credential.ID, credential.Provider, 2, &cooldownUntil, "upstream status 429", false); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("health events = %#v", events)
	}
	event := events[0]
	if event.Kind != repository.InvalidationAccountHealthChanged || event.Provider != account.ProviderBuild || event.AccountID != credential.ID || event.FailureCount != 2 || event.CooldownUntil == nil || !event.CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("health event = %#v", event)
	}
	if err := accounts.TouchLastUsed(ctx, credential.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("last-used touch emitted routing invalidation: %#v", events[1:])
	}
}

func TestClientKeyMutationsNotifyAfterCommit(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "client-key-invalidation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	keys := NewClientKeyRepository(database)
	created, err := keys.Create(ctx, clientkeydomain.Key{
		Name: "scoped", Prefix: "scoped", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken,
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4, ProviderScope: clientkeydomain.ProviderScopeBuild, TierScope: clientkeydomain.TierScopeFree,
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []repository.InvalidationEvent
	keys.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) {
		events = append(events, event)
	})

	created.ProviderScope = clientkeydomain.ProviderScopeWeb
	created.TierScope = clientkeydomain.TierScopeSuper
	if _, err := keys.Update(ctx, created); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != repository.InvalidationClientKeyChanged || events[0].ClientKeyID != created.ID || !events[0].Valid() {
		t.Fatalf("client-key update events = %#v", events)
	}

	missing := created
	missing.ID = created.ID + 999
	if _, err := keys.Update(ctx, missing); err == nil {
		t.Fatal("missing client-key update should fail")
	}
	if len(events) != 1 {
		t.Fatalf("failed update emitted invalidation: %#v", events[1:])
	}

	if _, err := keys.UpdateManyEnabled(ctx, []uint64{created.ID}, false); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].ClientKeyID != 0 || !events[1].Valid() {
		t.Fatalf("client-key batch events = %#v", events)
	}
	if err := keys.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].ClientKeyID != created.ID || !events[2].Valid() {
		t.Fatalf("client-key delete events = %#v", events)
	}
}

func TestModelUpdateInvalidationUsesStoredRouteIdentity(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-update-invalidation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	models := NewModelRepository(database)
	created, err := models.Create(ctx, model.Route{
		PublicID: "model-a", Provider: account.ProviderBuild, UpstreamModel: "upstream-a", Capability: model.CapabilityResponses, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []repository.InvalidationEvent
	models.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) {
		events = append(events, event)
	})
	created.Provider = account.ProviderWeb
	created.UpstreamModel = "caller-supplied-upstream"
	created.PublicID = "renamed"
	bindings := []uint64{}
	updated, err := models.Update(ctx, created, &bindings)
	if err != nil {
		t.Fatal(err)
	}
	wantPublicID, _ := model.NormalizePublicID(account.ProviderBuild, "renamed")
	if updated.Provider != account.ProviderBuild || updated.UpstreamModel != "upstream-a" || updated.PublicID != wantPublicID {
		t.Fatalf("updated route = %#v", updated)
	}
	if len(events) != 2 {
		t.Fatalf("events = %#v", events)
	}
	for _, event := range events {
		if event.Provider != account.ProviderBuild || event.UpstreamModel != "upstream-a" {
			t.Fatalf("invalidation used caller identity: %#v", event)
		}
	}
	events = events[:0]
	updatedCount, err := models.UpdateManyEnabled(ctx, []uint64{created.ID}, true)
	if err != nil {
		t.Fatal(err)
	}
	if updatedCount != 0 || len(events) != 0 {
		t.Fatalf("unchanged enabled state emitted invalidation: %#v", events)
	}
	updatedCount, err = models.UpdateManyEnabled(ctx, []uint64{created.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if updatedCount != 1 || len(events) != 1 || events[0].Provider != account.ProviderBuild {
		t.Fatalf("changed enabled state events = %#v", events)
	}
}

func TestAccountUpdatePreservesStoredProvider(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "account-update-provider.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	created, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build", SourceKey: "build", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	created.Provider = account.ProviderWeb
	created.BuildSuperEntitled = true
	created.BuildRouteMode = account.BuildRouteBuild
	updated, err := accounts.Update(ctx, created)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Provider != account.ProviderBuild || !updated.BuildSuperEntitled || updated.BuildRouteMode != account.BuildRouteBuild {
		t.Fatalf("updated account = %#v", updated)
	}
}
