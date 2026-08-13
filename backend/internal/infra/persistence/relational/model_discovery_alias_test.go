package relational

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Reproduces the reported behaviour: one alias collision inside a discovery
// batch rolls back the whole transaction, so unrelated new models in the same
// batch are silently dropped and can never be discovered again.
//
// Setup mirrors a real deployment: a route was renamed at some point, which
// makes preserveModelRouteAlias register the old public ID as a compatibility
// alias. Later the upstream catalog starts returning both the old model and a
// brand new one.
func TestUpsertDiscoveredAliasCollisionDropsWholeBatch(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "alias-batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	models := NewModelRepository(database)

	// A discovered route exists under its original public ID.
	if err := models.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-4.5"}); err != nil {
		t.Fatal(err)
	}
	routes, _, err := models.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 100}})
	if err != nil {
		t.Fatal(err)
	}
	var routeID uint64
	for _, route := range routes {
		if route.UpstreamModel == "grok-4.5" {
			routeID = route.ID
		}
	}
	if routeID == 0 {
		t.Fatalf("routes = %#v", routes)
	}

	// Renaming it leaves "Build/grok-4.5" behind as a compatibility alias.
	existing, err := models.Get(ctx, routeID)
	if err != nil {
		t.Fatal(err)
	}
	existing.PublicID = "Build/build-grok-4.5"
	if _, err := models.Update(ctx, existing, nil); err != nil {
		t.Fatal(err)
	}

	// Upstream now returns the old model plus a new one. The old model maps onto
	// the alias and conflicts; the new model does not.
	err = models.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-4.5", "grok-4.6"})
	t.Logf("UpsertDiscovered error = %v", err)

	routes, _, listErr := models.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 100}})
	if listErr != nil {
		t.Fatal(listErr)
	}
	found46 := false
	for _, route := range routes {
		if route.UpstreamModel == "grok-4.6" && route.Origin == model.OriginDiscovered {
			found46 = true
		}
	}
	if !found46 {
		t.Errorf("grok-4.6 was dropped because grok-4.5 collided with an alias in the same batch; routes = %#v", routes)
	}
}
