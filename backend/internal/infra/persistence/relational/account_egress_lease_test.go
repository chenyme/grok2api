package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestEgressLeaseBlockRoutesOnlyMatchingActiveBindingAndUsesCAS(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	firstNode := createHealthyEgressNode(t, ctx, nodes, cipher, "lease-first", 0)
	secondNode := createHealthyEgressNode(t, ctx, nodes, cipher, "lease-second", 0)
	credential := createEgressOperationsAccount(t, ctx, accounts, "lease-account")
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{credential.ID}, &firstNode.ID, account.EgressAssignmentManual, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	until := time.Now().UTC().Add(time.Hour)
	stored, err := accounts.UpsertEgressLeaseBlock(ctx, account.EgressLeaseBlock{
		AccountID: credential.ID, NodeID: firstNode.ID, Reason: "hard_tps", Version: "lease-version-0001", CooldownUntil: until,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != "lease-version-0001" {
		t.Fatalf("stored block = %#v", stored)
	}
	candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "grok-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].EgressLeaseBlock == nil || candidates[0].EgressLeaseBlock.NodeID != firstNode.ID {
		t.Fatalf("routing candidates = %#v", candidates)
	}

	shorter, err := accounts.UpsertEgressLeaseBlock(ctx, account.EgressLeaseBlock{
		AccountID: credential.ID, NodeID: firstNode.ID, Reason: "soft_tps", Version: "lease-version-0002", CooldownUntil: until.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if shorter.Version != stored.Version || !shorter.CooldownUntil.Equal(stored.CooldownUntil) {
		t.Fatalf("shorter hold replaced stronger block: %#v", shorter)
	}
	if deleted, err := accounts.DeleteEgressLeaseBlock(ctx, credential.ID, firstNode.ID, "lease-version-stale"); err != nil || deleted {
		t.Fatalf("stale CAS delete = %v, %v", deleted, err)
	}

	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{credential.ID}, &secondNode.ID, account.EgressAssignmentManual, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	blocks, err := accounts.ListEgressLeaseBlocks(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 0 {
		t.Fatalf("rebinding left stale lease blocks: %#v", blocks)
	}
}

func TestExpiredEgressLeaseBlockIsReconciledButNotRouted(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "expired-egress-lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	node := createHealthyEgressNode(t, ctx, nodes, egressOperationsCipher(t), "lease-expired", 0)
	credential := createEgressOperationsAccount(t, ctx, accounts, "lease-expired-account")
	if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{credential.ID}, &node.ID, account.EgressAssignmentManual, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpsertEgressLeaseBlock(ctx, account.EgressLeaseBlock{
		AccountID: credential.ID, NodeID: node.ID, Reason: "hard_tps", Version: "lease-version-0001", CooldownUntil: time.Now().UTC().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	blocks, err := accounts.ListEgressLeaseBlocks(ctx, 10, nil)
	if err != nil || len(blocks) != 1 {
		t.Fatalf("reconciliation blocks = %#v, %v", blocks, err)
	}
	candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "grok-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].EgressLeaseBlock != nil {
		t.Fatalf("expired block remained routable: %#v", candidates)
	}
}

func TestEgressLeaseBlockKeysetPaginationAndInvalidPruning(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-lease-page.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	nodes := NewEgressRepository(database)
	node := createHealthyEgressNode(t, ctx, nodes, egressOperationsCipher(t), "lease-page", 0)
	base := time.Now().UTC().Add(time.Hour)
	created := make([]account.Credential, 0, 5)
	for index := range 5 {
		credential := createEgressOperationsAccount(t, ctx, accounts, "lease-page-account-"+string(rune('a'+index)))
		created = append(created, credential)
		if _, err := accounts.UpdateEgressBindings(ctx, account.ProviderBuild, []uint64{credential.ID}, &node.ID, account.EgressAssignmentManual, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if _, err := accounts.UpsertEgressLeaseBlock(ctx, account.EgressLeaseBlock{
			AccountID: credential.ID, NodeID: node.ID, Reason: "hard_tps", Version: "lease-page-version-" + string(rune('a'+index)), CooldownUntil: base.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := accounts.ListEgressLeaseBlocks(ctx, 2, nil)
	if err != nil || len(first) != 2 {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	cursor := &account.EgressLeaseBlockCursor{CooldownUntil: first[1].CooldownUntil, AccountID: first[1].AccountID, NodeID: first[1].NodeID}
	second, err := accounts.ListEgressLeaseBlocks(ctx, 2, cursor)
	if err != nil || len(second) != 2 || second[0].AccountID == first[1].AccountID {
		t.Fatalf("second page = %#v, %v", second, err)
	}
	if _, err := accounts.UpdateMany(ctx, account.ProviderBuild, []uint64{created[0].ID}, repository.AccountUpdates{Enabled: boolPointer(false)}); err != nil {
		t.Fatal(err)
	}
	remaining, err := accounts.ListEgressLeaseBlocks(ctx, 10, nil)
	if err != nil || len(remaining) != 4 {
		t.Fatalf("remaining blocks = %#v, %v", remaining, err)
	}
	reauth := created[1]
	reauth.AuthStatus = account.AuthStatusReauthRequired
	if _, err := accounts.Update(ctx, reauth); err != nil {
		t.Fatal(err)
	}
	remaining, err = accounts.ListEgressLeaseBlocks(ctx, 10, nil)
	if err != nil || len(remaining) != 3 {
		t.Fatalf("reauth cleanup blocks = %#v, %v", remaining, err)
	}

	disabled := node
	disabled.Enabled = false
	if _, err := nodes.UpdateEgressNode(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	pruned, err := accounts.PruneInvalidEgressLeaseBlocks(ctx, 1000)
	if err != nil || pruned != 3 {
		t.Fatalf("pruned = %d, %v", pruned, err)
	}
}

func boolPointer(value bool) *bool { return &value }
