package relational

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestPostgresModelGroupsAggregateRouteMetricsBeforeGrouping(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	database, err := OpenPostgres(ctx, dsn, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := NewAccountRepository(database)
	if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, Name: "model-group-postgres", SourceKey: "model-group-postgres",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	models := NewModelRepository(database)
	const publicID = "postgres-grouped-console-image"
	if err := models.UpsertRoutes(ctx, []model.Route{
		{PublicID: publicID, Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Origin: model.OriginCatalog, Enabled: true},
		{PublicID: publicID, Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImageEdit, Origin: model.OriginCatalog, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{"accountSupport", "lastSyncedAt"} {
		values, total, err := models.ListGroups(ctx, repository.ModelListQuery{Page: repository.PageQuery{
			Limit: 20, Search: publicID, Sort: repository.SortQuery{Field: field, Direction: repository.SortDescending},
		}})
		if err != nil {
			t.Fatalf("sort PostgreSQL model groups by %s: %v", field, err)
		}
		if total != 1 || len(values) != 1 || len(values[0].Routes) != 2 {
			t.Fatalf("PostgreSQL group sorted by %s = %#v, total=%d", field, values, total)
		}
	}
}
