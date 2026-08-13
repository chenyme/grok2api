package relational

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestEgressOperationsAutoAssignRespectsNodeCapacity(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	first := createHealthyEgressNode(t, ctx, nodes, cipher, "first", 1)
	second := createHealthyEgressNode(t, ctx, nodes, cipher, "second", 1)
	created := []account.Credential{
		createEgressOperationsAccount(t, ctx, accounts, "one"),
		createEgressOperationsAccount(t, ctx, accounts, "two"),
		createEgressOperationsAccount(t, ctx, accounts, "three"),
	}

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	result, err := service.RebalanceAccounts(ctx, true, false, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if result.Assigned != 2 || result.Unplaced != 1 || result.Rebalanced != 0 {
		t.Fatalf("rebalance result = %#v", result)
	}

	assigned := make(map[uint64]int)
	for _, value := range created {
		actual, err := accounts.Get(ctx, value.ID)
		if err != nil {
			t.Fatal(err)
		}
		if actual.EgressNodeID != 0 {
			if actual.EgressAssignmentMode != account.EgressAssignmentAuto {
				t.Fatalf("account %d assignment mode = %q", actual.ID, actual.EgressAssignmentMode)
			}
			assigned[actual.EgressNodeID]++
		}
	}
	if assigned[first.ID] != 1 || assigned[second.ID] != 1 {
		t.Fatalf("capacity assignments = %#v", assigned)
	}
}

func TestEgressOperationsAutoAssignMovesAccountOffUnhealthyNode(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	unhealthy := createHealthyEgressNode(t, ctx, nodes, cipher, "unhealthy", 0)
	healthy := createHealthyEgressNode(t, ctx, nodes, cipher, "healthy", 0)
	credential := createEgressOperationsAccount(t, ctx, accounts, "recover")
	old := time.Now().UTC().Add(-10 * time.Minute)
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{credential.ID}, &unhealthy.ID, account.EgressAssignmentAuto, old); err != nil {
		t.Fatal(err)
	}
	unhealthy.ProbeStatus = egress.ProbeStatusUnhealthy
	if _, err := nodes.UpdateEgressNode(ctx, unhealthy); err != nil {
		t.Fatal(err)
	}

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	result, err := service.RebalanceAccounts(ctx, true, false, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if result.Assigned != 0 || result.Rebalanced != 1 || result.Unplaced != 0 {
		t.Fatalf("rebalance result = %#v", result)
	}
	actual, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.EgressNodeID != healthy.ID || actual.EgressAssignmentMode != account.EgressAssignmentAuto {
		t.Fatalf("unhealthy assignment was not repaired: %#v", actual)
	}
}

func TestEgressOperationsAutoAssignRepairsExistingCapacityOverflow(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	limited := createHealthyEgressNode(t, ctx, nodes, cipher, "limited", 1)
	available := createHealthyEgressNode(t, ctx, nodes, cipher, "available", 100)
	credentials := []account.Credential{
		createEgressOperationsAccount(t, ctx, accounts, "limited-one"),
		createEgressOperationsAccount(t, ctx, accounts, "limited-two"),
		createEgressOperationsAccount(t, ctx, accounts, "available-one"),
	}
	old := time.Now().UTC().Add(-10 * time.Minute)
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{credentials[0].ID, credentials[1].ID}, &limited.ID, account.EgressAssignmentAuto, old); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{credentials[2].ID}, &available.ID, account.EgressAssignmentAuto, old); err != nil {
		t.Fatal(err)
	}

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	result, err := service.RebalanceAccounts(ctx, true, false, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if result.Assigned != 0 || result.Rebalanced != 1 || result.Unplaced != 0 {
		t.Fatalf("rebalance result = %#v", result)
	}
	loads := map[uint64]int{}
	for _, credential := range credentials {
		actual, err := accounts.Get(ctx, credential.ID)
		if err != nil {
			t.Fatal(err)
		}
		loads[actual.EgressNodeID]++
	}
	if loads[limited.ID] != 1 || loads[available.ID] != 2 {
		t.Fatalf("capacity repair loads = %#v", loads)
	}
}

func TestEgressOperationsBalanceNeverMovesManualBindings(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	first := createHealthyEgressNode(t, ctx, nodes, cipher, "first", 0)
	second := createHealthyEgressNode(t, ctx, nodes, cipher, "second", 0)
	manual := []account.Credential{
		createEgressOperationsAccount(t, ctx, accounts, "manual-one"),
		createEgressOperationsAccount(t, ctx, accounts, "manual-two"),
	}
	automatic := []account.Credential{
		createEgressOperationsAccount(t, ctx, accounts, "auto-one"),
		createEgressOperationsAccount(t, ctx, accounts, "auto-two"),
	}
	old := time.Now().UTC().Add(-10 * time.Minute)
	manualIDs := []uint64{manual[0].ID, manual[1].ID}
	automaticIDs := []uint64{automatic[0].ID, automatic[1].ID}
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, manualIDs, &first.ID, account.EgressAssignmentManual, old); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, automaticIDs, &first.ID, account.EgressAssignmentAuto, old); err != nil {
		t.Fatal(err)
	}

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	result, err := service.RebalanceAccounts(ctx, true, true, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if result.Assigned != 0 || result.Rebalanced != 2 || result.Unplaced != 0 {
		t.Fatalf("rebalance result = %#v", result)
	}
	for _, value := range manual {
		actual, err := accounts.Get(ctx, value.ID)
		if err != nil {
			t.Fatal(err)
		}
		if actual.EgressNodeID != first.ID || actual.EgressAssignmentMode != account.EgressAssignmentManual {
			t.Fatalf("manual account moved: %#v", actual)
		}
	}
	for _, value := range automatic {
		actual, err := accounts.Get(ctx, value.ID)
		if err != nil {
			t.Fatal(err)
		}
		if actual.EgressNodeID != second.ID || actual.EgressAssignmentMode != account.EgressAssignmentAuto {
			t.Fatalf("automatic account was not balanced: %#v", actual)
		}
	}
}

