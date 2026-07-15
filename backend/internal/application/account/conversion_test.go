package account

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestConvertWebAccountsToBuildIsIdempotent(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "conversion.db"))
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
	encryptedSSO, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAccountRepository(database)
	webAccount, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web-source",
		EncryptedAccessToken: encryptedSSO, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &buildConversionAdapter{}
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, memory.NewLockStore())
	first, err := service.ConvertWebAccountsToBuild(ctx, []uint64{webAccount.ID})
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 1 || first.Linked != 0 || first.Skipped != 0 || first.Failed != 0 || len(first.BuildAccountIDs) != 1 {
		t.Fatalf("first conversion = %#v", first)
	}
	var secondProgress [][2]int
	second, err := service.ConvertWebAccountsToBuildWithProgress(ctx, []uint64{webAccount.ID}, nil, func(completed, total int) error {
		secondProgress = append(secondProgress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Created != 0 || second.Linked != 0 || second.Skipped != 1 || second.Failed != 0 || adapter.calls.Load() != 1 {
		t.Fatalf("second conversion = %#v, calls = %d", second, adapter.calls.Load())
	}
	if len(secondProgress) != 1 || secondProgress[0] != [2]int{0, 0} {
		t.Fatalf("second progress = %#v", secondProgress)
	}
	linkedWeb, err := repository.Get(ctx, webAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linkedWeb.LinkedAccountID != first.BuildAccountIDs[0] || linkedWeb.LinkedProvider != accountdomain.ProviderBuild {
		t.Fatalf("linked web account = %#v", linkedWeb)
	}
}

func TestConvertWebAccountsToBuildAllRefreshesLinkedCredential(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "conversion-refresh.db"))
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
	encryptedSSO, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAccountRepository(database)
	webAccount, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web-source",
		EncryptedAccessToken: encryptedSSO, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &buildConversionAdapter{}
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, memory.NewLockStore())
	first, err := service.ConvertWebAccountsToBuild(ctx, []uint64{webAccount.ID})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := service.ConvertWebAccountsToBuildWithStrategy(ctx, []uint64{webAccount.ID}, BuildConversionAll, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Created != 0 || refreshed.Linked != 1 || refreshed.Skipped != 0 || refreshed.Failed != 0 || len(refreshed.BuildAccountIDs) != 1 {
		t.Fatalf("refreshed conversion = %#v", refreshed)
	}
	if refreshed.BuildAccountIDs[0] != first.BuildAccountIDs[0] || adapter.calls.Load() != 2 {
		t.Fatalf("build ids first=%v refreshed=%v calls=%d", first.BuildAccountIDs, refreshed.BuildAccountIDs, adapter.calls.Load())
	}
	buildAccount, err := repository.Get(ctx, first.BuildAccountIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	accessToken, err := cipher.Decrypt(buildAccount.EncryptedAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if accessToken != "access-2" {
		t.Fatalf("access token = %q", accessToken)
	}
}

func TestConvertAllWebAccountsToBuildUsesUnlinkedPool(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "conversion-all.db"))
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
	encryptedSSO, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAccountRepository(database)
	createWeb := func(name, source string) accountdomain.Credential {
		value, _, createErr := repository.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: name, SourceKey: source,
			EncryptedAccessToken: encryptedSSO, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return value
	}
	firstWeb := createWeb("web-1", "web-source-1")
	createWeb("web-2", "web-source-2")
	adapter := &buildConversionAdapter{}
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, memory.NewLockStore())
	if _, err := service.ConvertWebAccountsToBuild(ctx, []uint64{firstWeb.ID}); err != nil {
		t.Fatal(err)
	}
	observed := make([]uint64, 0, 1)
	progress := make([][2]int, 0, 2)
	result, err := service.ConvertAllWebAccountsToBuildWithProgress(ctx, func(accountID uint64) error {
		observed = append(observed, accountID)
		return nil
	}, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.Linked != 0 || result.Skipped != 0 || result.Failed != 0 || adapter.calls.Load() != 2 || len(observed) != 1 || observed[0] != result.BuildAccountIDs[0] {
		t.Fatalf("all conversion = %#v, calls = %d", result, adapter.calls.Load())
	}
	if len(progress) != 2 || progress[0] != [2]int{0, 1} || progress[1] != [2]int{1, 1} {
		t.Fatalf("progress = %#v", progress)
	}
	empty, err := service.ConvertAllWebAccountsToBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Created != 0 || empty.Linked != 0 || empty.Skipped != 0 || empty.Failed != 0 || len(empty.BuildAccountIDs) != 0 || adapter.calls.Load() != 2 {
		t.Fatalf("empty conversion = %#v, calls = %d", empty, adapter.calls.Load())
	}
	resynced, err := service.ConvertAllWebAccountsToBuildWithStrategy(ctx, BuildConversionAll, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resynced.Created != 0 || resynced.Linked != 2 || resynced.Skipped != 0 || resynced.Failed != 0 || len(resynced.BuildAccountIDs) != 2 || adapter.calls.Load() != 4 {
		t.Fatalf("resynced conversion = %#v, calls = %d", resynced, adapter.calls.Load())
	}
}

func TestConvertAllWebAccountsToBuildProcessesMoreThanLegacyLimitInBatches(t *testing.T) {
	const totalAccounts = maxBuildConversionAccounts + 1
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedSSO, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	repository := &conversionBatchRepository{total: totalAccounts, encryptedSSO: encryptedSSO}
	adapter := &buildConversionAdapter{}
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, memory.NewLockStore())
	progress := make([][2]int, 0, totalAccounts+1)
	result, err := service.ConvertAllWebAccountsToBuildWithProgress(context.Background(), nil, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != totalAccounts || result.Linked != 0 || result.Skipped != 0 || result.Failed != 0 || len(result.BuildAccountIDs) != totalAccounts {
		t.Fatalf("conversion result = %#v", result)
	}
	if adapter.calls.Load() != totalAccounts || repository.listCalls != 2 {
		t.Fatalf("adapter calls = %d, repository batches = %d", adapter.calls.Load(), repository.listCalls)
	}
	if len(progress) != totalAccounts+1 || progress[0] != [2]int{0, totalAccounts} || progress[len(progress)-1] != [2]int{totalAccounts, totalAccounts} {
		t.Fatalf("progress first=%v last=%v count=%d", progress[0], progress[len(progress)-1], len(progress))
	}
}

type conversionBatchRepository struct {
	repository.AccountRepository
	total        int
	encryptedSSO string
	listCalls    int
	nextBuildID  atomic.Uint64
}

func (r *conversionBatchRepository) ListUnlinkedWebAccountIDs(_ context.Context, afterID uint64, limit int) ([]uint64, int64, error) {
	r.listCalls++
	ids := make([]uint64, 0, limit)
	for id := afterID + 1; id <= uint64(r.total) && len(ids) < limit; id++ {
		ids = append(ids, id)
	}
	return ids, int64(r.total), nil
}

func (r *conversionBatchRepository) Get(_ context.Context, id uint64) (accountdomain.Credential, error) {
	return accountdomain.Credential{
		ID: id, Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: fmt.Sprintf("web-%d", id), SourceKey: fmt.Sprintf("web-source-%d", id),
		EncryptedAccessToken: r.encryptedSSO, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	}, nil
}

func (r *conversionBatchRepository) UpsertByIdentity(_ context.Context, value accountdomain.Credential) (accountdomain.Credential, bool, error) {
	value.ID = 10_000 + r.nextBuildID.Add(1)
	return value, true, nil
}

func (r *conversionBatchRepository) LinkWebToBuild(context.Context, uint64, uint64) error { return nil }

type buildConversionAdapter struct{ calls atomic.Int64 }

func (a *buildConversionAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderWeb }

func (a *buildConversionAdapter) ConvertToBuild(_ context.Context, credential accountdomain.Credential) (provider.CredentialSeed, error) {
	call := a.calls.Add(1)
	return provider.CredentialSeed{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "build", UserID: credential.SourceKey,
		SourceKey: fmt.Sprintf("converted:%s:%d", credential.SourceKey, call), OIDCClientID: "client",
		AccessToken: fmt.Sprintf("access-%d", call), RefreshToken: fmt.Sprintf("refresh-%d", call), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}, nil
}
