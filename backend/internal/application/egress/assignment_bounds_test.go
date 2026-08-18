package egress

import (
	"math"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestNormalizeAndParseAutoAssignShare(t *testing.T) {
	if got := normalizeAutoAssignShare(0); got != 0 {
		t.Fatalf("zero must stay off: %v", got)
	}
	if got := normalizeAutoAssignShare(0.03); got != 0 {
		t.Fatalf("below 0.05 must stay off: %v", got)
	}
	if got := normalizeAutoAssignShare(1.2); got != 0 {
		t.Fatalf("above 1 must stay off: %v", got)
	}
	if got := normalizeAutoAssignShare(0.3); got != 0.3 {
		t.Fatalf("valid share = %v", got)
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := normalizeAutoAssignShare(value); got != 0 {
			t.Fatalf("special float %v must stay off: %v", value, got)
		}
	}
	if _, ok := parseAutoAssignShare("0"); !ok {
		t.Fatal("explicit 0 is a valid env override")
	}
	if _, ok := parseAutoAssignShare("0.03"); ok {
		t.Fatal("0.03 must be rejected so YAML/default remains")
	}
	if value, ok := parseAutoAssignShare("0.10"); !ok || value != 0.10 {
		t.Fatalf("0.10 = %v %v", value, ok)
	}
	for _, raw := range []string{"NaN", "Inf", "-Inf"} {
		if _, ok := parseAutoAssignShare(raw); ok {
			t.Fatalf("%q must be rejected so YAML/default remains", raw)
		}
	}
}

func TestCapAutomaticAssignmentShareDisabledByDefault(t *testing.T) {
	nodes := []domain.Node{{ID: 1, AccountCapacity: 0}, {ID: 2, AccountCapacity: 40}}
	accounts := makeActiveAccounts(20)
	got := capAutomaticAssignmentShare(nodes, accounts, 0)
	if got[0].AccountCapacity != 0 || got[1].AccountCapacity != 40 {
		t.Fatalf("disabled share must not rewrite capacities: %#v", got)
	}
}

func TestCapAutomaticAssignmentShareAppliesFraction(t *testing.T) {
	nodes := []domain.Node{{ID: 1, AccountCapacity: 0}, {ID: 2, AccountCapacity: 40}}
	accounts := makeActiveAccounts(100)
	got := capAutomaticAssignmentShare(nodes, accounts, 0.30)
	if got[0].AccountCapacity != 30 {
		t.Fatalf("uncapped node should absorb at most 30: %#v", got[0])
	}
	if got[1].AccountCapacity != 30 {
		t.Fatalf("configured 40 should be lowered to 30: %#v", got[1])
	}
}

func TestCapAutomaticAssignmentSharePreservesOtherProviderLoad(t *testing.T) {
	nodes := []domain.Node{{ID: 1, AccountCapacity: 0, AssignedAccountCount: 30}}
	accounts := makeActiveAccounts(100)
	got := capAutomaticAssignmentShare(nodes, accounts, 0.30)
	if got[0].AccountCapacity != 60 {
		t.Fatalf("30 reserved slots plus 30 active provider slots = %#v", got[0])
	}
}

func TestAutoAssignmentMigrationLimitPreservesHistoricalCeilingWhenOff(t *testing.T) {
	if got := autoAssignmentMigrationLimit(makeActiveAccounts(5000), 0); got != -1 {
		t.Fatalf("disabled share must leave first-pass unbounded: %d", got)
	}
	if got := autoAssignmentMigrationLimit(makeActiveAccounts(100), 0.10); got != 10 {
		t.Fatalf("10%% of 100 active accounts = %d", got)
	}
}

func TestResolveAutoAssignSharePrefersValidEnv(t *testing.T) {
	t.Setenv("GROK2API_AUTO_ASSIGN_MAX_NODE_SHARE", "0.25")
	if got := resolveAutoAssignShare("GROK2API_AUTO_ASSIGN_MAX_NODE_SHARE", 0); got != 0.25 {
		t.Fatalf("env should override disabled YAML: %v", got)
	}
	t.Setenv("GROK2API_AUTO_ASSIGN_MAX_NODE_SHARE", "nope")
	if got := resolveAutoAssignShare("GROK2API_AUTO_ASSIGN_MAX_NODE_SHARE", 0.4); got != 0.4 {
		t.Fatalf("invalid env should fall back to YAML: %v", got)
	}
}

func makeActiveAccounts(n int) []accountdomain.Credential {
	accounts := make([]accountdomain.Credential, n)
	for i := range accounts {
		accounts[i] = accountdomain.Credential{Enabled: true, AuthStatus: accountdomain.AuthStatusActive}
	}
	return accounts
}
