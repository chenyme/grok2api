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
	probedAt := time.Now().UTC().Truncate(time.Millisecond)
	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	service.SetNodeProber(egressProbeStub{result: egress.ProbeResult{
		Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 42, ExitIP: "1.1.1.1",
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
	if stored.ProbeStatus != egress.ProbeStatusHealthy || stored.ProbeLatencyMS != 42 || stored.ExitIP != "1.1.1.1" || stored.LastProbedAt == nil {
		t.Fatalf("stored probe = %#v", stored)
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
	interval := 900
	capacity := 3
	created, err := service.CreateSource(ctx, egressapp.SubscriptionSourceInput{
		Name: "source", Scope: egress.ScopeBuild, Enabled: true, URL: &url,
		RefreshIntervalSeconds: &interval, DefaultAccountCapacity: &capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.URLConfigured || created.DefaultAccountCapacity != capacity {
		t.Fatalf("public source = %#v", created)
	}
	stored, err := nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedURL == url || strings.Contains(stored.EncryptedURL, "subscription-token") {
		t.Fatalf("subscription URL stored in plaintext: %q", stored.EncryptedURL)
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
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		Fallbacks: map[egress.Scope]egressapp.FallbackConfigInput{
			egress.ScopeBuild: {Mode: egress.FallbackModeFixed, NodeID: fixed.ID},
			egress.ScopeWeb:   {Mode: egress.FallbackModeDirect},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fallback := saved.FallbackFor(egress.ScopeBuild); fallback.Mode != egress.FallbackModeFixed || fallback.NodeID != fixed.ID {
		t.Fatalf("saved Build fallback = %#v", fallback)
	}
	if fallback := saved.FallbackFor(egress.ScopeWeb); fallback.Mode != egress.FallbackModeDirect || fallback.NodeID != 0 {
		t.Fatalf("saved Web fallback = %#v", fallback)
	}
	if fallback := saved.FallbackFor(egress.ScopeConsole); fallback.Mode != egress.FallbackModeNone || fallback.NodeID != 0 {
		t.Fatalf("default Console fallback = %#v", fallback)
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

func (stub egressProbeStub) ProbeEgressNode(context.Context, uint64) (egress.ProbeResult, error) {
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
