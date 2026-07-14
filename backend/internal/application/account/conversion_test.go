package account

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
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
	second, err := service.ConvertWebAccountsToBuild(ctx, []uint64{webAccount.ID})
	if err != nil {
		t.Fatal(err)
	}
	if second.Created != 0 || second.Linked != 0 || second.Skipped != 1 || second.Failed != 0 || adapter.calls.Load() != 1 {
		t.Fatalf("second conversion = %#v, calls = %d", second, adapter.calls.Load())
	}
	linkedWeb, err := repository.Get(ctx, webAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linkedWeb.LinkedAccountID != first.BuildAccountIDs[0] || linkedWeb.LinkedProvider != accountdomain.ProviderBuild {
		t.Fatalf("linked web account = %#v", linkedWeb)
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
}

type buildConversionAdapter struct{ calls atomic.Int64 }

func (a *buildConversionAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderWeb }

func (a *buildConversionAdapter) ConvertToBuild(_ context.Context, credential accountdomain.Credential) (provider.CredentialSeed, error) {
	a.calls.Add(1)
	return provider.CredentialSeed{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "build", UserID: credential.SourceKey,
		SourceKey: "converted", OIDCClientID: "client", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().UTC().Add(time.Hour),
	}, nil
}
