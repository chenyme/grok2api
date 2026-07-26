package account

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestCleanupAccountsDeletesOnlySelectedCurrentStatuses(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "account-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("cleanup-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	service := NewService(repo, nil, nil, memory.NewStickyStore(), nil, cipher, nil)
	service.now = func() time.Time { return now }

	create := func(name string, providerValue accountdomain.Provider, mutate func(*accountdomain.Credential)) uint64 {
		t.Helper()
		value, _, createErr := repo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: providerValue, AuthType: accountdomain.AuthTypeSSO, Name: name, SourceKey: fmt.Sprintf("cleanup-%s", name),
			EncryptedAccessToken: token, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if mutate != nil {
			mutate(&value)
			value, createErr = repo.Update(ctx, value)
			if createErr != nil {
				t.Fatal(createErr)
			}
		}
		return value.ID
	}

	normalID := create("normal", accountdomain.ProviderBuild, nil)
	disabledID := create("disabled", accountdomain.ProviderBuild, func(value *accountdomain.Credential) { value.Enabled = false })
	invalidID := create("invalid", accountdomain.ProviderBuild, func(value *accountdomain.Credential) { value.AuthStatus = accountdomain.AuthStatusReauthRequired })
	coolingID := create("cooling", accountdomain.ProviderBuild, func(value *accountdomain.Credential) { until := now.Add(time.Hour); value.CooldownUntil = &until })
	expiredCooldownID := create("expired-cooldown", accountdomain.ProviderBuild, func(value *accountdomain.Credential) { until := now.Add(-time.Hour); value.CooldownUntil = &until })
	otherProviderID := create("web-disabled", accountdomain.ProviderWeb, func(value *accountdomain.Credential) { value.Enabled = false })

	result, err := service.CleanupAccounts(ctx, accountdomain.ProviderBuild, []CleanupStatus{CleanupStatusDisabled, CleanupStatusReauthRequired, CleanupStatusCooldown, CleanupStatusDisabled}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 3 || result.RootsDeleted != 3 || result.LinkedDeleted != 0 || result.Skipped != 0 {
		t.Fatalf("result = %#v", result)
	}
	for _, id := range []uint64{disabledID, invalidID, coolingID} {
		if _, err := repo.Get(ctx, id); err == nil {
			t.Fatalf("account %d was not deleted", id)
		}
	}
	for _, id := range []uint64{normalID, expiredCooldownID, otherProviderID} {
		if _, err := repo.Get(ctx, id); err != nil {
			t.Fatalf("account %d should remain: %v", id, err)
		}
	}
}

func TestCleanupAccountsRequiresStatus(t *testing.T) {
	service := NewService(nil, nil, nil, nil, nil, nil, nil)
	if _, err := service.CleanupAccounts(context.Background(), accountdomain.ProviderBuild, nil, nil); err == nil {
		t.Fatal("empty cleanup status unexpectedly succeeded")
	}
	// Linked targets cannot include the root provider or an invalid provider.
	if _, err := service.CleanupAccounts(context.Background(), accountdomain.ProviderBuild, []CleanupStatus{CleanupStatusDisabled}, []accountdomain.Provider{accountdomain.ProviderBuild}); err == nil {
		t.Fatal("self-target cleanup unexpectedly succeeded")
	}
	if _, err := service.CleanupAccounts(context.Background(), accountdomain.ProviderBuild, []CleanupStatus{CleanupStatusDisabled}, []accountdomain.Provider{accountdomain.Provider("nope")}); err == nil {
		t.Fatal("invalid target cleanup unexpectedly succeeded")
	}
}

// Cleanup with linked targets removes peers regardless of peer state and reports exact counts.
func TestCleanupAccountsWithLinkedTargets(t *testing.T) {
	ctx := context.Background()
	repo, service := newLinkedDeleteTestService(t, "svc-cleanup-linked.db")
	now := time.Now().UTC()
	service.now = func() time.Time { return now }

	web, build, console := seedLinkedTrio(t, repo, strings.Repeat("7", 64), "u-cleanup")
	// Mark the Web root invalid while keeping active peers to verify peer state is ignored.
	web.AuthStatus = accountdomain.AuthStatusReauthRequired
	if _, err := repo.Update(ctx, web); err != nil {
		t.Fatal(err)
	}
	healthyWeb := mustUpsertLinked(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "healthy", SourceKey: "sso:" + strings.Repeat("8", 64),
	})

	result, err := service.CleanupAccounts(ctx, accountdomain.ProviderWeb, []CleanupStatus{CleanupStatusReauthRequired}, []accountdomain.Provider{accountdomain.ProviderBuild, accountdomain.ProviderConsole})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 3 || result.RootsDeleted != 1 || result.LinkedDeleted != 2 || result.Skipped != 0 {
		t.Fatalf("result = %#v", result)
	}
	assertAccountMissing(t, repo, web.ID)
	assertAccountMissing(t, repo, build.ID)
	assertAccountMissing(t, repo, console.ID)
	assertAccountPresent(t, repo, healthyWeb.ID)
}

// Cleanup preview counts roots and linked targets without deleting rows.
func TestPreviewCleanupCountsWithoutDeleting(t *testing.T) {
	ctx := context.Background()
	repo, service := newLinkedDeleteTestService(t, "svc-cleanup-preview.db")
	now := time.Now().UTC()
	service.now = func() time.Time { return now }

	web, build, console := seedLinkedTrio(t, repo, strings.Repeat("9", 64), "u-preview")
	web.Enabled = false
	if _, err := repo.Update(ctx, web); err != nil {
		t.Fatal(err)
	}

	preview, err := service.PreviewCleanup(ctx, accountdomain.ProviderWeb, []CleanupStatus{CleanupStatusDisabled, CleanupStatusCooldown}, []accountdomain.Provider{accountdomain.ProviderBuild, accountdomain.ProviderConsole})
	if err != nil {
		t.Fatal(err)
	}
	if preview.RootsByStatus["disabled"] != 1 || preview.RootsByStatus["cooldown"] != 0 || preview.RootCount != 1 || preview.Total != 3 {
		t.Fatalf("preview = %#v", preview)
	}
	assertAccountPresent(t, repo, web.ID)
	assertAccountPresent(t, repo, build.ID)
	assertAccountPresent(t, repo, console.ID)

	if _, err := service.PreviewCleanup(ctx, accountdomain.ProviderWeb, nil, nil); err == nil {
		t.Fatal("empty statuses preview unexpectedly succeeded")
	}
}
