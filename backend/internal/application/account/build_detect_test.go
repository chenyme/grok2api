package account

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/batch"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type detectResponsesAdapter struct {
	status int
	body   []byte
}

func (a detectResponsesAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }
func (a detectResponsesAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{
		StatusCode: a.status,
		Body:       io.NopCloser(bytes.NewReader(a.body)),
	}, nil
}

func TestDetectBuildAccountsRequiresExplicitScope(t *testing.T) {
	service := &Service{}
	if _, _, err := service.DetectBuildAccountsWithProgress(context.Background(), nil, false, nil, nil); err == nil {
		t.Fatal("missing all and ids should be rejected")
	}
	if _, _, err := service.DetectBuildAccountsWithProgress(context.Background(), []uint64{1}, true, nil, nil); err == nil {
		t.Fatal("all and ids together should be rejected")
	}
	if _, _, err := service.DetectBuildAccountsWithProgress(context.Background(), []uint64{}, false, nil, nil); err == nil {
		t.Fatal("empty ids should not imply all accounts")
	}
}

func TestFinishBuildDetectCredentialErrorClassifiesPermanentRefreshAsInvalid(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "detect-refresh-error.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accessToken, err := cipher.Encrypt("expired-access-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	credential, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "expired-refresh", SourceKey: "expired-refresh",
		EncryptedAccessToken: accessToken, Enabled: true, AuthStatus: accountdomain.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(detectResponsesAdapter{}), cipher, nil)

	for _, refreshErr := range []error{
		ErrCredentialRefreshPermanent,
		&provider.CredentialRefreshError{Code: "invalid_grant", Permanent: true},
	} {
		item := service.finishBuildDetectCredentialError(ctx, credential, refreshErr)
		if item.Outcome != BuildDetectOutcomeInvalid {
			t.Fatalf("error %v produced outcome %s, want invalid", refreshErr, item.Outcome)
		}
	}

	item := service.finishBuildDetectCredentialError(ctx, credential, errors.New("temporary OAuth outage"))
	if item.Outcome != BuildDetectOutcomeFailed {
		t.Fatalf("temporary error outcome = %s, want failed", item.Outcome)
	}
}

func TestFinishBuildDetectResponseUsesScopedFailureState(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "detect.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accessToken, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	stored, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "spending-limit", UserID: "user-1",
		SourceKey: "detect-spending-limit", EncryptedAccessToken: accessToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(detectResponsesAdapter{}), cipher, nil)
	response := &provider.Response{
		StatusCode: http.StatusPaymentRequired,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"code":"personal-team-blocked:spending-limit","error":"blocked"}`))),
	}
	item := service.finishBuildDetectResponse(ctx, response, stored, nil)
	if item.Outcome != BuildDetectOutcomeFailed {
		t.Fatalf("outcome = %s, want failed, reason=%s", item.Outcome, item.Reason)
	}
	latest, err := repo.Get(ctx, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.AuthStatus != accountdomain.AuthStatusActive {
		t.Fatalf("auth status = %s, want active", latest.AuthStatus)
	}
	recovery, err := repo.GetQuotaRecovery(ctx, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Kind != accountdomain.QuotaRecoveryKindFree || recovery.Status != accountdomain.QuotaRecoveryStatusExhausted || recovery.NextProbeAt == nil {
		t.Fatalf("quota recovery = %#v", recovery)
	}

	deniedAccount, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "model-denied", UserID: "user-2",
		SourceKey: "detect-model-denied", EncryptedAccessToken: accessToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	deniedResponse := &provider.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"Access to the chat endpoint is denied"}`))),
	}
	item = service.finishBuildDetectResponse(ctx, deniedResponse, deniedAccount, nil)
	if item.Outcome != BuildDetectOutcomeFailed {
		t.Fatalf("model denial outcome = %s, want failed, reason=%s", item.Outcome, item.Reason)
	}
	latest, err = repo.Get(ctx, deniedAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.AuthStatus != accountdomain.AuthStatusActive {
		t.Fatalf("model denial auth status = %s, want active", latest.AuthStatus)
	}
	candidates, err := repo.ListRoutingCandidates(ctx, accountdomain.ProviderBuild, 0, buildDetectModel, "")
	if err != nil {
		t.Fatal(err)
	}
	var deniedCandidate *accountdomain.RoutingCandidate
	for index := range candidates {
		if candidates[index].Credential.ID == deniedAccount.ID {
			deniedCandidate = &candidates[index]
			break
		}
	}
	if deniedCandidate == nil || deniedCandidate.ModelQuotaBlock == nil || deniedCandidate.ModelQuotaBlock.Reason != "model_access_denied" {
		t.Fatalf("model-scoped denial was not persisted: %#v", candidates)
	}

	freeAccount, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "free-exhausted", UserID: "user-3",
		SourceKey: "detect-free-exhausted", EncryptedAccessToken: accessToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	item = service.finishBuildDetectResponse(ctx, &provider.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"code":"subscription:free-usage-exhausted","error":"tokens (actual/limit): 10/10"}`))),
	}, freeAccount, nil)
	if item.Outcome != BuildDetectOutcomeFailed {
		t.Fatalf("free quota outcome = %s, want failed", item.Outcome)
	}
	freeRecovery, err := repo.GetQuotaRecovery(ctx, freeAccount.ID)
	if err != nil || freeRecovery.Kind != accountdomain.QuotaRecoveryKindFree || freeRecovery.Status != accountdomain.QuotaRecoveryStatusExhausted {
		t.Fatalf("free quota recovery = %#v, err = %v", freeRecovery, err)
	}

	modelQuotaAccount, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "model-quota", UserID: "user-4",
		SourceKey: "detect-model-quota", EncryptedAccessToken: accessToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	item = service.finishBuildDetectResponse(ctx, &provider.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"You've used all the included free usage for model grok-4.5"}`))),
	}, modelQuotaAccount, nil)
	if item.Outcome != BuildDetectOutcomeFailed {
		t.Fatalf("model quota outcome = %s, want failed", item.Outcome)
	}
	candidates, err = repo.ListRoutingCandidates(ctx, accountdomain.ProviderBuild, 0, buildDetectModel, "")
	if err != nil {
		t.Fatal(err)
	}
	var modelQuotaCandidate *accountdomain.RoutingCandidate
	for index := range candidates {
		if candidates[index].Credential.ID == modelQuotaAccount.ID {
			modelQuotaCandidate = &candidates[index]
			break
		}
	}
	if modelQuotaCandidate == nil || modelQuotaCandidate.ModelQuotaBlock == nil || modelQuotaCandidate.ModelQuotaBlock.Reason != "model_quota_depleted" {
		t.Fatalf("model quota block was not persisted: %#v", modelQuotaCandidate)
	}
}

