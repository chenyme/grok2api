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
