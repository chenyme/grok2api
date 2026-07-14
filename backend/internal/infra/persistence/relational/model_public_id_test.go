package relational

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

func TestModelNamespaceMigrationPreservesAliasesRouteIDsAndKeyPermissions(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	now := time.Now().UTC()
	build := modelRouteModel{
		PublicID: "grok-4.3", Provider: string(account.ProviderBuild), UpstreamModel: "grok-4.3",
		Capability: string(modeldomain.CapabilityResponses), Origin: string(modeldomain.OriginDiscovered), Enabled: true,
	}
	console := modelRouteModel{
		PublicID: "grok-4.3-console", Provider: string(account.ProviderConsole), UpstreamModel: "grok-4.3",
		Capability: string(modeldomain.CapabilityResponses), Origin: string(modeldomain.OriginCatalog), Enabled: true,
	}
	if err := database.db.WithContext(ctx).Create(&build).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&console).Error; err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{
		Name: "namespace-migration", Prefix: "namespace", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4, CreatedAt: now, UpdatedAt: now,
	}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&clientKeyModelPermission{ClientKeyID: key.ID, ModelRouteID: console.ID}).Error; err != nil {
		t.Fatal(err)
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewModelRepository(database)
	buildAfter, err := repository.GetByPublicIDIncludingDisabled(ctx, "Build/grok-4.3")
	if err != nil || buildAfter.ID != build.ID {
		t.Fatalf("build route after migration = %#v, err = %v", buildAfter, err)
	}
	legacyBuild, err := repository.GetByPublicIDIncludingDisabled(ctx, "grok-4.3")
	if err != nil || legacyBuild.ID != build.ID {
		t.Fatalf("legacy Build alias = %#v, err = %v", legacyBuild, err)
	}
	consoleAfter, err := repository.GetByPublicIDIncludingDisabled(ctx, "Console/grok-4.3-console")
	if err != nil || consoleAfter.ID != console.ID {
		t.Fatalf("console route after migration = %#v, err = %v", consoleAfter, err)
	}
	if err := repository.ReplaceProviderRoutes(ctx, account.ProviderConsole, []modeldomain.Route{{
		PublicID: "Console/grok-4.3", Provider: account.ProviderConsole, UpstreamModel: "grok-4.3",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	consoleAfter, err = repository.GetByPublicIDIncludingDisabled(ctx, "Console/grok-4.3")
	if err != nil || consoleAfter.ID != console.ID {
		t.Fatalf("canonical Console route = %#v, err = %v", consoleAfter, err)
	}
	for _, alias := range []string{"grok-4.3-console", "Console/grok-4.3-console"} {
		value, lookupErr := repository.GetByPublicIDIncludingDisabled(ctx, alias)
		if lookupErr != nil || value.ID != console.ID {
			t.Fatalf("Console alias %q = %#v, err = %v", alias, value, lookupErr)
		}
	}
	var permission clientKeyModelPermission
	if err := database.db.WithContext(ctx).Where("client_key_id = ? AND model_route_id = ?", key.ID, console.ID).First(&permission).Error; err != nil {
		t.Fatalf("client-key permission did not survive model rename: %v", err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("namespace migration is not idempotent: %v", err)
	}
}
