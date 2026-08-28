package relational

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRoutingCohortDefaultsPersistsAndSurvivesAccountUpsert(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "routing-cohort.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	legacy, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "legacy", SourceKey: "legacy", EncryptedAccessToken: "encrypted",
		AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.RoutingCohort != account.DefaultRoutingCohort {
		t.Fatalf("legacy cohort = %q", legacy.RoutingCohort)
	}
	stress, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "stress", SourceKey: "stress", EncryptedAccessToken: "encrypted",
		AuthStatus: account.AuthStatusActive, RoutingCohort: "stress",
	})
	if err != nil {
		t.Fatal(err)
	}
	stress.RoutingCohort = "stress-next"
	stress, err = accounts.Update(ctx, stress)
	if err != nil {
		t.Fatal(err)
	}
	// A later import/re-auth upsert carries an empty legacy value, but the
	// administrator-owned cohort must remain unchanged.
	reimported, created, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "stress reimport", SourceKey: "stress", EncryptedAccessToken: "rotated",
		AuthStatus: account.AuthStatusActive,
	})
	if err != nil || created {
		t.Fatalf("reimport created=%v err=%v", created, err)
	}
	if reimported.RoutingCohort != "stress-next" {
		t.Fatalf("cohort reset by upsert: %q", reimported.RoutingCohort)
	}
	if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "invalid", SourceKey: "invalid", EncryptedAccessToken: "encrypted",
		AuthStatus: account.AuthStatusActive, RoutingCohort: "INVALID",
	}); err == nil {
		t.Fatal("database accepted an invalid imported account cohort")
	}
}

func TestClientKeyRoutingCohortPersistsAndInvalidValuesFailClosed(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "client-key-cohort.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	keys := NewClientKeyRepository(database)
	value, err := keys.Create(ctx, clientkey.Key{
		Name: "legacy", Prefix: "legacy", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.RoutingCohort != account.DefaultRoutingCohort {
		t.Fatalf("legacy client-key cohort = %q", value.RoutingCohort)
	}
	value.RoutingCohort = "stress"
	value, err = keys.Update(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	value.Name = "renamed"
	value, err = keys.Update(ctx, value)
	if err != nil || value.RoutingCohort != "stress" {
		t.Fatalf("client-key cohort not preserved: value=%+v err=%v", value, err)
	}
	if err := database.db.WithContext(ctx).Model(&clientKeyModel{}).Where("id = ?", value.ID).Update("routing_cohort", "INVALID").Error; err == nil {
		t.Fatal("database accepted an invalid client-key cohort")
	}
	if _, err := keys.Create(ctx, clientkey.Key{
		Name: "invalid", Prefix: "invalid", SecretHash: strings.Repeat("b", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 1, MaxConcurrent: 1, RoutingCohort: "INVALID",
	}); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("invalid client-key cohort error = %v", err)
	}
}

func TestInitializeSchemaBackfillsLegacyRoutingCohortsToShared(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-routing-cohort.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	keys := NewClientKeyRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "legacy", SourceKey: "legacy", EncryptedAccessToken: "encrypted",
		AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := keys.Create(ctx, clientkey.Key{
		Name: "legacy", Prefix: "legacy", SecretHash: strings.Repeat("c", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.withSQLiteForeignKeysDisabled(ctx, func() error {
		for _, statement := range []string{
			"DROP INDEX IF EXISTS idx_accounts_cohort_routing",
			"DROP INDEX IF EXISTS idx_client_keys_routing_cohort",
			"DROP TRIGGER IF EXISTS trg_accounts_routing_cohort_format_insert",
			"DROP TRIGGER IF EXISTS trg_accounts_routing_cohort_format_update",
			"DROP TRIGGER IF EXISTS trg_client_keys_routing_cohort_format_insert",
			"DROP TRIGGER IF EXISTS trg_client_keys_routing_cohort_format_update",
		} {
			if err := database.db.WithContext(ctx).Exec(statement).Error; err != nil {
				return err
			}
		}
		if err := database.db.WithContext(ctx).Migrator().DropConstraint(&accountModel{}, "chk_accounts_routing_cohort"); err != nil {
			return err
		}
		if err := database.db.WithContext(ctx).Migrator().DropConstraint(&clientKeyModel{}, "chk_client_keys_routing_cohort"); err != nil {
			return err
		}
		if err := database.db.WithContext(ctx).Migrator().DropColumn(&accountModel{}, "RoutingCohort"); err != nil {
			return err
		}
		return database.db.WithContext(ctx).Migrator().DropColumn(&clientKeyModel{}, "RoutingCohort")
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	storedAccount, err := accounts.Get(ctx, credential.ID)
	if err != nil || storedAccount.RoutingCohort != account.DefaultRoutingCohort {
		t.Fatalf("migrated account = %+v, err=%v", storedAccount, err)
	}
	storedKey, err := keys.Get(ctx, key.ID)
	if err != nil || storedKey.RoutingCohort != account.DefaultRoutingCohort {
		t.Fatalf("migrated key = %+v, err=%v", storedKey, err)
	}
}

func TestInitializeSchemaBackfillsLegacyMediaJobCohortFromClientKey(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-media-job-cohort.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, Name: "legacy-media", SourceKey: "legacy-media", EncryptedAccessToken: "encrypted",
		AuthStatus: account.AuthStatusActive, RoutingCohort: "stress",
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewClientKeyRepository(database).Create(ctx, clientkey.Key{
		Name: "legacy-media", Prefix: "legacy-media", SecretHash: strings.Repeat("d", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 1, MaxConcurrent: 1, RoutingCohort: "stress",
	})
	if err != nil {
		t.Fatal(err)
	}
	route, err := NewModelRepository(database).Create(ctx, model.Route{
		PublicID: "Web/legacy-media", Provider: account.ProviderWeb, UpstreamModel: "legacy-media",
		Capability: model.CapabilityVideo, Origin: model.OriginManual, Enabled: true,
	}, []uint64{accountValue.ID})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := media.Job{
		ID: "video_legacy_media_cohort", RequestID: "request-legacy-media", ClientKeyID: key.ID, ClientKeyName: key.Name,
		RoutingCohort: "stress", AccountID: accountValue.ID, AccountName: accountValue.Name,
		Provider: string(account.ProviderWeb), Model: route.PublicID, ModelRouteID: route.ID, UpstreamModel: route.UpstreamModel,
		Operation: media.VideoOperationGenerate, Status: media.StatusQueued, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}
	jobs := NewMediaJobRepository(database)
	if err := jobs.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := database.withSQLiteForeignKeysDisabled(ctx, func() error {
		for _, statement := range []string{
			"DROP TRIGGER IF EXISTS trg_media_jobs_routing_cohort_format_insert",
			"DROP TRIGGER IF EXISTS trg_media_jobs_routing_cohort_format_update",
		} {
			if err := database.db.WithContext(ctx).Exec(statement).Error; err != nil {
				return err
			}
		}
		if err := database.db.WithContext(ctx).Migrator().DropConstraint(&mediaJobModel{}, "chk_media_jobs_routing_cohort"); err != nil {
			return err
		}
		return database.db.WithContext(ctx).Migrator().DropColumn(&mediaJobModel{}, "RoutingCohort")
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := jobs.GetMediaJob(ctx, job.ID, key.ID)
	if err != nil || stored.RoutingCohort != "stress" {
		t.Fatalf("migrated media job = %+v, err=%v", stored, err)
	}
}
