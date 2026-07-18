package account

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestClassifyHealthProbe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		err    error
		want   HealthProbeStatus
	}{
		{"ok", 200, nil, HealthProbeHealthy},
		{"created", 201, nil, HealthProbeHealthy},
		{"unauthorized", 401, nil, HealthProbeUnauthorized},
		{"payment", 402, nil, HealthProbePayment},
		{"forbidden", 403, nil, HealthProbeForbidden},
		{"rate", 429, nil, HealthProbeRateLimited},
		{"unknown", 500, nil, HealthProbeUnknown},
		{"deadline", 0, context.DeadlineExceeded, HealthProbeNetwork},
		{"net", 0, &net.OpError{Op: "dial", Err: errors.New("connection refused")}, HealthProbeNetwork},
		{"local", 0, errors.New("decrypt failed"), HealthProbeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyHealthProbe(tc.status, tc.err)
			if got != tc.want {
				t.Fatalf("status=%v want=%v got=%v", tc.status, tc.want, got)
			}
		})
	}
}

func TestIsHealthProbeNetworkError(t *testing.T) {
	t.Parallel()
	if !isHealthProbeNetworkError(context.DeadlineExceeded) {
		t.Fatal("deadline should be network")
	}
	if isHealthProbeNetworkError(errors.New("decrypt failed")) {
		t.Fatal("local error should not be network")
	}
	if !isHealthProbeNetworkError(errors.New("dial tcp: i/o timeout")) {
		t.Fatal("timeout string should be network")
	}
	if !isHealthProbeNetworkError(&net.DNSError{Err: "no such host", Name: "example.invalid", IsNotFound: true}) {
		t.Fatal("dns error should be network")
	}
	_ = http.StatusOK
	_ = time.Second
}

