package relational

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
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

func TestEgressOperationsTestsAllNodesBeyondSelectedBatchLimit(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	ids := make([]uint64, 0, 201)
	for index := 0; index < 201; index++ {
		node := createHealthyEgressNode(t, ctx, nodes, cipher, fmt.Sprintf("probe-%03d", index), 0)
		ids = append(ids, node.ID)
	}
	service := egressapp.NewService(nodes, cipher, "test-browser", accounts)
	service.SetNodeProber(egressProbeStub{result: egress.ProbeResult{Status: egress.ProbeStatusHealthy, TestedAt: time.Now().UTC()}})

	all, err := service.TestNodes(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if all.Requested != len(ids) || all.Healthy != len(ids) || all.Unhealthy != 0 {
		t.Fatalf("test all result = %#v", all)
	}
	if _, err := service.TestNodes(ctx, ids); err == nil || !strings.Contains(err.Error(), "单次最多测试") {
		t.Fatalf("selected batch error = %v", err)
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
