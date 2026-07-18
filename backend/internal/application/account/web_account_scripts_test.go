package account

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestRunWebAccountScriptsReportsProgressAndIsolatesFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	first := createWebAccountForScriptTest(t, ctx, repo, "first")
	second := createWebAccountForScriptTest(t, ctx, repo, "second")
	adapter.failures = map[uint64]map[string]error{
		second.ID: {"setBirthDate": errors.New("birth rejected")},
	}

	progress := make([][2]int, 0, 3)
	succeeded, failed, err := service.RunWebAccountScriptsWithProgress(ctx, []uint64{first.ID, second.ID}, WebAccountScriptOptions{
		AcceptTerms: true,
		EnableNSFW:  true,
	}, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("succeeded=%d failed=%d", succeeded, failed)
	}
	if want := [][2]int{{0, 2}, {1, 2}, {2, 2}}; !reflect.DeepEqual(progress, want) {
		t.Fatalf("progress = %#v, want %#v", progress, want)
	}
	if calls := adapter.accountCalls(first.ID); !reflect.DeepEqual(calls, []string{"acceptTerms", "setBirthDate", "enableNSFW"}) {
		t.Fatalf("first calls = %#v", calls)
	}
	if calls := adapter.accountCalls(second.ID); !reflect.DeepEqual(calls, []string{"acceptTerms", "setBirthDate"}) {
		t.Fatalf("second calls = %#v", calls)
	}
	firstStored, err := repo.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondStored, err := repo.Get(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstStored.WebNSFWEnabledAt == nil || secondStored.WebNSFWEnabledAt != nil {
		t.Fatalf("markers first=%v second=%v", firstStored.WebNSFWEnabledAt, secondStored.WebNSFWEnabledAt)
	}
}

func TestEnableWebNSFWAlwaysSetsBirthDateFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	credential := createWebAccountForScriptTest(t, ctx, repo, "nsfw")

	if err := service.EnableWebNSFW(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	if calls := adapter.accountCalls(credential.ID); !reflect.DeepEqual(calls, []string{"setBirthDate", "enableNSFW"}) {
		t.Fatalf("calls = %#v", calls)
	}
	stored, err := repo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebNSFWEnabledAt == nil || !stored.WebNSFWEnabledAt.Equal(service.now()) {
		t.Fatalf("NSFW marker = %v, want %s", stored.WebNSFWEnabledAt, service.now())
	}
}

func TestEnableWebNSFWContinuesWhenBirthDateIsAlreadySet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	credential := createWebAccountForScriptTest(t, ctx, repo, "nsfw-existing-birth-date")
	adapter.failures = map[uint64]map[string]error{
		credential.ID: {"setBirthDate": provider.ErrBirthDateAlreadySet},
	}

	if err := service.EnableWebNSFW(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	if calls := adapter.accountCalls(credential.ID); !reflect.DeepEqual(calls, []string{"setBirthDate", "enableNSFW"}) {
		t.Fatalf("calls = %#v", calls)
	}
	stored, err := repo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebNSFWEnabledAt == nil {
		t.Fatal("successful NSFW was not marked after an already-set birth date")
	}
}

func TestEnableWebNSFWDoesNotMarkUpstreamFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	credential := createWebAccountForScriptTest(t, ctx, repo, "nsfw-failed")
	adapter.failures = map[uint64]map[string]error{
		credential.ID: {"enableNSFW": errors.New("nsfw rejected")},
	}

	if err := service.EnableWebNSFW(ctx, credential.ID); err == nil {
		t.Fatal("expected NSFW failure")
	}
	stored, err := repo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebNSFWEnabledAt != nil {
		t.Fatalf("failed NSFW was marked at %s", stored.WebNSFWEnabledAt)
	}
}

func TestEnableWebNSFWPersistsMarkerAfterClientCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	credential := createWebAccountForScriptTest(t, ctx, repo, "nsfw-canceled")
	adapter.afterCall = func(action string) {
		if action == "enableNSFW" {
			cancel()
		}
	}

	if err := service.EnableWebNSFW(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebNSFWEnabledAt == nil {
		t.Fatal("successful upstream NSFW was not marked after client cancellation")
	}
}

