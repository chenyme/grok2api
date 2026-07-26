package account

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestBatchDeleteWithLinkedRemovesPeersAndKeepsUntargetedWeb(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, service := newLinkedDeleteTestService(t, "svc-linked-delete.db")
	web, build, console := seedLinkedTrio(t, repo, strings.Repeat("1", 64), "u1")

	result, err := service.BatchDeleteWithLinked(ctx, accountdomain.ProviderBuild, []uint64{build.ID}, []accountdomain.Provider{accountdomain.ProviderConsole})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 2 || result.LinkedDeleted != 1 {
		t.Fatalf("result = %#v", result)
	}
	assertAccountMissing(t, repo, build.ID)
	assertAccountMissing(t, repo, console.ID)
	assertAccountPresent(t, repo, web.ID)

	web2 := mustUpsertLinked(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web2", SourceKey: "sso:" + strings.Repeat("2", 64),
	})
	deleted, err := service.BatchDelete(ctx, []uint64{web2.ID})
	if err != nil || deleted != 1 {
		t.Fatalf("legacy batch delete deleted=%d err=%v", deleted, err)
	}
}

func TestBatchDeleteWithLinkedWebDeletesBuildAndConsole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, service := newLinkedDeleteTestService(t, "svc-linked-delete-web.db")
	web, build, console := seedLinkedTrio(t, repo, strings.Repeat("a", 64), "u-web")

	result, err := service.BatchDeleteWithLinked(ctx, accountdomain.ProviderWeb, []uint64{web.ID}, []accountdomain.Provider{accountdomain.ProviderBuild, accountdomain.ProviderConsole})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 3 || result.RootsDeleted != 1 || result.LinkedDeleted != 2 {
		t.Fatalf("result = %#v", result)
	}
	assertAccountMissing(t, repo, web.ID)
	assertAccountMissing(t, repo, build.ID)
	assertAccountMissing(t, repo, console.ID)
}

func TestBatchDeleteWithLinkedMixedBatchOnlyExpandsLinkedRoots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, service := newLinkedDeleteTestService(t, "svc-linked-delete-mix.db")
	webLinked, _, console := seedLinkedTrio(t, repo, strings.Repeat("b", 64), "u-mix")
	webOnly := mustUpsertLinked(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web-only", SourceKey: "sso:" + strings.Repeat("c", 64),
	})

	result, err := service.BatchDeleteWithLinked(ctx, accountdomain.ProviderWeb, []uint64{webLinked.ID, webOnly.ID}, []accountdomain.Provider{accountdomain.ProviderConsole})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 3 {
		t.Fatalf("result = %#v", result)
	}
	assertAccountMissing(t, repo, webLinked.ID)
	assertAccountMissing(t, repo, webOnly.ID)
	assertAccountMissing(t, repo, console.ID)
}

func TestPreviewLinkedDeleteCountsWithoutDeleting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, service := newLinkedDeleteTestService(t, "svc-linked-preview.db")
	web, build, console := seedLinkedTrio(t, repo, strings.Repeat("d", 64), "u-prev")

	res, err := service.PreviewLinkedDelete(ctx, accountdomain.ProviderWeb, []uint64{web.ID}, []accountdomain.Provider{accountdomain.ProviderBuild, accountdomain.ProviderConsole})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RootIDs) != 1 || res.LinkedByProvider[accountdomain.ProviderBuild] != 1 || res.LinkedByProvider[accountdomain.ProviderConsole] != 1 || len(res.FinalIDs) != 3 {
		t.Fatalf("preview = %#v", res)
	}
	assertAccountPresent(t, repo, web.ID)
	assertAccountPresent(t, repo, build.ID)
	assertAccountPresent(t, repo, console.ID)
}

func TestBatchDeleteWithLinkedEmptyTargetsDeletesRootOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, service := newLinkedDeleteTestService(t, "svc-linked-empty-targets.db")
	web, build, console := seedLinkedTrio(t, repo, strings.Repeat("e", 64), "u-empty")

	result, err := service.BatchDeleteWithLinked(ctx, accountdomain.ProviderWeb, []uint64{web.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || result.LinkedDeleted != 0 {
		t.Fatalf("result = %#v", result)
	}
	assertAccountMissing(t, repo, web.ID)
	assertAccountPresent(t, repo, build.ID)
	assertAccountPresent(t, repo, console.ID)
}

func TestBatchDeleteWithLinkedInvalidTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, service := newLinkedDeleteTestService(t, "svc-linked-bad-target.db")
	web, _, _ := seedLinkedTrio(t, repo, strings.Repeat("f", 64), "u-bad")

	if _, err := service.BatchDeleteWithLinked(ctx, accountdomain.ProviderWeb, []uint64{web.ID}, []accountdomain.Provider{accountdomain.Provider("nope")}); err == nil {
		t.Fatal("expected invalid target error")
	}
	if _, err := service.BatchDeleteWithLinked(ctx, accountdomain.ProviderWeb, []uint64{web.ID}, []accountdomain.Provider{accountdomain.ProviderWeb}); err == nil {
		t.Fatal("expected self-target error")
	}
	assertAccountPresent(t, repo, web.ID)
}

func TestDeleteMissingAccountReturnsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, service := newLinkedDeleteTestService(t, "svc-delete-missing.db")

	if err := service.Delete(ctx, 9_999_999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing: err=%v", err)
	}
	if _, err := service.DeleteWithLinked(ctx, accountdomain.ProviderWeb, 9_999_999, []accountdomain.Provider{accountdomain.ProviderBuild}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteWithLinked missing: err=%v", err)
	}
}

func TestDeleteWithLinkedRejectsRootFromAnotherProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, service := newLinkedDeleteTestService(t, "svc-delete-provider-mismatch.db")
	web, _, _ := seedLinkedTrio(t, repo, strings.Repeat("9", 64), "u-provider-mismatch")

	_, err := service.DeleteWithLinked(ctx, accountdomain.ProviderBuild, web.ID, []accountdomain.Provider{accountdomain.ProviderConsole})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("provider mismatch error = %v", err)
	}
	assertAccountPresent(t, repo, web.ID)
}

func TestFinishLinkedDeleteUsesBatchStickyCleanup(t *testing.T) {
	sticky := &stickyBatchStub{}
	service := &Service{sticky: sticky}
	service.finishLinkedDelete(context.Background(), []uint64{3, 5, 8})
	if sticky.singleCalls != 0 {
		t.Fatalf("single-account cleanup calls = %d", sticky.singleCalls)
	}
	if len(sticky.batchCalls) != 1 || len(sticky.batchCalls[0]) != 3 || sticky.batchCalls[0][0] != 3 || sticky.batchCalls[0][2] != 8 {
		t.Fatalf("batch cleanup calls = %#v", sticky.batchCalls)
	}
}

func newLinkedDeleteTestService(t *testing.T, dbName string) (*relational.AccountRepository, *Service) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	return repo, &Service{accounts: repo, sticky: stickyStub{}, logger: nil}
}

func seedLinkedTrio(t *testing.T, repo *relational.AccountRepository, digest, userID string) (web, build, console accountdomain.Credential) {
	t.Helper()
	ctx := context.Background()
	web = mustUpsertLinked(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "sso:" + digest, UserID: userID,
	})
	build = mustUpsertLinked(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "build", SourceKey: "build-" + digest[:8], UserID: userID,
	})
	console = mustUpsertLinked(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO, Name: "console", SourceKey: "console-sso:" + digest, UserID: userID,
	})
	if err := repo.LinkWebToBuild(ctx, web.ID, build.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReconcileProviderLinks(ctx, web.ID); err != nil {
		t.Fatal(err)
	}
	return web, build, console
}

func mustUpsertLinked(t *testing.T, repo *relational.AccountRepository, value accountdomain.Credential) accountdomain.Credential {
	t.Helper()
	value.EncryptedAccessToken = "encrypted"
	value.Enabled = true
	value.AuthStatus = accountdomain.AuthStatusActive
	value.Priority = accountdomain.DefaultPriority
	value.MaxConcurrent = accountdomain.DefaultMaxConcurrent
	stored, _, err := repo.UpsertByIdentity(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func assertAccountMissing(t *testing.T, repo *relational.AccountRepository, id uint64) {
	t.Helper()
	if _, err := repo.Get(context.Background(), id); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("account %d should be missing, err=%v", id, err)
	}
}

func assertAccountPresent(t *testing.T, repo *relational.AccountRepository, id uint64) {
	t.Helper()
	if _, err := repo.Get(context.Background(), id); err != nil {
		t.Fatalf("account %d should remain: %v", id, err)
	}
}

type stickyStub struct{}

func (stickyStub) Get(context.Context, string, time.Time) (uint64, bool, error) {
	return 0, false, nil
}
func (stickyStub) Bind(context.Context, string, uint64, time.Time, time.Time) (uint64, error) {
	return 0, nil
}
func (stickyStub) Set(context.Context, string, uint64, time.Time) error { return nil }
func (stickyStub) DeleteByAccount(context.Context, uint64) error        { return nil }

type stickyBatchStub struct {
	singleCalls int
	batchCalls  [][]uint64
}

func (*stickyBatchStub) Get(context.Context, string, time.Time) (uint64, bool, error) {
	return 0, false, nil
}
func (*stickyBatchStub) Bind(context.Context, string, uint64, time.Time, time.Time) (uint64, error) {
	return 0, nil
}
func (*stickyBatchStub) Set(context.Context, string, uint64, time.Time) error { return nil }
func (s *stickyBatchStub) DeleteByAccount(context.Context, uint64) error {
	s.singleCalls++
	return nil
}
func (s *stickyBatchStub) DeleteByAccounts(_ context.Context, ids []uint64) error {
	s.batchCalls = append(s.batchCalls, append([]uint64(nil), ids...))
	return nil
}
