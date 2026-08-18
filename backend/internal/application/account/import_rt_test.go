package account

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type rtImportAdapter struct{}

func (rtImportAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }

func (rtImportAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider:       accountdomain.ProviderBuild,
		ModelNamespace: accountdomain.ProviderBuild.ModelNamespace(),
		Credential: provider.CredentialSurface{
			AuthType: accountdomain.AuthTypeOAuth,
			Import:   true,
			Refresh:  true,
		},
	}
}

func (rtImportAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return []provider.CredentialSeed{
		{Name: "valid", SourceKey: "rt:valid", OIDCClientID: "client", RefreshToken: "valid-rt"},
		{Name: "valid duplicate", SourceKey: "rt:valid", OIDCClientID: "client", RefreshToken: "valid-rt"},
		{Name: "invalid", SourceKey: "rt:invalid", OIDCClientID: "client", RefreshToken: "invalid-rt"},
	}, nil
}

func (rtImportAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) { return nil, nil }

func (rtImportAdapter) PrepareImportedCredential(_ context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
	if seed.RefreshToken == "invalid-rt" {
		return provider.CredentialSeed{}, errors.New("invalid_grant")
	}
	seed.AccessToken = "fresh-access"
	seed.RefreshToken = "rotated-rt"
	seed.ExpiresAt = time.Now().UTC().Add(time.Hour)
	return seed, nil
}

type cancelingRTImportAdapter struct {
	rtImportAdapter
	cancel context.CancelFunc
}

func (a cancelingRTImportAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return []provider.CredentialSeed{{Name: "cancel-safe", SourceKey: "rt:cancel-safe", OIDCClientID: "client", RefreshToken: "original-rt"}}, nil
}

func (a cancelingRTImportAdapter) PrepareImportedCredential(_ context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
	seed.AccessToken = "fresh-after-cancel"
	seed.RefreshToken = "rotated-after-cancel"
	seed.ExpiresAt = time.Now().UTC().Add(time.Hour)
	a.cancel()
	return seed, nil
}

func TestImportRefreshTokensPersistsSuccessfulRotations(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "rt-import.db"))
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
	repository := relational.NewAccountRepository(database)
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(rtImportAdapter{}), cipher, nil)
	progress := make([][2]int, 0, 3)
	result, err := service.ImportCredentialsWithProgress(ctx, []byte("ignored"), nil, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.Skipped != 1 || result.Failed != 1 || len(result.AccountIDs) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(progress) != 3 || progress[0] != [2]int{0, 2} || progress[2] != [2]int{2, 2} {
		t.Fatalf("progress = %#v", progress)
	}
	values, err := repository.ListEnabled(ctx, accountdomain.ProviderBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("stored accounts = %#v", values)
	}
	accessToken, err := cipher.Decrypt(values[0].EncryptedAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	refreshToken, err := cipher.Decrypt(values[0].EncryptedRefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if accessToken != "fresh-access" || refreshToken != "rotated-rt" || values[0].RefreshDueAt == nil {
		t.Fatalf("stored credential = %#v, access=%q refresh=%q", values[0], accessToken, refreshToken)
	}
	lead := values[0].ExpiresAt.Sub(*values[0].RefreshDueAt)
	if lead < 5*time.Minute || lead > 8*time.Minute {
		t.Fatalf("refresh lead = %s", lead)
	}
}

func TestImportRefreshTokenPersistsRotationWhenRequestCancelsAfterExchange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	database, err := relational.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "rt-import-cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAccountRepository(database)
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(cancelingRTImportAdapter{cancel: cancel}), cipher, nil)
	result, err := service.ImportCredentials(ctx, []byte("ignored"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if result.Created != 1 || len(result.AccountIDs) != 1 {
		t.Fatalf("result = %#v", result)
	}
	values, err := repository.ListEnabled(context.Background(), accountdomain.ProviderBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("stored accounts = %#v", values)
	}
	refreshToken, err := cipher.Decrypt(values[0].EncryptedRefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if refreshToken != "rotated-after-cancel" {
		t.Fatalf("stored refresh token = %q", refreshToken)
	}
}

func TestImportRefreshTokenPersistsRotationBeforeProgressFailure(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "rt-import-progress.db"))
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
	repository := relational.NewAccountRepository(database)
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(cancelingRTImportAdapter{cancel: func() {}}), cipher, nil)
	progressFailure := errors.New("progress stream closed")
	progressCalls := 0
	result, err := service.ImportCredentialsWithProgress(ctx, []byte("ignored"), nil, func(_, _ int) error {
		progressCalls++
		if progressCalls > 1 {
			return progressFailure
		}
		return nil
	})
	if !errors.Is(err, progressFailure) {
		t.Fatalf("error = %v, want progress failure", err)
	}
	if result.Created != 1 || len(result.AccountIDs) != 1 {
		t.Fatalf("result = %#v", result)
	}
	values, err := repository.ListEnabled(ctx, accountdomain.ProviderBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("stored accounts = %#v", values)
	}
	refreshToken, err := cipher.Decrypt(values[0].EncryptedRefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if refreshToken != "rotated-after-cancel" {
		t.Fatalf("stored refresh token = %q", refreshToken)
	}
}
