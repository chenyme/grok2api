package account

import (
	"context"
	"errors"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestListRejectsInvalidWebFilters(t *testing.T) {
	tests := []struct {
		name   string
		filter ListFilter
	}{
		{name: "agreement on non-Web provider", filter: ListFilter{Provider: string(accountdomain.ProviderBuild), Agreement: "nsfwEnabled"}},
		{name: "web association value on Build provider", filter: ListFilter{Provider: string(accountdomain.ProviderBuild), Association: "buildLinked"}},
		{name: "web association value on Console provider", filter: ListFilter{Provider: string(accountdomain.ProviderConsole), Association: "allLinked"}},
		{name: "webLinked on Web provider", filter: ListFilter{Provider: string(accountdomain.ProviderWeb), Association: "webLinked"}},
		{name: "association without provider", filter: ListFilter{Association: "webLinked"}},
		{name: "invalid agreement", filter: ListFilter{Provider: string(accountdomain.ProviderWeb), Agreement: "invalid"}},
		{name: "invalid association", filter: ListFilter{Provider: string(accountdomain.ProviderWeb), Association: "invalid"}},
	}

	service := &Service{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := service.List(context.Background(), 1, 20, "", test.filter); !errors.Is(err, ErrInvalidFilter) {
				t.Fatalf("List() error = %v, want %v", err, ErrInvalidFilter)
			}
		})
	}
}

// Validate provider-specific association filters: Web supports six values; Build and Console support Web links only.
func TestValidAssociationFilterPerProvider(t *testing.T) {
	web := string(accountdomain.ProviderWeb)
	build := string(accountdomain.ProviderBuild)
	console := string(accountdomain.ProviderConsole)
	tests := []struct {
		provider    string
		association string
		want        bool
	}{
		{web, "", true},
		{build, "", true},
		{"", "", true},
		{web, "buildLinked", true},
		{web, "allUnlinked", true},
		{web, "webLinked", false},
		{build, "webLinked", true},
		{build, "webUnlinked", true},
		{build, "consoleLinked", false},
		{console, "webLinked", true},
		{console, "webUnlinked", true},
		{console, "buildLinked", false},
		{"", "webLinked", false},
	}
	for _, test := range tests {
		if got := validAssociationFilter(test.provider, test.association); got != test.want {
			t.Fatalf("validAssociationFilter(%q, %q) = %v, want %v", test.provider, test.association, got, test.want)
		}
	}
}

// The egress filter carries an optional narrowing target: "node:<id>" and
// "source:<id>" both mean bound, scoped to one node or subscription source.
func TestParseEgressFilter(t *testing.T) {
	tests := []struct {
		value    string
		mode     string
		nodeID   uint64
		sourceID uint64
		ok       bool
	}{
		{value: "", mode: "", ok: true},
		{value: "bound", mode: "bound", ok: true},
		{value: "unbound", mode: "unbound", ok: true},
		{value: "node:12", mode: "bound", nodeID: 12, ok: true},
		{value: "source:7", mode: "bound", sourceID: 7, ok: true},
		{value: "node:9223372036854775807", mode: "bound", nodeID: 9223372036854775807, ok: true},
		{value: "node:0"},
		{value: "node:"},
		{value: "node:abc"},
		{value: "node:-1"},
		{value: "node:9223372036854775808"},
		{value: "source:18446744073709551615"},
		{value: "group:1"},
		{value: "bound:1"},
		{value: "node"},
	}
	for _, test := range tests {
		mode, nodeID, sourceID, ok := parseEgressFilter(test.value)
		if ok != test.ok || mode != test.mode || nodeID != test.nodeID || sourceID != test.sourceID {
			t.Fatalf("parseEgressFilter(%q) = (%q, %d, %d, %v), want (%q, %d, %d, %v)",
				test.value, mode, nodeID, sourceID, ok, test.mode, test.nodeID, test.sourceID, test.ok)
		}
	}
}

func TestListRejectsInvalidEgressFilter(t *testing.T) {
	service := &Service{}
	for _, value := range []string{"node:abc", "node:9223372036854775808", "source:18446744073709551615"} {
		if _, _, err := service.List(context.Background(), 1, 20, "", ListFilter{Egress: value}); !errors.Is(err, ErrInvalidFilter) {
			t.Fatalf("List(egress=%q) error = %v, want %v", value, err, ErrInvalidFilter)
		}
	}
}
