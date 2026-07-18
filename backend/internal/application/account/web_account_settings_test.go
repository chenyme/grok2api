package account

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestWebAccountSettingsAreWebOnlyAndGenerateBirthDate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	webAccount, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "web", SourceKey: "web", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	buildAccount, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
		Name: "build", SourceKey: "build", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := service.AcceptWebTerms(ctx, webAccount.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.SetWebBirthDate(ctx, webAccount.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.EnableWebNSFW(ctx, webAccount.ID); err != nil {
		t.Fatal(err)
	}
	earliest, latest := webBirthDateRange(service.now().In(time.Local))
	if adapter.terms != 1 || adapter.birthDate.Before(earliest) || adapter.birthDate.After(latest) || adapter.nsfw != 1 {
		t.Fatalf("adapter terms=%d birth=%v nsfw=%d", adapter.terms, adapter.birthDate, adapter.nsfw)
	}
	updatedWeb, err := repo.Get(ctx, webAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedWeb.WebTermsAcceptedAt == nil || updatedWeb.WebTermsAcceptedVersion != accountdomain.CurrentWebTermsVersion || updatedWeb.WebBirthDateSetAt == nil || updatedWeb.WebNSFWEnabledAt == nil || !updatedWeb.WebTermsAcceptedAt.Equal(service.now()) || !updatedWeb.WebBirthDateSetAt.Equal(service.now()) || !updatedWeb.WebNSFWEnabledAt.Equal(service.now()) {
		t.Fatalf("profile markers terms=%v version=%d birth=%v nsfw=%v", updatedWeb.WebTermsAcceptedAt, updatedWeb.WebTermsAcceptedVersion, updatedWeb.WebBirthDateSetAt, updatedWeb.WebNSFWEnabledAt)
	}
	if err := service.AcceptWebTerms(ctx, webAccount.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.SetWebBirthDate(ctx, webAccount.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.EnableWebNSFW(ctx, webAccount.ID); err != nil {
		t.Fatal(err)
	}
	if adapter.terms != 1 || adapter.birthCalls != 1 || adapter.nsfw != 1 {
		t.Fatalf("recorded steps repeated: terms=%d birth=%d nsfw=%d", adapter.terms, adapter.birthCalls, adapter.nsfw)
	}
	if err := service.AcceptWebTerms(ctx, buildAccount.ID); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("build err = %v", err)
	}
	if err := service.SetWebBirthDate(ctx, buildAccount.ID); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("build birth err = %v", err)
	}
}

func TestRandomWebBirthDateUsesInclusiveAgeRange(t *testing.T) {
	t.Parallel()
	// 当地已跨日但 UTC 仍是前一天时，必须按当地日历日期计算年龄。
	now := time.Date(2026, 7, 18, 0, 30, 0, 0, time.FixedZone("test", 8*60*60))
	value, err := randomWebBirthDate(now, bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	earliest := time.Date(1985, 7, 19, 0, 0, 0, 0, time.UTC)
	latest := time.Date(2006, 7, 18, 0, 0, 0, 0, time.UTC)
	if value.Before(earliest) || value.After(latest) {
		t.Fatalf("birth date %s is outside [%s, %s]", value, earliest, latest)
	}
}

func TestPendingWebAccountScriptOptionsSkipsRecordedSteps(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	all := WebAccountScriptOptions{AcceptTerms: true, SetBirthDate: true, EnableNSFW: true}
	tests := []struct {
		name       string
		credential accountdomain.Credential
		want       WebAccountScriptOptions
	}{
		{name: "none recorded", want: all},
		{name: "legacy terms marker requires current version", credential: accountdomain.Credential{WebTermsAcceptedAt: &now}, want: all},
		{name: "current terms recorded", credential: accountdomain.Credential{WebTermsAcceptedAt: &now, WebTermsAcceptedVersion: accountdomain.CurrentWebTermsVersion}, want: WebAccountScriptOptions{SetBirthDate: true, EnableNSFW: true}},
		{name: "birth recorded", credential: accountdomain.Credential{WebBirthDateSetAt: &now}, want: WebAccountScriptOptions{AcceptTerms: true, EnableNSFW: true}},
		{name: "nsfw implies birth", credential: accountdomain.Credential{WebNSFWEnabledAt: &now}, want: WebAccountScriptOptions{AcceptTerms: true}},
		{name: "all recorded", credential: accountdomain.Credential{WebTermsAcceptedAt: &now, WebTermsAcceptedVersion: accountdomain.CurrentWebTermsVersion, WebBirthDateSetAt: &now, WebNSFWEnabledAt: &now}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := pendingWebAccountScriptOptions(test.credential, all); got != test.want {
				t.Fatalf("options = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestWebBirthDateRangeHandlesLeapDay(t *testing.T) {
	t.Parallel()
	earliest, latest := webBirthDateRange(time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC))
	if want := time.Date(1983, 3, 1, 0, 0, 0, 0, time.UTC); !earliest.Equal(want) {
		t.Fatalf("earliest = %s, want %s", earliest, want)
	}
	if want := time.Date(2004, 2, 29, 0, 0, 0, 0, time.UTC); !latest.Equal(want) {
		t.Fatalf("latest = %s, want %s", latest, want)
	}
}

func TestRandomWebBirthDateRejectsUnavailableRandomSource(t *testing.T) {
	t.Parallel()
	if _, err := randomWebBirthDate(time.Now(), nil); err == nil {
		t.Fatal("expected nil random source error")
	}
	if _, err := randomWebBirthDate(time.Now(), errorReader{}); err == nil {
		t.Fatal("expected random source read error")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestWebAccountSettingsUnauthorizedMarksReauth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	credential, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "expired", SourceKey: "expired", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter.err = provider.ErrUnauthorized
	if err := service.AcceptWebTerms(ctx, credential.ID); !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	updated, err := repo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AuthStatus != accountdomain.AuthStatusReauthRequired {
		t.Fatalf("auth status = %s", updated.AuthStatus)
	}
}

func newWebAccountSettingsTestService(t *testing.T) (*Service, *relational.AccountRepository, *webAccountSettingsAdapterStub) {
	t.Helper()
	database, err := relational.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "web-account-settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	adapter := &webAccountSettingsAdapterStub{}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	service.now = func() time.Time { return time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC) }
	return service, repo, adapter
}

type webAccountSettingsAdapterStub struct {
	mu            sync.Mutex
	terms         int
	birthDate     time.Time
	birthCalls    int
	nsfw          int
	err           error
	calls         map[uint64][]string
	failures      map[uint64]map[string]error
	afterCall     func(string)
	identity      provider.AccountIdentity
	identityErr   error
	identityCalls int
}

func (a *webAccountSettingsAdapterStub) SyncAccountIdentity(context.Context, accountdomain.Credential) (provider.AccountIdentity, error) {
	a.mu.Lock()
	a.identityCalls++
	a.mu.Unlock()
	return a.identity, a.identityErr
}

func (*webAccountSettingsAdapterStub) Provider() accountdomain.Provider {
	return accountdomain.ProviderWeb
}

func (a *webAccountSettingsAdapterStub) AcceptTerms(_ context.Context, credential accountdomain.Credential) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.terms++
	return a.recordLocked(credential.ID, "acceptTerms")
}

func (a *webAccountSettingsAdapterStub) SetBirthDate(_ context.Context, credential accountdomain.Credential, value time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.birthDate = value
	a.birthCalls++
	return a.recordLocked(credential.ID, "setBirthDate")
}

func (a *webAccountSettingsAdapterStub) EnableNSFW(_ context.Context, credential accountdomain.Credential) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nsfw++
	return a.recordLocked(credential.ID, "enableNSFW")
}

func (a *webAccountSettingsAdapterStub) recordLocked(accountID uint64, action string) error {
	if a.calls == nil {
		a.calls = make(map[uint64][]string)
	}
	a.calls[accountID] = append(a.calls[accountID], action)
	var err error
	if failure := a.failures[accountID]; failure != nil {
		err = failure[action]
	}
	if err == nil {
		err = a.err
	}
	if a.afterCall != nil {
		a.afterCall(action)
	}
	return err
}

func (a *webAccountSettingsAdapterStub) accountCalls(accountID uint64) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls[accountID]...)
}
