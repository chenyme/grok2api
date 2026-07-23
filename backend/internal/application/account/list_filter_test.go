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
		{name: "association on non-Web provider", filter: ListFilter{Provider: string(accountdomain.ProviderBuild), Association: "buildLinked"}},
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
