package gateway

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestCliPoolLeaseReportRelease(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "clipool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cipher, err := security.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	access, err := cipher.Encrypt("access-token-1")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := cipher.Encrypt("refresh-token-1")
	if err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	created, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "primary", SourceKey: "primary",
		Email: "a@example.com", UserID: "u1", OIDCClientID: "client-1",
		EncryptedAccessToken: access, EncryptedRefreshToken: refresh,
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	pool := NewCliPool(selector, accounts, nil, cipher)

	lease, err := pool.Lease(ctx, nil)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if lease.AccountID != created.ID || lease.AccessToken != "access-token-1" || lease.RefreshToken != "refresh-token-1" {
		t.Fatalf("unexpected lease: %+v", lease)
	}
	if err := pool.Report(ctx, lease.AccountID, ReportReasonFreeUsageExhausted, 429, lease.LeaseID); err != nil {
		t.Fatalf("report: %v", err)
	}
	if err := pool.Release(lease.LeaseID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := pool.Release(lease.LeaseID); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
}

func TestCliPoolLeaseExcludes(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "clipool-exclude.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cipher, err := security.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	for _, item := range []struct {
		name  string
		token string
	}{
		{"a", "tok-a"},
		{"b", "tok-b"},
	} {
		access, _ := cipher.Encrypt(item.token)
		refresh, _ := cipher.Encrypt("rt-" + item.token)
		if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: item.name, SourceKey: item.name,
			EncryptedAccessToken: access, EncryptedRefreshToken: refresh,
			Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
		}); err != nil {
			t.Fatal(err)
		}
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	pool := NewCliPool(selector, accounts, nil, cipher)

	first, err := pool.Lease(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Lease(ctx, []uint64{first.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	if second.AccountID == first.AccountID {
		t.Fatalf("expected excluded account to be skipped, got %#v then %#v", first, second)
	}
	_ = pool.Release(first.LeaseID)
	_ = pool.Release(second.LeaseID)
}
