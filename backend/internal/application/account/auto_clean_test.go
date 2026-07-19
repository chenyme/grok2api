package account

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestAutoCleanReauthDeletesOnlyAgedReauth(t *testing.T) {
	ctx := context.Background()
	service, repo := newAutoCleanTestService(t)

	create := func(name string, mutate func(*accountdomain.Credential)) accountdomain.Credential {
		t.Helper()
		value, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: name, SourceKey: "auto-clean-" + name,
			EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(&value)
			value, err = repo.Update(ctx, value)
			if err != nil {
				t.Fatal(err)
			}
		}
		return value
	}

	oldReauth := create("old-reauth", func(value *accountdomain.Credential) {
		value.AuthStatus = accountdomain.AuthStatusReauthRequired
	})
	freshReauth := create("fresh-reauth", func(value *accountdomain.Credential) {
		value.AuthStatus = accountdomain.AuthStatusReauthRequired
	})
	activePermanent := create("active-permanent", func(value *accountdomain.Credential) {
		value.EncryptedRefreshToken = "r"
		value.RefreshPermanent = true
		value.ExpiresAt = time.Now().UTC().Add(time.Hour)
	})
	cooldown := create("cooldown", func(value *accountdomain.Credential) {
		until := time.Now().UTC().Add(time.Hour)
		value.CooldownUntil = &until
	})
	disabledReauth := create("disabled-reauth", func(value *accountdomain.Credential) {
		value.Enabled = false
		value.AuthStatus = accountdomain.AuthStatusReauthRequired
	})

	// Past threshold: nothing is old enough relative to wall-clock updated_at.
	ids, err := repo.ListAutoCleanReauthIDs(ctx, time.Now().UTC().Add(-time.Hour), false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("unexpected aged ids right after create: %v", ids)
	}

	// Future threshold includes all reauth rows matching the enabled filter.
	ids, err = repo.ListAutoCleanReauthIDs(ctx, time.Now().UTC().Add(time.Hour), false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("enabled reauth candidates = %v", ids)
	}
	ids, err = repo.ListAutoCleanReauthIDs(ctx, time.Now().UTC().Add(time.Hour), true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("include disabled candidates = %v", ids)
	}

	// Flag off is a no-op even when minAge would match everything.
	service.now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }
	service.UpdateAutoCleanConfig(config.AccountsConfig{
		AutoCleanReauthEnabled: false, AutoCleanReauthInterval: config.Duration(10 * time.Minute),
		AutoCleanReauthMinAge: config.Duration(time.Hour),
	})
	deleted, scanned, err := service.AutoCleanReauthOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 || scanned != 0 {
		t.Fatalf("flag off deleted=%d scanned=%d", deleted, scanned)
	}

	service.UpdateAutoCleanConfig(config.AccountsConfig{
		AutoCleanReauthEnabled: true, AutoCleanReauthInterval: config.Duration(10 * time.Minute),
		AutoCleanReauthMinAge: config.Duration(time.Hour), AutoCleanDisabledEnabled: false,
	})
	deleted, scanned, err = service.AutoCleanReauthOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 || scanned != 2 {
		t.Fatalf("enabled cleanup deleted=%d scanned=%d", deleted, scanned)
	}
	assertMissing(t, repo, oldReauth.ID)
	assertMissing(t, repo, freshReauth.ID)
	assertPresent(t, repo, activePermanent.ID)
	assertPresent(t, repo, cooldown.ID)
	assertPresent(t, repo, disabledReauth.ID)

	service.UpdateAutoCleanConfig(config.AccountsConfig{
		AutoCleanReauthEnabled: true, AutoCleanReauthInterval: config.Duration(10 * time.Minute),
		AutoCleanReauthMinAge: config.Duration(time.Hour), AutoCleanDisabledEnabled: true,
	})
	deleted, scanned, err = service.AutoCleanReauthOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || scanned != 1 {
		t.Fatalf("disabled cleanup deleted=%d scanned=%d", deleted, scanned)
	}
	assertMissing(t, repo, disabledReauth.ID)
	assertPresent(t, repo, activePermanent.ID)
	assertPresent(t, repo, cooldown.ID)
}

func newAutoCleanTestService(t *testing.T) (*Service, *relational.AccountRepository) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auto-clean.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	service := NewService(repo, nil, nil, memory.NewStickyStore(), nil, nil, nil)
	return service, repo
}

func assertMissing(t *testing.T, repo *relational.AccountRepository, id uint64) {
	t.Helper()
	if _, err := repo.Get(context.Background(), id); err == nil {
		t.Fatalf("account %d still present", id)
	}
}

func assertPresent(t *testing.T, repo *relational.AccountRepository, id uint64) {
	t.Helper()
	if _, err := repo.Get(context.Background(), id); err != nil {
		t.Fatalf("account %d missing: %v", id, err)
	}
}