func TestDetectBuildAccountsStreamsInvalidOnlyForAll(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "detect-all.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accessToken, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	okAccount, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "ok", UserID: "user-ok",
		SourceKey: "detect-ok", EncryptedAccessToken: accessToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, RefreshPermanent: true, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	invalidAccount, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "invalid", UserID: "user-invalid",
		SourceKey: "detect-invalid", EncryptedAccessToken: accessToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, RefreshPermanent: true, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 401 属于凭据拒绝；全量模式只将 invalid 明细推送给 observer。
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(detectResponsesAdapter{
		status: http.StatusUnauthorized,
	}), cipher, nil)

	var items []BuildDetectItemResult
	succeeded, failed, err := service.DetectBuildAccountsWithProgress(ctx, nil, true, nil, func(item BuildDetectItemResult) error {
		items = append(items, item)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 0 || failed != 2 {
		t.Fatalf("summary succeeded=%d failed=%d", succeeded, failed)
	}
	if len(items) != 2 {
		t.Fatalf("all-mode items = %d, want 2 invalid only", len(items))
	}
	for _, item := range items {
		if item.Outcome != BuildDetectOutcomeInvalid {
			t.Fatalf("item = %#v", item)
		}
	}
	_ = okAccount
	_ = invalidAccount
}

func TestDetectBuildAccountsSerializesItemObserver(t *testing.T) {
	const accountCount = 8
	service := newConcurrentInvalidBuildDetectService(t, accountCount)

	started := make(chan struct{}, accountCount)
	release := make(chan struct{})
	var items []BuildDetectItemResult
	errCh := make(chan error, 1)
	go func() {
		_, _, detectErr := service.DetectBuildAccountsWithProgress(context.Background(), nil, true, nil, func(item BuildDetectItemResult) error {
			started <- struct{}{}
			<-release
			items = append(items, item)
			return nil
		})
		errCh <- detectErr
	}()

	deadline := time.After(2 * time.Second)
	select {
	case <-started:
	case <-deadline:
		t.Fatal("itemObserver was not invoked")
	}
	close(release)
	if detectErr := <-errCh; detectErr != nil {
		t.Fatal(detectErr)
	}
	if len(items) != accountCount {
		t.Fatalf("serialized items = %d, want %d", len(items), accountCount)
	}
}

func TestDetectBuildAccountsObserverPanicDoesNotDeadlock(t *testing.T) {
	const accountCount = 8
	service := newConcurrentInvalidBuildDetectService(t, accountCount)

	type detectResult struct {
		succeeded int
		failed    int
		err       error
	}
	var calls atomic.Int64
	resultCh := make(chan detectResult, 1)
	go func() {
		succeeded, failed, err := service.DetectBuildAccountsWithProgress(context.Background(), nil, true, nil, func(BuildDetectItemResult) error {
			if calls.Add(1) == 1 {
				panic("item observer panic")
			}
			return nil
		})
		resultCh <- detectResult{succeeded: succeeded, failed: failed, err: err}
	}()

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.succeeded != 0 || result.failed != accountCount {
			t.Fatalf("summary succeeded=%d failed=%d, want 0/%d", result.succeeded, result.failed, accountCount)
		}
		if got := calls.Load(); got != accountCount {
			t.Fatalf("observer calls = %d, want %d", got, accountCount)
		}
	case <-timer.C:
		t.Fatal("itemObserver panic deadlocked Build account detection")
	}
}

func newConcurrentInvalidBuildDetectService(t *testing.T, accountCount int) *Service {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "detect-observer.db"))
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
	accessToken, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	for index := range accountCount {
		if _, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: "invalid-" + strconv.Itoa(index), UserID: "user-invalid-" + strconv.Itoa(index),
			SourceKey: "detect-invalid-" + strconv.Itoa(index), EncryptedAccessToken: accessToken,
			Enabled: true, AuthStatus: accountdomain.AuthStatusActive, RefreshPermanent: true, Priority: 1, MaxConcurrent: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(detectResponsesAdapter{
		status: http.StatusUnauthorized,
	}), cipher, nil)
	service.SetDetectPool(batch.NewPool(accountCount))
	return service
}
