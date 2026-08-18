package relational

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	repositorypkg "github.com/chenyme/grok2api/backend/internal/repository"
)

func TestEgressNodeWritesUseCurrentProxyProfileValue(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "profile-canonical.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewEgressRepository(database)
	profile, err := repository.CreateEgressProxyProfile(ctx, egress.ProxyProfile{Name: "canonical", EncryptedProxyURL: "encrypted-current"})
	if err != nil {
		t.Fatal(err)
	}

	created, err := repository.CreateEgressNode(ctx, egress.Node{
		Name: "bound", Scope: egress.ScopeBuild, Enabled: true,
		ProxyProfileID: profile.ID, EncryptedProxyURL: "encrypted-stale-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.EncryptedProxyURL != "encrypted-current" {
		t.Fatalf("created proxy = %q", created.EncryptedProxyURL)
	}

	created.EncryptedProxyURL = "encrypted-stale-update"
	updated, err := repository.UpdateEgressNode(ctx, created)
	if err != nil {
		t.Fatal(err)
	}
	if updated.EncryptedProxyURL != "encrypted-current" {
		t.Fatalf("updated proxy = %q", updated.EncryptedProxyURL)
	}

	updated.ProxyProfileID = 0
	updated.EncryptedProxyURL = "encrypted-independent"
	detached, err := repository.UpdateEgressNode(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	if detached.EncryptedProxyURL != "encrypted-independent" {
		t.Fatalf("detached proxy = %q", detached.EncryptedProxyURL)
	}
	if err := repository.DeleteEgressNode(ctx, detached.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateEgressNode(ctx, detached); !errors.Is(err, repositorypkg.ErrNotFound) {
		t.Fatalf("update deleted node error = %v", err)
	}
	var nodeCount int64
	if err := database.db.Model(&egressNodeModel{}).Where("id = ?", detached.ID).Count(&nodeCount).Error; err != nil || nodeCount != 0 {
		t.Fatalf("deleted node count = %d, %v", nodeCount, err)
	}

	_, err = repository.CreateEgressNode(ctx, egress.Node{
		Name: "missing-profile", Scope: egress.ScopeBuild, Enabled: true,
		ProxyProfileID: profile.ID + 999, EncryptedProxyURL: "encrypted-stale",
	})
	if !errors.Is(err, repositorypkg.ErrEgressProxyProfileNotFound) {
		t.Fatalf("missing profile error = %v", err)
	}
}

func TestListEgressProxyProfilesIsPagedAndSearchable(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "profile-page.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewEgressRepository(database)
	for index := 0; index < 25; index++ {
		name := fmt.Sprintf("Proxy %02d", index)
		if index >= 22 {
			name = fmt.Sprintf("Needle %02d", index)
		}
		if _, err := repository.CreateEgressProxyProfile(ctx, egress.ProxyProfile{Name: name, EncryptedProxyURL: fmt.Sprintf("encrypted-%d", index)}); err != nil {
			t.Fatal(err)
		}
	}
	page, total, err := repository.ListEgressProxyProfiles(ctx, repositorypkg.PageQuery{Offset: 10, Limit: 10})
	if err != nil || total != 25 || len(page) != 10 {
		t.Fatalf("page total=%d len=%d err=%v", total, len(page), err)
	}
	if page[0].Name != "Proxy 07" || page[0].EncryptedProxyURL != "encrypted-7" || page[0].ID == 0 || page[0].CreatedAt.IsZero() || page[0].UpdatedAt.IsZero() {
		t.Fatalf("first page item was not fully scanned: %#v", page[0])
	}
	matches, total, err := repository.ListEgressProxyProfiles(ctx, repositorypkg.PageQuery{Limit: 20, Search: " needle "})
	if err != nil || total != 3 || len(matches) != 3 {
		t.Fatalf("search total=%d len=%d err=%v", total, len(matches), err)
	}
}

func TestSharedProxyProfileUpdatesBoundNodesAndKeepsStateIsolated(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "proxy-profiles.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := NewEgressRepository(database)
	service := egressapp.NewService(repository, cipher, "browser-agent")
	firstURL := "socks5h://user:secret@proxy.example:1080"
	profile, err := service.CreateProxyProfile(ctx, egressapp.ProxyProfileInput{Name: "Tokyo", ProxyURL: &firstURL})
	if err != nil || profile.ProxyDisplay == "" || profile.ProxyFingerprint == "" {
		t.Fatalf("create profile = %#v, %v", profile, err)
	}
	profileID := profile.ID
	build, err := service.Create(ctx, egressapp.Input{Name: "build", Scope: egress.ScopeBuild, Enabled: true, ProxyProfileID: &profileID})
	if err != nil {
		t.Fatal(err)
	}
	web, err := service.Create(ctx, egressapp.Input{Name: "web", Scope: egress.ScopeWeb, Enabled: true, ProxyProfileID: &profileID})
	if err != nil {
		t.Fatal(err)
	}
	if build.ProxyProfileID != profileID || web.ProxyProfileID != profileID {
		t.Fatalf("profile bindings build=%d web=%d", build.ProxyProfileID, web.ProxyProfileID)
	}
	profiles, total, err := service.ListProxyProfiles(ctx, 1, 20, "")
	if err != nil || total != 1 || len(profiles) != 1 || profiles[0].BoundNodeCount != 2 {
		t.Fatalf("profiles = %#v, %v", profiles, err)
	}

	storedWeb, err := repository.GetEgressNode(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedWeb.Health = 0.3
	storedWeb.FailureCount = 4
	storedWeb.EncryptedCloudflareCookie, err = cipher.Encrypt("cf_clearance=old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateEgressNode(ctx, storedWeb); err != nil {
		t.Fatal(err)
	}
	secondURL := "http://new-user:new-secret@new-proxy.example:8080"
	updated, err := service.UpdateProxyProfile(ctx, profileID, egressapp.ProxyProfileInput{Name: "Tokyo primary", ProxyURL: &secondURL})
	if err != nil || updated.BoundNodeCount != 2 {
		t.Fatalf("update profile = %#v, %v", updated, err)
	}
	for _, nodeID := range []uint64{build.ID, web.ID} {
		node, getErr := repository.GetEgressNode(ctx, nodeID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		actual, decryptErr := cipher.Decrypt(node.EncryptedProxyURL)
		if decryptErr != nil || actual != secondURL {
			t.Fatalf("node %d proxy = %q, %v", nodeID, actual, decryptErr)
		}
		if node.Health != 1 || node.FailureCount != 0 || node.EncryptedCloudflareCookie != "" || node.ProbeStatus != egress.ProbeStatusUnknown {
			t.Fatalf("node %d stale state was not reset: %#v", nodeID, node)
		}
	}
	if err := service.DeleteProxyProfile(ctx, profileID); !errors.Is(err, egressapp.ErrProxyProfileInUse) {
		t.Fatalf("delete bound profile error = %v", err)
	}
}

func TestStandaloneProxyProfileMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "proxy-profile-migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewEgressRepository(database)
	node, err := repository.CreateEgressNode(ctx, egress.Node{Name: "legacy", Scope: egress.ScopeBuild, Enabled: true, EncryptedProxyURL: "encrypted-legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.db.Model(&egressOperationsConfigModel{}).Where("id = 1").Update("proxy_profile_migration_completed", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	migrated, err := repository.GetEgressNode(ctx, node.ID)
	if err != nil || migrated.ProxyProfileID == 0 || migrated.EncryptedProxyURL != "encrypted-legacy" {
		t.Fatalf("migrated node = %#v, %v", migrated, err)
	}
	profiles, total, err := repository.ListEgressProxyProfiles(ctx, repositorypkg.PageQuery{Limit: 20})
	if err != nil || total != 1 || len(profiles) != 1 || profiles[0].BoundNodeCount != 1 {
		t.Fatalf("migrated profiles = %#v, %v", profiles, err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	profiles, total, err = repository.ListEgressProxyProfiles(ctx, repositorypkg.PageQuery{Limit: 20})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles after restart = %#v, %v", profiles, err)
	}
}

func TestStandaloneProxyProfileMigrationProcessesMultipleBatches(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "proxy-profile-batches.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewEgressRepository(database)
	count := standaloneProxyProfileMigrationBatchSize*2 + 5
	values := make([]egress.Node, 0, count)
	for index := 0; index < count; index++ {
		values = append(values, egress.Node{
			Name: fmt.Sprintf("legacy-%03d", index), Scope: egress.ScopeBuild, Enabled: true,
			EncryptedProxyURL: fmt.Sprintf("encrypted-legacy-%d", index),
		})
	}
	if created, err := repository.CreateEgressNodes(ctx, values); err != nil || created != count {
		t.Fatalf("create nodes = %d, %v", created, err)
	}
	if err := database.db.Model(&egressOperationsConfigModel{}).Where("id = 1").Update("proxy_profile_migration_completed", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.migrateStandaloneEgressProxyProfiles(ctx); err != nil {
		t.Fatal(err)
	}
	var unbound int64
	if err := database.db.Model(&egressNodeModel{}).Where("proxy_profile_id IS NULL").Count(&unbound).Error; err != nil {
		t.Fatal(err)
	}
	profiles, total, err := repository.ListEgressProxyProfiles(ctx, repositorypkg.PageQuery{Limit: count})
	if err != nil || unbound != 0 || total != int64(count) || len(profiles) != count {
		t.Fatalf("unbound=%d total=%d profiles=%d err=%v", unbound, total, len(profiles), err)
	}
}