func TestRunAllWebAccountScriptsOnlyProcessesWebAccounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	first := createWebAccountForScriptTest(t, ctx, repo, "first")
	second := createWebAccountForScriptTest(t, ctx, repo, "second")
	if _, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
		Name: "build", SourceKey: "build", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	}); err != nil {
		t.Fatal(err)
	}

	progress := make([][2]int, 0, 3)
	succeeded, failed, err := service.RunAllWebAccountScriptsWithProgress(ctx, WebAccountScriptOptions{AcceptTerms: true}, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 2 || failed != 0 {
		t.Fatalf("succeeded=%d failed=%d", succeeded, failed)
	}
	if want := [][2]int{{0, 2}, {1, 2}, {2, 2}}; !reflect.DeepEqual(progress, want) {
		t.Fatalf("progress = %#v, want %#v", progress, want)
	}
	if calls := adapter.accountCalls(first.ID); !reflect.DeepEqual(calls, []string{"acceptTerms"}) {
		t.Fatalf("first calls = %#v", calls)
	}
	if calls := adapter.accountCalls(second.ID); !reflect.DeepEqual(calls, []string{"acceptTerms"}) {
		t.Fatalf("second calls = %#v", calls)
	}
}

func TestRunWebAccountScriptsRejectsEmptyPlan(t *testing.T) {
	t.Parallel()
	service, _, _ := newWebAccountSettingsTestService(t)
	if _, _, err := service.RunWebAccountScriptsWithProgress(context.Background(), []uint64{1}, WebAccountScriptOptions{}, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
}

func TestWebAccountScriptsRejectConcurrentWorkForTheSameAccount(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	service, repo, _ := newWebAccountSettingsTestService(t)
	credential := createWebAccountForScriptTest(t, ctx, repo, "serialized")
	adapter := &blockingWebAccountSettingsAdapter{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	service.providers = provider.NewRegistry(adapter)
	service.refreshLock = memory.NewLockStore()

	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- service.AcceptWebTerms(ctx, credential.ID) }()
	select {
	case <-adapter.entered:
	case <-time.After(time.Second):
		t.Fatal("first script did not reach the adapter")
	}
	if err := service.AcceptWebTerms(ctx, credential.ID); !errors.Is(err, ErrWebAccountScriptBusy) {
		close(adapter.release)
		t.Fatalf("concurrent err = %v", err)
	}
	close(adapter.release)
	if err := <-errorsChannel; err != nil {
		t.Fatal(err)
	}
	if got := adapter.maxActive.Load(); got != 1 {
		t.Fatalf("max active = %d", got)
	}
}

type blockingWebAccountSettingsAdapter struct {
	active    atomic.Int32
	maxActive atomic.Int32
	entered   chan struct{}
	release   chan struct{}
}

func (*blockingWebAccountSettingsAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderWeb
}

func (a *blockingWebAccountSettingsAdapter) AcceptTerms(ctx context.Context, _ accountdomain.Credential) error {
	active := a.active.Add(1)
	defer a.active.Add(-1)
	for {
		current := a.maxActive.Load()
		if active <= current || a.maxActive.CompareAndSwap(current, active) {
			break
		}
	}
	a.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.release:
		return nil
	}
}

func (*blockingWebAccountSettingsAdapter) SetBirthDate(context.Context, accountdomain.Credential, time.Time) error {
	return nil
}

func (*blockingWebAccountSettingsAdapter) EnableNSFW(context.Context, accountdomain.Credential) error {
	return nil
}

func createWebAccountForScriptTest(t *testing.T, ctx context.Context, repo interface {
	UpsertByIdentity(context.Context, accountdomain.Credential) (accountdomain.Credential, bool, error)
}, sourceKey string) accountdomain.Credential {
	t.Helper()
	credential, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: sourceKey, SourceKey: sourceKey, EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	return credential
}
