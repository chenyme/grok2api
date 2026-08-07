package account

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func openAccountService(t *testing.T) (*Service, *relational.AccountRepository) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "build-super.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	audits := relational.NewAuditRepository(database)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(accounts, audits, nil, nil, nil, cipher, nil)
	return service, accounts
}

type credentialMetadataAdapterStub struct {
	calls *atomic.Int32
}

func (credentialMetadataAdapterStub) Provider() accountdomain.Provider {
	return accountdomain.ProviderBuild
}

func (s credentialMetadataAdapterStub) CredentialMetadata(credential accountdomain.Credential) provider.CredentialMetadata {
	if s.calls != nil {
		s.calls.Add(1)
	}
	if credential.ID == 1 {
		return provider.CredentialMetadata{BuildBotFlagInspected: true, BuildBotFlagged: true, BuildBotFlagSource: 1}
	}
	return provider.CredentialMetadata{BuildBotFlagInspected: true}
}

func TestBuildBotFlagIndexIsRebuiltOnceAndReadWithoutTokenInspection(t *testing.T) {
	ctx := context.Background()
	service, accounts := openAccountService(t)
	var calls atomic.Int32
	service.providers = provider.NewRegistry(credentialMetadataAdapterStub{calls: &calls})
	if _, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "flagged", SourceKey: "cached-build-bot-flag",
		EncryptedAccessToken: "enc", AuthStatus: accountdomain.AuthStatusActive, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	if err := service.RebuildBuildBotFlagIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("metadata inspections during rebuild = %d, want 1", got)
	}

	if _, err := service.buildBotFlaggedAccountIDs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.buildBotFlaggedAccountIDs(ctx); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("metadata inspections after indexed reads = %d, want 1", got)
	}
	bases, err := accounts.ListRoutingAccountBases(ctx, accountdomain.ProviderBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bases) != 1 || bases[0].Credential.BuildBotFlagSource != 1 || bases[0].Credential.EncryptedAccessToken != "" {
		t.Fatalf("routing projection = %#v", bases)
	}
}

type unknownCredentialMetadataAdapterStub struct{}

func (unknownCredentialMetadataAdapterStub) Provider() accountdomain.Provider {
	return accountdomain.ProviderBuild
}

func (unknownCredentialMetadataAdapterStub) CredentialMetadata(accountdomain.Credential) provider.CredentialMetadata {
	return provider.CredentialMetadata{}
}

