package account

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	cliprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestExportCredentialsRoundTripsImportFormat(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "export.db"))
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
	refreshToken, err := cipher.Encrypt("refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	repository := relational.NewAccountRepository(database)
	if _, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "primary", Email: "user@example.com", UserID: "user-1",
		SourceKey: "export-test", OIDCClientID: "client-1", EncryptedAccessToken: accessToken,
		EncryptedRefreshToken: refreshToken, ExpiresAt: expiresAt, Enabled: false,
		AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 8,
	}); err != nil {
		t.Fatal(err)
	}
	adapter := cliprovider.NewAdapter(cliprovider.Config{}, cipher)
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)

	result, err := service.ExportCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	values, err := adapter.ParseImportedCredentials(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || len(values) != 1 {
		t.Fatalf("export count = %d, imported values = %d", result.Count, len(values))
	}
	value := values[0]
	if value.Name != "primary" || value.Email != "user@example.com" || value.UserID != "user-1" || value.OIDCClientID != "client-1" || value.AccessToken != "access-token" || value.RefreshToken != "refresh-token" || !value.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("round-trip credential = %#v", value)
	}
	progress := make([][2]int, 0, 2)
	if _, err := service.ImportCredentialsWithProgress(ctx, result.Data, nil, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(progress) != 2 || progress[0] != [2]int{0, 1} || progress[1] != [2]int{1, 1} {
		t.Fatalf("import progress = %#v", progress)
	}

	multiProgress := make([][2]int, 0, 3)
	multiResult, err := service.ImportCredentialDocumentsWithProgress(ctx, [][]byte{
		result.Data,
		result.Data,
		[]byte(`{"provider":"grok_build","name":"secondary","access_token":"second-access","refresh_token":"second-refresh","user_id":"user-2"}`),
	}, nil, func(completed, total int) error {
		multiProgress = append(multiProgress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if multiResult.Created != 1 || multiResult.Updated != 1 {
		t.Fatalf("multi-file import result = %#v", multiResult)
	}
	if len(multiProgress) != 3 || multiProgress[0] != [2]int{0, 2} || multiProgress[2] != [2]int{2, 2} {
		t.Fatalf("multi-file import progress = %#v", multiProgress)
	}
}

func TestExportProviderCredentialsRoundTripsSSOProviders(t *testing.T) {
	for _, test := range []struct {
		name          string
		providerValue accountdomain.Provider
		adapter       provider.Adapter
		webTier       accountdomain.WebTier
	}{
		{name: "web", providerValue: accountdomain.ProviderWeb, adapter: webprovider.NewAdapter(webprovider.Config{}, nil, nil, nil, nil), webTier: accountdomain.WebTierSuper},
		{name: "console", providerValue: accountdomain.ProviderConsole, adapter: consoleprovider.NewAdapter(consoleprovider.Config{}, nil, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			nsfwAt := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
			tosAt := nsfwAt.Add(-time.Hour)
			birthDateAt := tosAt.Add(-time.Hour)
			database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "export-sso.db"))
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
			token, err := cipher.Encrypt("sso-token")
			if err != nil {
				t.Fatal(err)
			}
			cookies, err := cipher.Encrypt("cf_clearance=clearance-token")
			if err != nil {
				t.Fatal(err)
			}
			repository := relational.NewAccountRepository(database)
			created, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
				Provider: test.providerValue, AuthType: accountdomain.AuthTypeSSO, WebTier: test.webTier,
				Name: test.name + "-account", SourceKey: test.name + "-export-test",
				EncryptedAccessToken: token, EncryptedCloudflareCookie: cookies,
				Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
				WebNSFWEnabledAt: &nsfwAt, WebTermsAcceptedAt: &tosAt,
				WebTermsAcceptedVersion: accountdomain.CurrentWebTermsVersion, WebBirthDateSetAt: &birthDateAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			// 模拟旧账号先按 SSO 来源创建，后续身份同步再补齐邮箱与 user_id；
			// 回导必须命中原账号，不能因新身份字段生成重复记录。
			created.Email = test.name + "@example.com"
			created.UserID = test.name + "-user-id"
			if _, err := repository.Update(ctx, created); err != nil {
				t.Fatal(err)
			}
			service := NewService(repository, nil, nil, nil, provider.NewRegistry(test.adapter), cipher, nil)

			result, err := service.ExportProviderCredentials(ctx, test.providerValue)
			if err != nil {
				t.Fatal(err)
			}
			codec, ok := test.adapter.(provider.CredentialCodecAdapter)
			if !ok {
				t.Fatal("adapter does not implement credential codec")
			}
			values, err := codec.ParseImportedCredentials(result.Data)
			if err != nil {
				t.Fatal(err)
			}
			if result.Count != 1 || len(values) != 1 || values[0].Provider != test.providerValue || values[0].AccessToken != "sso-token" || values[0].CloudflareCookies != "cf_clearance=clearance-token" || values[0].Email != test.name+"@example.com" || values[0].UserID != test.name+"-user-id" {
				t.Fatalf("round-trip result = %#v, values = %#v", result, values)
			}
			if test.providerValue == accountdomain.ProviderWeb && (values[0].WebTier != accountdomain.WebTierSuper || values[0].WebNSFWEnabledAt == nil || !values[0].WebNSFWEnabledAt.Equal(nsfwAt) || values[0].WebTermsAcceptedAt == nil || !values[0].WebTermsAcceptedAt.Equal(tosAt) || values[0].WebTermsAcceptedVersion != accountdomain.CurrentWebTermsVersion || values[0].WebBirthDateSetAt == nil || !values[0].WebBirthDateSetAt.Equal(birthDateAt)) {
				t.Fatalf("web metadata = %#v", values[0])
			}
			var imported ImportResult
			if test.providerValue == accountdomain.ProviderWeb {
				imported, err = service.ImportWebCredentialsWithProgress(ctx, result.Data, nil, nil)
			} else {
				imported, err = service.ImportConsoleCredentialsWithProgress(ctx, result.Data, nil, nil)
			}
			if err != nil || len(imported.AccountIDs) != 1 {
				t.Fatalf("reimport result = %#v, error = %v", imported, err)
			}
			stored, err := repository.Get(ctx, imported.AccountIDs[0])
			if err != nil || stored.Email != test.name+"@example.com" || stored.UserID != test.name+"-user-id" {
				t.Fatalf("reimported account = %#v, error = %v", stored, err)
			}
			if test.providerValue == accountdomain.ProviderWeb && (stored.WebNSFWEnabledAt == nil || !stored.WebNSFWEnabledAt.Equal(nsfwAt) || stored.WebTermsAcceptedAt == nil || !stored.WebTermsAcceptedAt.Equal(tosAt) || stored.WebTermsAcceptedVersion != accountdomain.CurrentWebTermsVersion || stored.WebBirthDateSetAt == nil || !stored.WebBirthDateSetAt.Equal(birthDateAt)) {
				t.Fatalf("reimported web metadata = %#v", stored)
			}
		})
	}
}
