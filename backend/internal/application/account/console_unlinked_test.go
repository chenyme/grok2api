package account

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestSSORejectedReasonMarksOnlyUnlinkedConsole(t *testing.T) {
	unlinked := errors.Join(provider.ErrUnauthorized, provider.ErrConsoleUnlinked)
	console := accountdomain.Credential{Provider: accountdomain.ProviderConsole}
	if got := ssoRejectedReason(console, unlinked); got != accountdomain.LastErrorConsoleUnlinked {
		t.Fatalf("console unlinked reason = %q", got)
	}
	if got := ssoRejectedReason(console, provider.ErrUnauthorized); got == accountdomain.LastErrorConsoleUnlinked {
		t.Fatal("generic 401 must keep the generic reason")
	}
	if got := ssoRejectedReason(accountdomain.Credential{Provider: accountdomain.ProviderWeb}, unlinked); got == accountdomain.LastErrorConsoleUnlinked {
		t.Fatal("non-Console providers must never get the Console marker")
	}
}

func TestAutoDeleteConsoleUnlinkedAccounts(t *testing.T) {
	ctx := context.Background()
	service, repo := newAutoCleanTestService(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	upsertConsole := func(name string) accountdomain.Credential {
		return mustUpsert(t, repo, accountdomain.Credential{
			Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO, Name: name, SourceKey: "console-" + name,
			EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		})
	}
	exists := func(id uint64) bool {
		_, err := repo.Get(ctx, id)
		return err == nil
	}

	// Switch off (default): the account is only marked invalid.
	kept := upsertConsole("kept")
	if err := service.MarkReauthRequired(ctx, kept.ID, accountdomain.LastErrorConsoleUnlinked); err != nil {
		t.Fatal(err)
	}
	if !exists(kept.ID) {
		t.Fatal("account deleted while the switch is off")
	}

	// Other reasons and other providers are never deleted.
	otherReason := upsertConsole("other-reason")
	web := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web-unlinked",
		EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})

	// Turning the switch on sweeps accounts that were marked earlier.
	service.UpdateAutoDeleteConsoleUnlinked(true)
	deadline := time.Now().Add(5 * time.Second)
	for exists(kept.ID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if exists(kept.ID) {
		t.Fatal("previously marked account was not swept")
	}

	// With the switch on, marking deletes immediately.
	fresh := upsertConsole("fresh")
	if err := service.MarkReauthRequired(ctx, fresh.ID, accountdomain.LastErrorConsoleUnlinked); err != nil {
		t.Fatal(err)
	}
	if exists(fresh.ID) {
		t.Fatal("unlinked account kept while the switch is on")
	}
	if err := service.MarkReauthRequired(ctx, otherReason.ID, "grok_console SSO credential rejected"); err != nil {
		t.Fatal(err)
	}
	if err := service.MarkReauthRequired(ctx, web.ID, accountdomain.LastErrorConsoleUnlinked); err != nil {
		t.Fatal(err)
	}
	if !exists(otherReason.ID) || !exists(web.ID) {
		t.Fatal("unrelated accounts were deleted")
	}
}

func TestCredentialFromSeedStoresConsoleCachesOnlyForConsole(t *testing.T) {
	service, _ := newAutoCleanTestService(t, time.Now())
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	service.cipher = cipher
	seed := provider.CredentialSeed{Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO, Name: "c", SourceKey: "console-sso:x", AccessToken: "sso",
		ConsoleAPIKeyID: "11111111-2222-4333-8444-555555555555", ConsoleWorkOSSession: `{"session_token":"t","xai_origin":"o","workos_id":"i"}`}
	value, err := service.credentialFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := cipher.Decrypt(value.EncryptedConsoleWorkOSSession)
	if err != nil || plain != seed.ConsoleWorkOSSession || value.ConsoleAPIKeyID != seed.ConsoleAPIKeyID {
		t.Fatalf("console caches = %q / %q, %v", value.ConsoleAPIKeyID, plain, err)
	}
	seed.Provider = accountdomain.ProviderWeb
	web, err := service.credentialFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if web.ConsoleAPIKeyID != "" || web.EncryptedConsoleWorkOSSession != "" {
		t.Fatalf("web row received console caches: %#v", web)
	}
}