func TestBuildBotFlagIndexPreservesStoredSourceWhenInspectionFails(t *testing.T) {
	ctx := context.Background()
	service, accounts := openAccountService(t)
	service.providers = provider.NewRegistry(unknownCredentialMetadataAdapterStub{})
	created, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "unknown", SourceKey: "unknown-build-bot-flag",
		EncryptedAccessToken: "invalid", AuthStatus: accountdomain.AuthStatusActive, Enabled: true,
		BuildBotFlagSource: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RebuildBuildBotFlagIndex(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := accounts.Get(ctx, created.ID)
	if err != nil || stored.BuildBotFlagSource != 2 {
		t.Fatalf("stored credential = %#v, err=%v", stored, err)
	}
	view, err := service.Get(ctx, created.ID)
	if err != nil || !view.BuildBotFlagged || view.BuildBotFlagSource != 2 {
		t.Fatalf("view = %#v, err=%v", view, err)
	}
}

func TestAccountViewsIncludeBuildBotFlagMetadata(t *testing.T) {
	ctx := context.Background()
	service, accounts := openAccountService(t)
	service.providers = provider.NewRegistry(credentialMetadataAdapterStub{})
	build, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "flagged", SourceKey: "build-bot-flag",
		EncryptedAccessToken: "enc", AuthStatus: accountdomain.AuthStatusActive, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RebuildBuildBotFlagIndex(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := service.Get(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.BuildBotFlagged {
		t.Fatal("single account view did not include bot flag metadata")
	}
	views, _, err := service.List(ctx, 1, 20, "", ListFilter{Provider: string(accountdomain.ProviderBuild)})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || !views[0].BuildBotFlagged {
		t.Fatalf("list views = %#v", views)
	}
	normal, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "normal", SourceKey: "build-normal",
		EncryptedAccessToken: "enc", AuthStatus: accountdomain.AuthStatusActive, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	views, total, err := service.List(ctx, 1, 20, "", ListFilter{Provider: string(accountdomain.ProviderBuild), Risk: "flagged"})
	if err != nil || total != 1 || len(views) != 1 || views[0].Credential.ID != build.ID {
		t.Fatalf("flagged views=%#v total=%d err=%v", views, total, err)
	}
	views, total, err = service.List(ctx, 1, 20, "", ListFilter{Provider: string(accountdomain.ProviderBuild), Risk: "normal"})
	if err != nil || total != 1 || len(views) != 1 || views[0].Credential.ID != normal.ID {
		t.Fatalf("normal views=%#v total=%d err=%v", views, total, err)
	}
	if _, _, err := service.List(ctx, 1, 20, "", ListFilter{Provider: string(accountdomain.ProviderWeb), Risk: "flagged"}); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("non-Build risk filter err = %v", err)
	}
	summary, err := service.Summary(ctx)
	if err != nil || summary.Risk != 1 {
		t.Fatalf("summary=%#v err=%v", summary, err)
	}
}

func TestSummaryUsesOneAvailabilitySnapshotWhenExcludingBotRisk(t *testing.T) {
	ctx := context.Background()
	service, accounts := openAccountService(t)
	initial := time.Now().UTC().Truncate(time.Second)
	cooldownUntil := initial.Add(time.Minute)
	if _, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "flagged-cooldown", SourceKey: "flagged-cooldown",
		EncryptedAccessToken: "enc", AuthStatus: accountdomain.AuthStatusActive,
		BuildBotFlagSource: 1, CooldownUntil: &cooldownUntil,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "normal-active", SourceKey: "normal-active",
		EncryptedAccessToken: "enc", AuthStatus: accountdomain.AuthStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	service.UpdateExcludeBuildBotFlaggedFromScheduling(true)
	var calls atomic.Int32
	service.now = func() time.Time {
		if calls.Add(1) == 1 {
			return initial
		}
		return initial.Add(2 * time.Minute)
	}

	summary, err := service.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	build := summary.Providers[string(accountdomain.ProviderBuild)]
	if summary.Risk != 1 || build.Available != 1 || summary.Available != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestUpdateBuildSuperEntitledBuildOnly(t *testing.T) {
	ctx := context.Background()
	service, accounts := openAccountService(t)
	build, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "build", SourceKey: "build-super-patch",
		EncryptedAccessToken: "enc", AuthStatus: accountdomain.AuthStatusActive, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	web, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web-super-patch",
		EncryptedAccessToken: "enc", AuthStatus: accountdomain.AuthStatusActive, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	trueVal := true
	view, err := service.Update(ctx, build.ID, UpdateInput{BuildSuperEntitled: &trueVal})
	if err != nil {
		t.Fatal(err)
	}
	if !view.Credential.BuildSuperEntitled {
		t.Fatalf("build entitlement not set: %#v", view.Credential)
	}
	if view.Quota.Type != QuotaTypePaid || view.Quota.Source != "buildSuperEntitlement" || view.Quota.Confidence != "confirmed" {
		t.Fatalf("quota = %#v", view.Quota)
	}
	// 零 Billing 数值保持未知/零。
	if view.Quota.LimitKnown || view.Quota.Used != 0 || view.Quota.Limit != 0 {
		t.Fatalf("must not fabricate limits: %#v", view.Quota)
	}
	if _, err := service.Update(ctx, web.ID, UpdateInput{BuildSuperEntitled: &trueVal}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("web update err = %v", err)
	}
	xaiMode := accountdomain.BuildRouteXAI
	view, err = service.Update(ctx, build.ID, UpdateInput{BuildRouteMode: &xaiMode})
	if err != nil || view.Credential.BuildRouteMode != accountdomain.BuildRouteXAI {
		t.Fatalf("route update view=%#v err=%v", view.Credential, err)
	}
	if _, err := service.Update(ctx, web.ID, UpdateInput{BuildRouteMode: &xaiMode}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("web route update err = %v", err)
	}
	invalidMode := accountdomain.BuildRouteMode("invalid")
	if _, err := service.Update(ctx, build.ID, UpdateInput{BuildRouteMode: &invalidMode}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid route update err = %v", err)
	}
	// 清除 entitlement
	falseVal := false
	view, err = service.Update(ctx, build.ID, UpdateInput{BuildSuperEntitled: &falseVal})
	if err != nil {
		t.Fatal(err)
	}
	if view.Credential.BuildSuperEntitled || view.Quota.Type != QuotaTypeUnknown {
		t.Fatalf("cleared view = %#v quota=%#v", view.Credential, view.Quota)
	}
}
