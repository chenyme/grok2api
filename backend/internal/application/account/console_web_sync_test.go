package account

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestSyncConsoleAccountsToWebIsIdempotentAndPreservesBuildLink(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-web-sync.db"))
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
	encrypt := func(value string) string {
		encrypted, encryptErr := cipher.Encrypt(value)
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		return encrypted
	}

	accounts := relational.NewAccountRepository(database)
	token := "shared-sso-token"
	consoleAccount, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		Name: "Grok Console primary", SourceKey: "console-sso:" + security.HashToken(token),
		EncryptedAccessToken: encrypt(token), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(webSSOCodecAdapter{}), cipher, memory.NewLockStore())
	var observed []uint64
	var progress [][2]int
	first, err := service.SyncConsoleAccountsToWebWithProgress(ctx, []uint64{consoleAccount.ID}, func(accountID uint64) error {
		observed = append(observed, accountID)
		return nil
	}, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 1 || first.Updated != 0 || len(first.AccountIDs) != 1 || len(observed) != 1 || observed[0] != first.AccountIDs[0] {
		t.Fatalf("first sync = %#v, observed = %#v", first, observed)
	}
	if len(progress) != 2 || progress[0] != [2]int{0, 1} || progress[1] != [2]int{1, 1} {
		t.Fatalf("progress = %#v", progress)
	}
	webAccount, err := accounts.Get(ctx, first.AccountIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := cipher.Decrypt(webAccount.EncryptedAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if webAccount.Provider != accountdomain.ProviderWeb || webAccount.Name != "Grok Web primary" || decrypted != token {
		t.Fatalf("web account = %#v, token = %q", webAccount, decrypted)
	}

	buildAccount, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
		Name: "build", SourceKey: "build-source", EncryptedAccessToken: encrypt("build-access"),
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.LinkWebToBuild(ctx, webAccount.ID, buildAccount.ID); err != nil {
		t.Fatal(err)
	}

	second, err := service.SyncAllConsoleAccountsToWebWithProgress(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created != 0 || second.Updated != 1 || len(second.AccountIDs) != 1 || second.AccountIDs[0] != webAccount.ID {
		t.Fatalf("second sync = %#v", second)
	}
	updatedWeb, err := accounts.Get(ctx, webAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedWeb.LinkedAccountID != buildAccount.ID || updatedWeb.LinkedProvider != accountdomain.ProviderBuild {
		t.Fatalf("updated web account = %#v", updatedWeb)
	}
	_, total, err := accounts.List(ctx, repository.AccountListQuery{
		Page: repository.PageQuery{Limit: 10}, Filter: repository.AccountListFilter{Provider: string(accountdomain.ProviderWeb)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("web account count = %d", total)
	}
}

type webSSOCodecAdapter struct{}

func (webSSOCodecAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderWeb }

func (webSSOCodecAdapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	token := strings.TrimSpace(string(data))
	return []provider.CredentialSeed{{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, WebTier: accountdomain.WebTierAuto,
		Name: "Grok Web " + security.HashToken(token)[:8], SourceKey: "sso:" + security.HashToken(token), AccessToken: token,
	}}, nil
}

func (webSSOCodecAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}