func TestProbeBuildHealthAutoRefreshesOn401(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, credential, adapter, cipher := newHealthProbeTestService(t)

	// 首次 access token 返回 401，刷新后的 token 返回 200。
	adapter.probeByToken = map[string]int{
		"access-old": http.StatusUnauthorized,
		"access-1":   http.StatusOK,
	}

	summary, err := service.ProbeBuildHealth(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 1 || summary.Healthy != 1 || summary.Unauthorized != 0 || summary.Refreshed != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(summary.Items) != 1 || !summary.Items[0].Refreshed || summary.Items[0].Status != HealthProbeHealthy {
		t.Fatalf("item = %#v", summary.Items[0])
	}
	if adapter.probeCount.Load() != 2 {
		t.Fatalf("probe count = %d, want 2", adapter.probeCount.Load())
	}
	if adapter.refreshCount.Load() != 1 {
		t.Fatalf("refresh count = %d, want 1", adapter.refreshCount.Load())
	}

	// 持久化的 token 应已轮转。
	updated, err := service.accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	access, err := cipher.Decrypt(updated.EncryptedAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if access != "access-1" {
		t.Fatalf("access token = %q", access)
	}
	if updated.AuthStatus != accountdomain.AuthStatusActive {
		t.Fatalf("auth status = %s", updated.AuthStatus)
	}
}

func TestProbeBuildHealthMarksReauthWhenStillUnauthorizedAfterRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, credential, adapter, _ := newHealthProbeTestService(t)
	adapter.probeStatus = http.StatusUnauthorized

	summary, err := service.ProbeBuildHealth(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 1 || summary.Unauthorized != 1 || summary.Refreshed != 1 || summary.Healthy != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if !summary.Items[0].Refreshed || summary.Items[0].Status != HealthProbeUnauthorized {
		t.Fatalf("item = %#v", summary.Items[0])
	}
	if !strings.Contains(summary.Items[0].Error, "仍拒绝") {
		t.Fatalf("error = %q", summary.Items[0].Error)
	}
	if adapter.refreshCount.Load() != 1 || adapter.probeCount.Load() != 2 {
		t.Fatalf("refresh=%d probe=%d", adapter.refreshCount.Load(), adapter.probeCount.Load())
	}

	updated, err := service.accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AuthStatus != accountdomain.AuthStatusReauthRequired {
		t.Fatalf("auth status = %s", updated.AuthStatus)
	}
}

func TestProbeBuildHealthSkipsRefreshWithoutRefreshToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, credential, adapter, _ := newHealthProbeTestService(t, healthProbeFixture{withRefreshToken: false})
	adapter.probeStatus = http.StatusUnauthorized

	summary, err := service.ProbeBuildHealth(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Unauthorized != 1 || summary.Refreshed != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if adapter.refreshCount.Load() != 0 || adapter.probeCount.Load() != 1 {
		t.Fatalf("refresh=%d probe=%d", adapter.refreshCount.Load(), adapter.probeCount.Load())
	}
	if !strings.Contains(summary.Items[0].Error, "无法自动重刷") {
		t.Fatalf("error = %q", summary.Items[0].Error)
	}
	updated, err := service.accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AuthStatus != accountdomain.AuthStatusReauthRequired {
		t.Fatalf("auth status = %s, want reauthRequired", updated.AuthStatus)
	}
}

func TestProbeBuildHealthSyncsForbiddenAndCooldownStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("forbidden marks reauth", func(t *testing.T) {
		service, credential, adapter, _ := newHealthProbeTestService(t)
		adapter.probeStatus = http.StatusForbidden
		summary, err := service.ProbeBuildHealth(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if summary.Forbidden != 1 {
			t.Fatalf("summary = %#v", summary)
		}
		updated, err := service.accounts.Get(ctx, credential.ID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.AuthStatus != accountdomain.AuthStatusReauthRequired {
			t.Fatalf("auth status = %s", updated.AuthStatus)
		}
		if !strings.Contains(updated.LastError, "403") && !strings.Contains(updated.LastError, "health probe") {
			t.Fatalf("last error = %q", updated.LastError)
		}
	})

	t.Run("rate limited sets cooldown", func(t *testing.T) {
		service, credential, adapter, _ := newHealthProbeTestService(t)
		adapter.probeStatus = http.StatusTooManyRequests
		if _, err := service.ProbeBuildHealth(ctx, nil); err != nil {
			t.Fatal(err)
		}
		updated, err := service.accounts.Get(ctx, credential.ID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.AuthStatus != accountdomain.AuthStatusActive {
			t.Fatalf("auth status = %s", updated.AuthStatus)
		}
		if updated.FailureCount != 1 || updated.CooldownUntil == nil || !updated.CooldownUntil.After(time.Now().UTC()) {
			t.Fatalf("cooldown state = %#v", updated)
		}
	})

	t.Run("healthy clears cooldown and reactivates", func(t *testing.T) {
		service, credential, adapter, _ := newHealthProbeTestService(t)
		until := time.Now().UTC().Add(10 * time.Minute)
		if err := service.accounts.UpdateHealth(ctx, credential.ID, 3, &until, "old failure", false); err != nil {
			t.Fatal(err)
		}
		if err := service.MarkReauthRequired(ctx, credential.ID, "previous reauth"); err != nil {
			t.Fatal(err)
		}
		adapter.probeStatus = http.StatusOK
		if _, err := service.ProbeBuildHealth(ctx, nil); err != nil {
			t.Fatal(err)
		}
		updated, err := service.accounts.Get(ctx, credential.ID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.AuthStatus != accountdomain.AuthStatusActive {
			t.Fatalf("auth status = %s", updated.AuthStatus)
		}
		if updated.FailureCount != 0 || updated.CooldownUntil != nil || updated.LastError != "" {
			t.Fatalf("healthy state not cleared: %#v", updated)
		}
	})

	t.Run("network only records last error", func(t *testing.T) {
		service, credential, adapter, _ := newHealthProbeTestService(t)
		adapter.probeStatus = 0
		adapter.probeErr = &net.OpError{Op: "dial", Err: errors.New("connection refused")}
		if _, err := service.ProbeBuildHealth(ctx, nil); err != nil {
			t.Fatal(err)
		}
		updated, err := service.accounts.Get(ctx, credential.ID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.AuthStatus != accountdomain.AuthStatusActive {
			t.Fatalf("auth status = %s", updated.AuthStatus)
		}
		if updated.CooldownUntil != nil {
			t.Fatalf("network should not cooldown: %#v", updated)
		}
		if !strings.Contains(updated.LastError, "health probe") {
			t.Fatalf("last error = %q", updated.LastError)
		}
	})
}

type healthProbeFixture struct {
	withRefreshToken bool
}

func newHealthProbeTestService(t *testing.T, fixtures ...healthProbeFixture) (*Service, accountdomain.Credential, *healthProbeAdapter, *security.Cipher) {
	t.Helper()
	fixture := healthProbeFixture{withRefreshToken: true}
	if len(fixtures) > 0 {
		fixture = fixtures[0]
	}
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "health-probe.db"))
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
	access, err := cipher.Encrypt("access-old")
	if err != nil {
		t.Fatal(err)
	}
	refresh := ""
	if fixture.withRefreshToken {
		refresh, err = cipher.Encrypt("refresh-old")
		if err != nil {
			t.Fatal(err)
		}
	}
	repository := relational.NewAccountRepository(database)
	credential, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider:              accountdomain.ProviderBuild,
		Name:                  "probe-test",
		Email:                 "probe@example.com",
		SourceKey:             "probe-test",
		EncryptedAccessToken:  access,
		EncryptedRefreshToken: refresh,
		ExpiresAt:             time.Now().UTC().Add(time.Hour),
		Enabled:               true,
		AuthStatus:            accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &healthProbeAdapter{cipher: cipher, probeStatus: http.StatusOK}
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)
	return service, credential, adapter, cipher
}

type healthProbeAdapter struct {
	cipher       *security.Cipher
	probeStatus  int
	probeErr     error
	probeByToken map[string]int
	probeCount   atomic.Int64
	refreshCount atomic.Int64
	refreshErr   error
}

func (a *healthProbeAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }

func (a *healthProbeAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider:   accountdomain.ProviderBuild,
		Quota:      provider.QuotaBilling,
		Credential: provider.CredentialSurface{Refresh: true},
	}
}

func (a *healthProbeAdapter) ProbeResponses(_ context.Context, _ accountdomain.Credential, accessToken string) (int, error) {
	a.probeCount.Add(1)
	if a.probeByToken != nil {
		if status, ok := a.probeByToken[accessToken]; ok {
			return status, nil
		}
		return 0, fmt.Errorf("unexpected access token %q", accessToken)
	}
	if a.probeErr != nil {
		return a.probeStatus, a.probeErr
	}
	return a.probeStatus, nil
}

func (a *healthProbeAdapter) RefreshCredential(context.Context, accountdomain.Credential) (provider.RefreshedCredential, error) {
	count := a.refreshCount.Add(1)
	if a.refreshErr != nil {
		return provider.RefreshedCredential{}, a.refreshErr
	}
	access, err := a.cipher.Encrypt(fmt.Sprintf("access-%d", count))
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	refresh, err := a.cipher.Encrypt(fmt.Sprintf("refresh-%d", count))
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	return provider.RefreshedCredential{
		EncryptedAccessToken:  access,
		EncryptedRefreshToken: refresh,
		ExpiresAt:             time.Now().UTC().Add(time.Hour),
	}, nil
}

func (a *healthProbeAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return nil, nil
}

func (a *healthProbeAdapter) ListModels(context.Context, accountdomain.Credential) ([]string, error) {
	return nil, nil
}

func (a *healthProbeAdapter) GetBilling(context.Context, accountdomain.Credential) (accountdomain.Billing, error) {
	return accountdomain.Billing{}, nil
}