func TestEgressOperationsSharesWebNodeCapacityAcrossProviders(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	node := createHealthyEgressNodeForScope(t, ctx, nodes, cipher, "shared-web", egress.ScopeWeb, 1)
	web := createEgressOperationsProviderAccount(t, ctx, accounts, account.ProviderWeb, "web")
	console := createEgressOperationsProviderAccount(t, ctx, accounts, account.ProviderConsole, "console")

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	result, err := service.RebalanceAccounts(ctx, true, false, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if result.Assigned != 1 || result.Unplaced != 1 {
		t.Fatalf("rebalance result = %#v", result)
	}
	storedWeb, err := accounts.Get(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedConsole, err := accounts.Get(ctx, console.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedWeb.EgressNodeID != node.ID || storedConsole.EgressNodeID != 0 {
		t.Fatalf("shared node capacity web=%d console=%d", storedWeb.EgressNodeID, storedConsole.EgressNodeID)
	}
}

func TestEgressOperationsRejectsIncompatibleNodeScopeChangeWithBindings(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	node := createHealthyEgressNode(t, ctx, nodes, cipher, "bound-build", 0)
	credential := createEgressOperationsAccount(t, ctx, accounts, "bound-build")
	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	if _, err := service.AssignAccounts(ctx, node.ID, account.ProviderBuild, []uint64{credential.ID}, account.EgressAssignmentManual); err != nil {
		t.Fatal(err)
	}

	_, err := service.Update(ctx, node.ID, egressapp.Input{Name: node.Name, Scope: egress.ScopeWeb, Enabled: true})
	if !errors.Is(err, egressapp.ErrInvalidInput) {
		t.Fatalf("incompatible scope update error = %v", err)
	}
	stored, err := nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Scope != egress.ScopeBuild {
		t.Fatalf("persisted scope = %q", stored.Scope)
	}
}

func TestEgressOperationsAllowsCompatibleNodeScopeChangeWithBindings(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	node := createHealthyEgressNodeForScope(t, ctx, nodes, cipher, "bound-console", egress.ScopeWeb, 0)
	credential := createEgressOperationsProviderAccount(t, ctx, accounts, account.ProviderConsole, "bound-console")
	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	if _, err := service.AssignAccounts(ctx, node.ID, account.ProviderConsole, []uint64{credential.ID}, account.EgressAssignmentManual); err != nil {
		t.Fatal(err)
	}

	updated, err := service.Update(ctx, node.ID, egressapp.Input{Name: node.Name, Scope: egress.ScopeConsole, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Scope != egress.ScopeConsole {
		t.Fatalf("updated scope = %q", updated.Scope)
	}
}

func TestEgressOperationsRejectsIncompatibleSourceScopeChangeWithBindings(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	url := "https://subscription.example/proxies"
	source, err := service.CreateSource(ctx, egressapp.SubscriptionSourceInput{Name: "bound-source", Scope: egress.ScopeBuild, Enabled: true, URL: &url})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := cipher.Encrypt("http://source-node.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.UpsertEgressNodesFromSource(ctx, source.ID, []egress.Node{{
		Name: "source-node", Scope: egress.ScopeBuild, Enabled: true, SourceID: source.ID,
		SourceKey: "source-node", EncryptedProxyURL: proxy,
	}}); err != nil {
		t.Fatal(err)
	}
	listed, err := nodes.ListEgressNodes(ctx, egress.ScopeBuild, repository.SortQuery{})
	if err != nil || len(listed) != 1 {
		t.Fatalf("source nodes = %#v, err = %v", listed, err)
	}
	credential := createEgressOperationsAccount(t, ctx, accounts, "bound-source")
	if _, err := service.AssignAccounts(ctx, listed[0].ID, account.ProviderBuild, []uint64{credential.ID}, account.EgressAssignmentManual); err != nil {
		t.Fatal(err)
	}

	_, err = service.UpdateSource(ctx, source.ID, egressapp.SubscriptionSourceInput{Name: source.Name, Scope: egress.ScopeWeb, Enabled: true})
	if !errors.Is(err, egressapp.ErrInvalidInput) {
		t.Fatalf("incompatible source scope update error = %v", err)
	}
	stored, err := nodes.GetEgressSource(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Scope != egress.ScopeBuild {
		t.Fatalf("persisted source scope = %q", stored.Scope)
	}
}

func TestEgressOperationsListsSourcePagesByScopeAndSearch(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	service := egressapp.NewService(nodes, egressOperationsCipher(t), "test-browser")
	url := "https://subscription.example/proxies"
	for _, input := range []egressapp.SubscriptionSourceInput{
		{Name: "Alpha Build", Scope: egress.ScopeBuild, Enabled: true, URL: &url},
		{Name: "beta build", Scope: egress.ScopeBuild, Enabled: true, URL: &url},
		{Name: "Alpha Web", Scope: egress.ScopeWeb, Enabled: true, URL: &url},
	} {
		if _, err := service.CreateSource(ctx, input); err != nil {
			t.Fatal(err)
		}
	}

	first, total, err := service.ListSourcePage(ctx, 1, 1, "BUILD", egressapp.SourceListFilter{Scope: egress.ScopeBuild})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(first) != 1 || first[0].Name != "Alpha Build" {
		t.Fatalf("first page = %#v, total = %d", first, total)
	}
	second, total, err := service.ListSourcePage(ctx, 2, 1, "build", egressapp.SourceListFilter{Scope: egress.ScopeBuild})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(second) != 1 || second[0].Name != "beta build" {
		t.Fatalf("second page = %#v, total = %d", second, total)
	}
	web, total, err := service.ListSourcePage(ctx, 1, 100, "alpha", egressapp.SourceListFilter{Scope: egress.ScopeWeb})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(web) != 1 || web[0].Name != "Alpha Web" {
		t.Fatalf("web page = %#v, total = %d", web, total)
	}
	if _, _, err := service.ListSourcePage(ctx, 1, 20, "", egressapp.SourceListFilter{Scope: egress.Scope("invalid")}); !errors.Is(err, egressapp.ErrInvalidFilter) {
		t.Fatalf("invalid scope error = %v", err)
	}
}

func TestEgressOperationsAutoAssignSkipsCoolingFixedNode(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	cooling := createHealthyEgressNode(t, ctx, nodes, cipher, "cooling", 0)
	available := createHealthyEgressNode(t, ctx, nodes, cipher, "available", 0)
	cooldownUntil := time.Now().UTC().Add(time.Hour)
	cooling.CooldownUntil = &cooldownUntil
	if _, err := nodes.UpdateEgressNode(ctx, cooling); err != nil {
		t.Fatal(err)
	}
	credential := createEgressOperationsAccount(t, ctx, accounts, "cooldown")

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	result, err := service.RebalanceAccounts(ctx, true, false, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if result.Assigned != 1 || result.Unplaced != 0 {
		t.Fatalf("rebalance result = %#v", result)
	}
	stored, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EgressNodeID != available.ID {
		t.Fatalf("assigned node = %d, want %d (cooling node %d)", stored.EgressNodeID, available.ID, cooling.ID)
	}
}

func TestEgressOperationsAssignsManyAccountsToOneManualNode(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	node := createHealthyEgressNode(t, ctx, nodes, cipher, "manual", 0)
	first := createEgressOperationsAccount(t, ctx, accounts, "first")
	second := createEgressOperationsAccount(t, ctx, accounts, "second")

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	result, err := service.AssignAccounts(ctx, node.ID, account.ProviderBuild, []uint64{first.ID, second.ID}, account.EgressAssignmentManual)
	if err != nil {
		t.Fatal(err)
	}
	if result.Assigned != 2 {
		t.Fatalf("assigned = %#v", result)
	}
	for _, value := range []account.Credential{first, second} {
		actual, err := accounts.Get(ctx, value.ID)
		if err != nil {
			t.Fatal(err)
		}
		if actual.EgressNodeID != node.ID || actual.EgressAssignmentMode != account.EgressAssignmentManual {
			t.Fatalf("manual binding = %#v", actual)
		}
	}
}

func TestEgressOperationsBatchDeleteClearsAccountBindings(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	first := createHealthyEgressNode(t, ctx, nodes, cipher, "delete-first", 0)
	second := createHealthyEgressNode(t, ctx, nodes, cipher, "delete-second", 0)
	firstAccount := createEgressOperationsAccount(t, ctx, accounts, "delete-first-account")
	secondAccount := createEgressOperationsAccount(t, ctx, accounts, "delete-second-account")
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{firstAccount.ID}, &first.ID, account.EgressAssignmentManual, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{secondAccount.ID}, &second.ID, account.EgressAssignmentAuto, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	deleted, err := service.DeleteMany(ctx, []uint64{first.ID, second.ID, first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d", deleted)
	}
	for _, value := range []account.Credential{firstAccount, secondAccount} {
		stored, err := accounts.Get(ctx, value.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.EgressNodeID != 0 || stored.EgressAssignmentMode != "" || stored.EgressAssignedAt != nil {
			t.Fatalf("account binding not cleared: %#v", stored)
		}
	}
}

func TestEgressOperationsBatchUpdatesEnabledState(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	first := createHealthyEgressNode(t, ctx, nodes, cipher, "batch-enable-first", 0)
	second := createHealthyEgressNode(t, ctx, nodes, cipher, "batch-enable-second", 0)
	second.Enabled = false
	if _, err := nodes.UpdateEgressNode(ctx, second); err != nil {
		t.Fatal(err)
	}

	service := egressapp.NewService(nodes, cipher, "test-browser")
	updated, err := service.UpdateManyEnabled(ctx, []uint64{first.ID, second.ID, first.ID}, false)
	if err != nil || updated != 1 {
		t.Fatalf("disable updated = %d, err = %v", updated, err)
	}
	updated, err = service.UpdateManyEnabled(ctx, []uint64{first.ID, second.ID}, true)
	if err != nil || updated != 2 {
		t.Fatalf("enable updated = %d, err = %v", updated, err)
	}
	for _, id := range []uint64{first.ID, second.ID} {
		stored, err := nodes.GetEgressNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !stored.Enabled {
			t.Fatalf("node %d remained disabled", id)
		}
	}
}

func TestEgressOperationsBatchDisableRejectsFixedFallback(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	fallbackNode := createHealthyEgressNode(t, ctx, nodes, cipher, "batch-fallback", 0)
	otherNode := createHealthyEgressNode(t, ctx, nodes, cipher, "batch-other", 0)
	service := egressapp.NewService(nodes, cipher, "test-browser")
	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		Fallbacks: map[egress.Scope]egressapp.FallbackConfigInput{
			egress.ScopeBuild: {Mode: egress.FallbackModeFixed, NodeID: fallbackNode.ID},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.UpdateManyEnabled(ctx, []uint64{otherNode.ID, fallbackNode.ID}, false); err == nil {
		t.Fatal("expected fixed fallback disable to fail")
	}
	for _, id := range []uint64{fallbackNode.ID, otherNode.ID} {
		stored, err := nodes.GetEgressNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !stored.Enabled {
			t.Fatalf("node %d changed despite rejected batch", id)
		}
	}
}

func TestEgressOperationsConfigRechecksFallbackNodeInsideTransaction(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	fallbackNode := createHealthyEgressNode(t, ctx, nodes, cipher, "transaction-fallback", 0)

	if _, err := nodes.UpdateEgressNodesEnabled(ctx, []uint64{fallbackNode.ID}, false); err != nil {
		t.Fatal(err)
	}
	config := egress.DefaultOperationsConfig()
	config.Fallbacks[egress.ScopeBuild] = egress.FallbackConfig{Mode: egress.FallbackModeFixed, NodeID: fallbackNode.ID}
	config.UpdatedAt = time.Now().UTC()
	if _, err := nodes.SaveEgressOperationsConfig(ctx, config); !errors.Is(err, repository.ErrEgressFallbackInUse) {
		t.Fatalf("disabled fallback save error = %v", err)
	}
	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fallback := stored.FallbackFor(egress.ScopeBuild); fallback.Mode != egress.FallbackModeNone || fallback.NodeID != 0 {
		t.Fatalf("rejected fallback was persisted: %#v", fallback)
	}
}

func TestEgressOperationsCleanupDeletesOnlyDualStackUnhealthyNodes(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)

	source, err := nodes.CreateEgressSource(ctx, egress.SubscriptionSource{
		Name: "cleanup-source", Scope: egress.ScopeBuild, Enabled: true, RefreshIntervalSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	manual := createHealthyEgressNode(t, ctx, nodes, cipher, "cleanup-manual", 0)
	managed := createHealthyEgressNode(t, ctx, nodes, cipher, "cleanup-managed", 0)
	managed.SourceID = source.ID
	managed.SourceKey = "managed"
	if managed, err = nodes.UpdateEgressNode(ctx, managed); err != nil {
		t.Fatal(err)
	}
	v4Healthy := createHealthyEgressNode(t, ctx, nodes, cipher, "cleanup-v4-healthy", 0)
	v6Healthy := createHealthyEgressNode(t, ctx, nodes, cipher, "cleanup-v6-healthy", 0)
	untested := createHealthyEgressNode(t, ctx, nodes, cipher, "cleanup-untested", 0)

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		Fallbacks: map[egress.Scope]egressapp.FallbackConfigInput{
			egress.ScopeBuild: {Mode: egress.FallbackModeFixed, NodeID: manual.ID},
		},
	}); err != nil {
		t.Fatal(err)
	}

	setEgressProbeFamilies(t, ctx, nodes, manual, egress.ProbeStatusUnhealthy, egress.ProbeStatusUnhealthy)
	setEgressProbeFamilies(t, ctx, nodes, managed, egress.ProbeStatusUnhealthy, egress.ProbeStatusUnhealthy)
	setEgressProbeFamilies(t, ctx, nodes, v4Healthy, egress.ProbeStatusHealthy, egress.ProbeStatusUnhealthy)
	setEgressProbeFamilies(t, ctx, nodes, v6Healthy, egress.ProbeStatusUnhealthy, egress.ProbeStatusHealthy)
	setEgressProbeFamilies(t, ctx, nodes, untested, egress.ProbeStatusUnknown, egress.ProbeStatusUnknown)

	firstAccount := createEgressOperationsAccount(t, ctx, accounts, "cleanup-manual-account")
	secondAccount := createEgressOperationsAccount(t, ctx, accounts, "cleanup-managed-account")
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{firstAccount.ID}, &manual.ID, account.EgressAssignmentManual, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{secondAccount.ID}, &managed.ID, account.EgressAssignmentAuto, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	preview, err := service.PreviewUnhealthyCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Nodes != 2 || preview.BoundAccounts != 2 || preview.SubscriptionManaged != 1 {
		t.Fatalf("cleanup preview = %#v", preview)
	}
	deleted, err := service.DeleteUnhealthy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d", deleted)
	}
	for _, id := range []uint64{manual.ID, managed.ID} {
		if _, err := nodes.GetEgressNode(ctx, id); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("dual-stack unhealthy node %d still exists: %v", id, err)
		}
	}
	for _, id := range []uint64{v4Healthy.ID, v6Healthy.ID, untested.ID} {
		if _, err := nodes.GetEgressNode(ctx, id); err != nil {
			t.Fatalf("preserved node %d: %v", id, err)
		}
	}
	for _, value := range []account.Credential{firstAccount, secondAccount} {
		stored, err := accounts.Get(ctx, value.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.EgressNodeID != 0 || stored.EgressAssignmentMode != "" || stored.EgressAssignedAt != nil {
			t.Fatalf("account binding not cleared: %#v", stored)
		}
	}
	config, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fallback := config.FallbackFor(egress.ScopeBuild); fallback.Mode != egress.FallbackModeNone || fallback.NodeID != 0 {
		t.Fatalf("cleanup fallback reference = %#v", fallback)
	}
}

func TestEgressOperationsRejectsManualBindingsToDisabledOrDirectNodes(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	credential := createEgressOperationsAccount(t, ctx, accounts, "manual-validation")
	direct, err := nodes.CreateEgressNode(ctx, egress.Node{Name: "direct", Scope: egress.ScopeBuild, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	disabled := createHealthyEgressNode(t, ctx, nodes, cipher, "disabled", 0)
	disabled.Enabled = false
	if _, err := nodes.UpdateEgressNode(ctx, disabled); err != nil {
		t.Fatal(err)
	}

	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	for _, nodeID := range []uint64{direct.ID, disabled.ID} {
		if _, err := service.AssignAccounts(ctx, nodeID, account.ProviderBuild, []uint64{credential.ID}, account.EgressAssignmentManual); err == nil {
			t.Fatalf("node %d was accepted for a manual proxy binding", nodeID)
		}
	}
}

func TestEgressOperationsPersistsProbeResult(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	node := createHealthyEgressNode(t, ctx, nodes, cipher, "probe", 0)
	cooldown := time.Now().UTC().Add(time.Minute)
	if err := nodes.UpdateEgressNodeHealth(ctx, node.ID, 0.7, 1, &cooldown, egress.LastErrorTransport); err != nil {
		t.Fatal(err)
	}
	probedAt := time.Now().UTC().Truncate(time.Millisecond)
	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	service.SetNodeProber(egressProbeStub{result: egress.ProbeResult{
		Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 42, ExitIP: "1.1.1.1",
		Provider: egress.ProbeProviderCloudflare,
		IPv4:     egress.ProbeFamilyResult{Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 40, ExitIP: "1.1.1.1"},
		IPv6:     egress.ProbeFamilyResult{Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 42, ExitIP: "2606:4700:4700::1111"},
	}})

	result, err := service.TestNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != egress.ProbeStatusHealthy || result.ExitIP != "1.1.1.1" {
		t.Fatalf("probe result = %#v", result)
	}
	stored, err := nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProbeStatus != egress.ProbeStatusHealthy || stored.ProbeProvider != egress.ProbeProviderCloudflare || stored.ProbeLatencyMS != 42 || stored.ExitIP != "1.1.1.1" || stored.LastProbedAt == nil {
		t.Fatalf("stored probe = %#v", stored)
	}
	if stored.IPv4Probe.ExitIP != "1.1.1.1" || stored.IPv6Probe.ExitIP != "2606:4700:4700::1111" || stored.IPv6Probe.Status != egress.ProbeStatusHealthy {
		t.Fatalf("stored family probes = ipv4:%#v ipv6:%#v", stored.IPv4Probe, stored.IPv6Probe)
	}
	if stored.Health != 1 || stored.FailureCount != 0 || stored.CooldownUntil != nil || stored.LastError != "" {
		t.Fatalf("healthy probe did not recover transport failure: %#v", stored)
	}
	if err := nodes.UpdateEgressNodeHealth(ctx, node.ID, 0.7, 1, &cooldown, "anti-bot rejection"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.TestNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	stored, err = nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Health != 0.7 || stored.FailureCount != 1 || stored.CooldownUntil == nil || stored.LastError != "anti-bot rejection" {
		t.Fatalf("healthy probe cleared a non-transport failure: %#v", stored)
	}
	updatedConfig, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeProvider: egress.ProbeProviderIPInfo, ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
	})
	if err != nil || updatedConfig.ProbeProvider != egress.ProbeProviderIPInfo {
		t.Fatalf("updated probe provider = %#v, err=%v", updatedConfig, err)
	}
	stored, err = nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProbeProvider != egress.ProbeProviderCloudflare {
		t.Fatalf("stored result provider changed with future probe configuration: %q", stored.ProbeProvider)
	}
}

func TestEgressOperationsDiscardsProbeAfterProxyConfigurationChanges(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	node := createHealthyEgressNode(t, ctx, nodes, cipher, "probe-stale", 0)
	replacementProxy, err := cipher.Encrypt("http://replacement.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	probedAt := time.Now().UTC().Truncate(time.Millisecond)
	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	service.SetNodeProber(mutatingEgressProbeStub{
		repository:  nodes,
		replacement: replacementProxy,
		result: egress.ProbeResult{
			Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 10, ExitIP: "198.51.100.20", Provider: egress.ProbeProviderCloudflare,
			IPv4: egress.ProbeFamilyResult{Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 10, ExitIP: "198.51.100.20"},
			IPv6: egress.ProbeFamilyResult{Status: egress.ProbeStatusUnhealthy, TestedAt: probedAt, LatencyMS: 10, Error: "代理连接失败"},
		},
	})

	_, err = service.TestNode(ctx, node.ID)
	if !errors.Is(err, egressapp.ErrProbeStale) {
		t.Fatalf("stale probe error = %v", err)
	}
	stored, err := nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedProxyURL != replacementProxy || stored.ProbeStatus != egress.ProbeStatusUnknown || stored.ProbeProvider != "" || stored.IPv4Probe.Status != egress.ProbeStatusUnknown {
		t.Fatalf("stale probe overwrote edited node: %#v", stored)
	}
}

func TestEgressOperationsReturnsPersistedUnhealthyProbeAsResult(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	node := createHealthyEgressNode(t, ctx, nodes, cipher, "unreachable", 0)
	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	service.SetNodeProber(egressProbeStub{err: errors.New("connection refused")})

	result, err := service.TestNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != egress.ProbeStatusUnhealthy || result.Error == "" {
		t.Fatalf("failed probe result = %#v", result)
	}
	stored, err := nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProbeStatus != egress.ProbeStatusUnhealthy || stored.ProbeError == "" || stored.LastProbedAt == nil {
		t.Fatalf("stored failed probe = %#v", stored)
	}
}

func TestEgressOperationsStoresSubscriptionURLEncrypted(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	url := "https://subscription.example/proxies?token=subscription-token"
	proxyURL := "socks5h://proxy-user:proxy-secret@proxy.example:1080"
	interval := 900
	capacity := 3
	created, err := service.CreateSource(ctx, egressapp.SubscriptionSourceInput{
		Name: "source", Scope: egress.ScopeBuild, Enabled: true, URL: &url, ProxyURL: &proxyURL,
		RefreshIntervalSeconds: &interval, DefaultAccountCapacity: &capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.URLConfigured || !created.ProxyConfigured || created.DefaultAccountCapacity != capacity {
		t.Fatalf("public source = %#v", created)
	}
	stored, err := nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedURL == url || strings.Contains(stored.EncryptedURL, "subscription-token") {
		t.Fatalf("subscription URL stored in plaintext: %q", stored.EncryptedURL)
	}
	if stored.EncryptedProxyURL == proxyURL || strings.Contains(stored.EncryptedProxyURL, "proxy-secret") {
		t.Fatalf("subscription proxy URL stored in plaintext: %q", stored.EncryptedProxyURL)
	}
	originalEncryptedProxyURL := stored.EncryptedProxyURL

	updated, err := service.UpdateSource(ctx, created.ID, egressapp.SubscriptionSourceInput{
		Name: created.Name, Scope: created.Scope, Enabled: created.Enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.ProxyConfigured {
		t.Fatal("omitted proxy update cleared the configured proxy")
	}
	stored, err = nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedProxyURL != originalEncryptedProxyURL {
		t.Fatal("omitted proxy update replaced the encrypted proxy")
	}

	replacementProxyURL := "http://replacement-user:replacement-secret@proxy.example:8080"
	updated, err = service.UpdateSource(ctx, created.ID, egressapp.SubscriptionSourceInput{
		Name: created.Name, Scope: created.Scope, Enabled: created.Enabled, ProxyURL: &replacementProxyURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err = nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	decryptedProxyURL, err := cipher.Decrypt(stored.EncryptedProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if decryptedProxyURL != replacementProxyURL || !updated.ProxyConfigured {
		t.Fatalf("replacement proxy = %q, public source = %#v", decryptedProxyURL, updated)
	}

	for _, invalidProxyURL := range []string{"", "socks5h://Default.{account}:secret@proxy.example:1080"} {
		_, updateErr := service.UpdateSource(ctx, created.ID, egressapp.SubscriptionSourceInput{
			Name: created.Name, Scope: created.Scope, Enabled: created.Enabled, ProxyURL: &invalidProxyURL,
		})
		if !errors.Is(updateErr, egressapp.ErrInvalidInput) {
			t.Fatalf("invalid proxy %q error = %v", invalidProxyURL, updateErr)
		}
	}
	stored, err = nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedProxyURL == "" {
		t.Fatal("invalid proxy update modified the configured proxy")
	}

	updated, err = service.UpdateSource(ctx, created.ID, egressapp.SubscriptionSourceInput{
		Name: created.Name, Scope: created.Scope, Enabled: created.Enabled, ClearProxyURL: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ProxyConfigured {
		t.Fatal("cleared subscription proxy remains publicly configured")
	}
	stored, err = nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedProxyURL != "" {
		t.Fatal("cleared subscription proxy remains encrypted at rest")
	}
}

func TestEgressOperationsSubscriptionImportCountsOnlyNewNodes(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	source, err := nodes.CreateEgressSource(ctx, egress.SubscriptionSource{
		Name: "count-source", Scope: egress.ScopeBuild, Enabled: true, EncryptedURL: "encrypted",
		RefreshIntervalSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := cipher.Encrypt("http://count-source.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	values := []egress.Node{{
		Name: "count-node", Scope: egress.ScopeBuild, Enabled: true, SourceID: source.ID,
		SourceKey: "count-node", EncryptedProxyURL: proxy,
	}}
	firstValues := append(append([]egress.Node(nil), values...), values[0])
	first, err := nodes.UpsertEgressNodesFromSource(ctx, source.ID, firstValues)
	if err != nil {
		t.Fatal(err)
	}
	second, err := nodes.UpsertEgressNodesFromSource(ctx, source.ID, values)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 0 {
		t.Fatalf("import counts = first %d, second %d", first, second)
	}
}

func TestEgressOperationsMaintenanceRetriesAssignmentAfterFailure(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := &retryingAssignmentRepository{AccountRepository: NewAccountRepository(database), failNext: true}
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	if _, err := nodes.SaveEgressOperationsConfig(ctx, egress.OperationsConfig{
		ProbeIntervalSeconds: 900, AutoAssignEnabled: true, AssignmentIntervalSeconds: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)

	if err := service.RunMaintenance(ctx); err == nil {
		t.Fatal("first maintenance run unexpectedly succeeded")
	}
	firstCalls := accounts.assignmentCalls
	if firstCalls != 1 {
		t.Fatalf("first assignment calls = %d", firstCalls)
	}
	if err := service.RunMaintenance(ctx); err != nil {
		t.Fatalf("second maintenance run = %v", err)
	}
	if accounts.assignmentCalls <= firstCalls {
		t.Fatalf("assignment was not retried: calls = %d", accounts.assignmentCalls)
	}
}

func TestEgressOperationsConfigPersistsFixedFallback(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	fixed := createHealthyEgressNode(t, ctx, nodes, cipher, "fixed-fallback", 0)
	service := egressapp.NewService(nodes, cipher, "test-browser")

	saved, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeProvider: egress.ProbeProviderCloudflare, ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		Fallbacks: map[egress.Scope]egressapp.FallbackConfigInput{
			egress.ScopeBuild:        {Mode: egress.FallbackModeFixed, NodeID: fixed.ID},
			egress.ScopeWeb:          {Mode: egress.FallbackModeDirect},
			egress.ScopeConsoleAsset: {Mode: egress.FallbackModeDirect},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fallback := saved.FallbackFor(egress.ScopeBuild); fallback.Mode != egress.FallbackModeFixed || fallback.NodeID != fixed.ID {
		t.Fatalf("saved Build fallback = %#v", fallback)
	}
	if saved.ProbeProvider != egress.ProbeProviderCloudflare {
		t.Fatalf("saved probe provider = %q", saved.ProbeProvider)
	}
	if fallback := saved.FallbackFor(egress.ScopeWeb); fallback.Mode != egress.FallbackModeDirect || fallback.NodeID != 0 {
		t.Fatalf("saved Web fallback = %#v", fallback)
	}
	if fallback := saved.FallbackFor(egress.ScopeConsole); fallback.Mode != egress.FallbackModeNone || fallback.NodeID != 0 {
		t.Fatalf("default Console fallback = %#v", fallback)
	}
	if fallback := saved.FallbackFor(egress.ScopeConsoleAsset); fallback.Mode != egress.FallbackModeDirect || fallback.NodeID != 0 {
		t.Fatalf("saved Console asset fallback = %#v", fallback)
	}

	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fallback := stored.FallbackFor(egress.ScopeBuild); fallback.Mode != egress.FallbackModeFixed || fallback.NodeID != fixed.ID {
		t.Fatalf("stored Build fallback = %#v", fallback)
	}
	if fallback := stored.FallbackFor(egress.ScopeWeb); fallback.Mode != egress.FallbackModeDirect || fallback.NodeID != 0 {
		t.Fatalf("stored Web fallback = %#v", fallback)
	}
	if fallback := stored.FallbackFor(egress.ScopeConsoleAsset); fallback.Mode != egress.FallbackModeDirect || fallback.NodeID != 0 {
		t.Fatalf("stored Console asset fallback = %#v", fallback)
	}
	if stored.ProbeProvider != egress.ProbeProviderCloudflare {
		t.Fatalf("stored probe provider = %q", stored.ProbeProvider)
	}
	updated, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		Fallbacks: map[egress.Scope]egressapp.FallbackConfigInput{
			egress.ScopeBuild: {Mode: egress.FallbackModeNone},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fallback := updated.FallbackFor(egress.ScopeWeb); fallback.Mode != egress.FallbackModeDirect {
		t.Fatalf("sparse update reset Web fallback = %#v", fallback)
	}
	if fallback := updated.FallbackFor(egress.ScopeConsoleAsset); fallback.Mode != egress.FallbackModeDirect {
		t.Fatalf("sparse update reset Console asset fallback = %#v", fallback)
	}
}

func TestFixedFallbackReferenceIsProtectedAndClearedOnDelete(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	fixed := createHealthyEgressNode(t, ctx, nodes, cipher, "fixed-fallback", 0)
	service := egressapp.NewService(nodes, cipher, "test-browser")
	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		Fallbacks: map[egress.Scope]egressapp.FallbackConfigInput{
			egress.ScopeBuild: {Mode: egress.FallbackModeFixed, NodeID: fixed.ID},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(ctx, fixed.ID, egressapp.Input{
		Name: fixed.Name, Scope: fixed.Scope, Enabled: false,
	}); !errors.Is(err, egressapp.ErrInvalidInput) || !strings.Contains(err.Error(), "固定回退") {
		t.Fatalf("disable fixed fallback error = %v", err)
	}
	if err := service.Delete(ctx, fixed.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fallback := stored.FallbackFor(egress.ScopeBuild); fallback.Mode != egress.FallbackModeNone || fallback.NodeID != 0 {
		t.Fatalf("deleted fallback reference = %#v", fallback)
	}
}

func TestSubscriptionSyncClearsStaleFixedFallbackReference(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	source, err := nodes.CreateEgressSource(ctx, egress.SubscriptionSource{
		Name: "source", Scope: egress.ScopeBuild, Enabled: true, RefreshIntervalSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("http://subscription.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.UpsertEgressNodesFromSource(ctx, source.ID, []egress.Node{{
		Name: "subscription", Scope: egress.ScopeBuild, Enabled: true, SourceID: source.ID,
		SourceKey: "one", EncryptedProxyURL: encryptedProxy, Health: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	listed, err := nodes.ListEgressNodes(ctx, egress.ScopeBuild, repository.SortQuery{})
	if err != nil || len(listed) != 1 {
		t.Fatalf("subscription nodes=%#v err=%v", listed, err)
	}
	service := egressapp.NewService(nodes, cipher, "test-browser")
	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		Fallbacks: map[egress.Scope]egressapp.FallbackConfigInput{
			egress.ScopeBuild: {Mode: egress.FallbackModeFixed, NodeID: listed[0].ID},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.UpsertEgressNodesFromSource(ctx, source.ID, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fallback := stored.FallbackFor(egress.ScopeBuild); fallback.Mode != egress.FallbackModeNone || fallback.NodeID != 0 {
		t.Fatalf("stale subscription fallback reference = %#v", fallback)
	}
}

func TestEgressOperationsConfigRejectsUnsafeFixedFallback(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	pool := createHealthyEgressNode(t, ctx, nodes, cipher, "pool-fallback", 0)
	pool.ProxyPool = true
	if _, err := nodes.UpdateEgressNode(ctx, pool); err != nil {
		t.Fatal(err)
	}
	service := egressapp.NewService(nodes, cipher, "test-browser")
	_, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		Fallbacks: map[egress.Scope]egressapp.FallbackConfigInput{
			egress.ScopeBuild: {Mode: egress.FallbackModeFixed, NodeID: pool.ID},
		},
	})
	if !errors.Is(err, egressapp.ErrInvalidInput) || !strings.Contains(err.Error(), "代理池") {
		t.Fatalf("pool fallback error = %v", err)
	}
}

type retryingAssignmentRepository struct {
	*AccountRepository
	failNext        bool
	assignmentCalls int
}

func (r *retryingAssignmentRepository) ListEgressAssignments(ctx context.Context, provider account.Provider) ([]account.Credential, error) {
	r.assignmentCalls++
	if r.failNext {
		r.failNext = false
		return nil, errors.New("temporary assignment failure")
	}
	return r.AccountRepository.ListEgressAssignments(ctx, provider)
}

type egressProbeStub struct {
	result egress.ProbeResult
	err    error
}

type mutatingEgressProbeStub struct {
	repository  *EgressRepository
	replacement string
	result      egress.ProbeResult
}

func (stub mutatingEgressProbeStub) ProbeEgressNode(ctx context.Context, node egress.Node) (egress.ProbeResult, error) {
	node.EncryptedProxyURL = stub.replacement
	node.ProbeStatus = egress.ProbeStatusUnknown
	node.LastProbedAt = nil
	node.ProbeLatencyMS = 0
	node.ExitIP = ""
	node.ProbeError = ""
	node.ProbeProvider = ""
	node.IPv4Probe = egress.ProbeFamilyResult{Status: egress.ProbeStatusUnknown}
	node.IPv6Probe = egress.ProbeFamilyResult{Status: egress.ProbeStatusUnknown}
	if _, err := stub.repository.UpdateEgressNode(ctx, node); err != nil {
		return egress.ProbeResult{}, err
	}
	return stub.result, nil
}

func (stub egressProbeStub) ProbeEgressNode(context.Context, egress.Node) (egress.ProbeResult, error) {
	return stub.result, stub.err
}

func egressOperationsCipher(t *testing.T) *security.Cipher {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func createHealthyEgressNode(t *testing.T, ctx context.Context, repository *EgressRepository, cipher *security.Cipher, name string, capacity int) egress.Node {
	return createHealthyEgressNodeForScope(t, ctx, repository, cipher, name, egress.ScopeBuild, capacity)
}

func createHealthyEgressNodeForScope(t *testing.T, ctx context.Context, repository *EgressRepository, cipher *security.Cipher, name string, scope egress.Scope, capacity int) egress.Node {
	t.Helper()
	proxy, err := cipher.Encrypt("http://" + name + ".example:8080")
	if err != nil {
		t.Fatal(err)
	}
	probedAt := time.Now().UTC()
	created, err := repository.CreateEgressNode(ctx, egress.Node{
		Name: name, Scope: scope, Enabled: true, EncryptedProxyURL: proxy, AccountCapacity: capacity,
		Health: 1, ProbeStatus: egress.ProbeStatusHealthy, LastProbedAt: &probedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func setEgressProbeFamilies(t *testing.T, ctx context.Context, repository *EgressRepository, node egress.Node, ipv4, ipv6 egress.ProbeStatus) {
	t.Helper()
	now := time.Now().UTC()
	if err := repository.UpdateEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL, egress.ProbeResult{
		Status: egress.ProbeStatusUnhealthy, TestedAt: now, Error: "probe failed",
		IPv4: egress.ProbeFamilyResult{Status: ipv4, TestedAt: now},
		IPv6: egress.ProbeFamilyResult{Status: ipv6, TestedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
}

func createEgressOperationsAccount(t *testing.T, ctx context.Context, repository *AccountRepository, sourceKey string) account.Credential {
	return createEgressOperationsProviderAccount(t, ctx, repository, account.ProviderBuild, sourceKey)
}

func createEgressOperationsProviderAccount(t *testing.T, ctx context.Context, repository *AccountRepository, provider account.Provider, sourceKey string) account.Credential {
	t.Helper()
	authType := account.AuthTypeOAuth
	if provider != account.ProviderBuild {
		authType = account.AuthTypeSSO
	}
	created, _, err := repository.UpsertByIdentity(ctx, account.Credential{
		Provider: provider, AuthType: authType, Name: sourceKey, SourceKey: sourceKey,
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}
