package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestWebNSFWMarkerPersistsAcrossAccountUpserts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	credential, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO,
		Name: "web", SourceKey: "web-nsfw", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if credential.WebNSFWEnabledAt != nil {
		t.Fatalf("new account marker = %s", credential.WebNSFWEnabledAt)
	}

	first := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	if err := repo.MarkWebNSFWEnabled(ctx, credential.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkWebTermsAccepted(ctx, credential.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkWebBirthDateSet(ctx, credential.ID, first); err != nil {
		t.Fatal(err)
	}
	marked, err := repo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if marked.WebNSFWEnabledAt == nil || !marked.WebNSFWEnabledAt.Equal(first) || marked.WebTermsAcceptedAt == nil || !marked.WebTermsAcceptedAt.Equal(first) || marked.WebBirthDateSetAt == nil || !marked.WebBirthDateSetAt.Equal(first) {
		t.Fatalf("markers nsfw=%v terms=%v birth=%v, want %s", marked.WebNSFWEnabledAt, marked.WebTermsAcceptedAt, marked.WebBirthDateSetAt, first)
	}

	if err := repo.MarkWebNSFWEnabled(ctx, credential.ID, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkWebTermsAccepted(ctx, credential.ID, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkWebBirthDateSet(ctx, credential.ID, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertManyByIdentity(ctx, []account.Credential{{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO,
		Name: "web renamed", SourceKey: "web-nsfw", EncryptedAccessToken: "encrypted-new", Enabled: true, AuthStatus: account.AuthStatusActive,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	refreshed, err := repo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.WebNSFWEnabledAt == nil || !refreshed.WebNSFWEnabledAt.Equal(first) || refreshed.WebTermsAcceptedAt == nil || !refreshed.WebTermsAcceptedAt.Equal(first) || refreshed.WebBirthDateSetAt == nil || !refreshed.WebBirthDateSetAt.Equal(first) {
		t.Fatalf("markers after upsert nsfw=%v terms=%v birth=%v, want first timestamp %s", refreshed.WebNSFWEnabledAt, refreshed.WebTermsAcceptedAt, refreshed.WebBirthDateSetAt, first)
	}
}

func TestWebNSFWMarkerRejectsNonWebAccounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	credential, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
		Name: "build", SourceKey: "build-nsfw", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkWebNSFWEnabled(ctx, credential.ID, time.Now()); err == nil {
		t.Fatal("expected non-Web marker rejection")
	}
	if err := repo.MarkWebTermsAccepted(ctx, credential.ID, time.Now()); err == nil {
		t.Fatal("expected non-Web terms marker rejection")
	}
	if err := repo.MarkWebBirthDateSet(ctx, credential.ID, time.Now()); err == nil {
		t.Fatal("expected non-Web birth marker rejection")
	}
}

func TestInitializeSchemaAddsWebNSFWMarkerColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	credential, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO,
		Name: "legacy", SourceKey: "legacy-web-nsfw", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.db.Migrator().DropColumn(&webAccountProfileModel{}, "NSFWEnabledAt"); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Migrator().DropColumn(&webAccountProfileModel{}, "TermsAcceptedAt"); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Migrator().DropColumn(&webAccountProfileModel{}, "BirthDateSetAt"); err != nil {
		t.Fatal(err)
	}
	if database.db.Migrator().HasColumn(&webAccountProfileModel{}, "NSFWEnabledAt") {
		t.Fatal("legacy schema still contains NSFW marker column")
	}
	if database.db.Migrator().HasColumn(&webAccountProfileModel{}, "TermsAcceptedAt") {
		t.Fatal("legacy schema still contains terms marker column")
	}
	if database.db.Migrator().HasColumn(&webAccountProfileModel{}, "BirthDateSetAt") {
		t.Fatal("legacy schema still contains birth marker column")
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if !database.db.Migrator().HasColumn(&webAccountProfileModel{}, "NSFWEnabledAt") {
		t.Fatal("schema migration did not add NSFW marker column")
	}
	if !database.db.Migrator().HasColumn(&webAccountProfileModel{}, "TermsAcceptedAt") {
		t.Fatal("schema migration did not add terms marker column")
	}
	if !database.db.Migrator().HasColumn(&webAccountProfileModel{}, "BirthDateSetAt") {
		t.Fatal("schema migration did not add birth marker column")
	}
	refreshed, err := repo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ID != credential.ID || refreshed.WebNSFWEnabledAt != nil || refreshed.WebTermsAcceptedAt != nil || refreshed.WebBirthDateSetAt != nil {
		t.Fatalf("migrated account = %#v", refreshed)
	}
}
