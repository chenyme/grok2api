package account

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestSyncWebAccountsToConsoleIsIdempotentAndPreservesBuildLink(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "web-console-sync.db"))
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
	cloudflareCookie := "cf_clearance=shared-clearance; __cf_bm=shared-bm"
	webAccount, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "Grok Web primary", SourceKey: "sso:" + security.HashToken(token),
		EncryptedAccessToken: encrypt(token), EncryptedCloudflareCookie: encrypt(cloudflareCookie),
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
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
	var parseCalls atomic.Int64
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(consoleSSOCodecAdapter{parseCalls: &parseCalls}), cipher, memory.NewLockStore())
	var observed []uint64
	var progress [][2]int
	first, err := service.SyncWebAccountsToConsoleWithProgress(ctx, []uint64{webAccount.ID}, func(accountID uint64) error {
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
	consoleAccount, err := accounts.Get(ctx, first.AccountIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := cipher.Decrypt(consoleAccount.EncryptedAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if consoleAccount.Provider != accountdomain.ProviderConsole || consoleAccount.Name != "Grok Console primary" || decrypted != token {
		t.Fatalf("console account = %#v, token = %q", consoleAccount, decrypted)
	}
	consoleCookie, err := cipher.Decrypt(consoleAccount.EncryptedCloudflareCookie)
	if err != nil {
		t.Fatal(err)
	}
	if consoleCookie != cloudflareCookie {
		t.Fatalf("console Cloudflare cookie = %q, want %q", consoleCookie, cloudflareCookie)
	}

	second, err := service.SyncAllWebAccountsToConsoleWithProgress(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created != 0 || second.Updated != 1 || len(second.AccountIDs) != 1 || second.AccountIDs[0] != consoleAccount.ID {
		t.Fatalf("second sync = %#v", second)
	}
	secondToken := "missing-sso-token"
	missingWeb, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "Grok Web missing", SourceKey: "sso:" + security.HashToken(secondToken),
		EncryptedAccessToken: encrypt(secondToken), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	var missingProgress [][2]int
	missing, err := service.SyncAllWebAccountsToConsoleWithStrategy(ctx, WebConsoleSyncMissing, nil, func(completed, total int) error {
		missingProgress = append(missingProgress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if missing.Created != 1 || missing.Updated != 0 || missing.Skipped != 1 || len(missing.AccountIDs) != 1 || parseCalls.Load() != 3 {
		t.Fatalf("missing-only sync = %#v, parse calls = %d", missing, parseCalls.Load())
	}
	if len(missingProgress) != 2 || missingProgress[0] != [2]int{0, 1} || missingProgress[1] != [2]int{1, 1} {
		t.Fatalf("missing-only progress = %#v", missingProgress)
	}
	selectedMissing, err := service.SyncWebAccountsToConsoleWithStrategy(ctx, []uint64{missingWeb.ID}, WebConsoleSyncMissing, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if selectedMissing.Created != 0 || selectedMissing.Updated != 0 || selectedMissing.Skipped != 1 || parseCalls.Load() != 3 {
		t.Fatalf("selected missing-only sync = %#v, parse calls = %d", selectedMissing, parseCalls.Load())
	}
	updatedWeb, err := accounts.Get(ctx, webAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedWeb.LinkedAccountID != buildAccount.ID || updatedWeb.LinkedProvider != accountdomain.ProviderBuild {
		t.Fatalf("updated web account = %#v", updatedWeb)
	}
	_, total, err := accounts.List(ctx, repository.AccountListQuery{
		Page: repository.PageQuery{Limit: 10}, Filter: repository.AccountListFilter{Provider: string(accountdomain.ProviderConsole)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("console account count = %d", total)
	}
}

func TestSyncAllWebAccountsToConsoleProcessesMoreThanLegacyLimitInBatches(t *testing.T) {
	const totalAccounts = maxWebConsoleSyncAccounts + 1
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	values := make([]accountdomain.Credential, 0, totalAccounts)
	for index := 1; index <= totalAccounts; index++ {
		token := fmt.Sprintf("sso-token-%d", index)
		encrypted, encryptErr := cipher.Encrypt(token)
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		values = append(values, accountdomain.Credential{
			ID: uint64(index), Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
			Name: fmt.Sprintf("Grok Web %d", index), SourceKey: "sso:" + security.HashToken(token),
			EncryptedAccessToken: encrypted, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		})
	}
	repository := &webConsoleBatchRepository{values: values}
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(consoleSSOCodecAdapter{}), cipher, memory.NewLockStore())
	progress := make([][2]int, 0, totalAccounts+1)
	result, err := service.SyncAllWebAccountsToConsoleWithProgress(context.Background(), nil, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != totalAccounts || result.Updated != 0 || len(result.AccountIDs) != totalAccounts {
		t.Fatalf("sync result = %#v", result)
	}
	if repository.listCalls != 2 {
		t.Fatalf("repository batches = %d", repository.listCalls)
	}
	if len(progress) != totalAccounts+1 || progress[0] != [2]int{0, totalAccounts} || progress[len(progress)-1] != [2]int{totalAccounts, totalAccounts} {
		t.Fatalf("progress first=%v last=%v count=%d", progress[0], progress[len(progress)-1], len(progress))
	}
}

type webConsoleBatchRepository struct {
	repository.AccountRepository
	values    []accountdomain.Credential
	listCalls int
	nextID    atomic.Uint64
}

func (r *webConsoleBatchRepository) ListProviderAccountBatch(_ context.Context, providerValue accountdomain.Provider, afterID uint64, limit int) ([]accountdomain.Credential, int64, error) {
	r.listCalls++
	values := make([]accountdomain.Credential, 0, limit)
	for _, value := range r.values {
		if value.Provider == providerValue && value.ID > afterID {
			values = append(values, value)
			if len(values) == limit {
				break
			}
		}
	}
	return values, int64(len(r.values)), nil
}

func (r *webConsoleBatchRepository) UpsertManyByIdentity(_ context.Context, values []accountdomain.Credential) ([]repository.AccountUpsertResult, error) {
	results := make([]repository.AccountUpsertResult, len(values))
	for index := range values {
		results[index] = repository.AccountUpsertResult{ID: 10_000 + r.nextID.Add(1), Created: true}
	}
	return results, nil
}

type consoleSSOCodecAdapter struct{ parseCalls *atomic.Int64 }

func (consoleSSOCodecAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderConsole }

func (a consoleSSOCodecAdapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	if a.parseCalls != nil {
		a.parseCalls.Add(1)
	}
	token := strings.TrimSpace(string(data))
	return []provider.CredentialSeed{{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		Name: "Grok Console " + security.HashToken(token)[:8], SourceKey: "console-sso:" + security.HashToken(token), AccessToken: token,
	}}, nil
}

func (consoleSSOCodecAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}
