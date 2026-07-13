package app

import (
	"context"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestResolveConsoleRoutesUsesOriginalIDUnlessOccupied(t *testing.T) {
	lookup := publicModelLookupStub{values: map[string]modeldomain.Route{
		"grok-4.3":         {PublicID: "grok-4.3", Provider: account.ProviderBuild},
		"grok-4.3-console": {PublicID: "grok-4.3-console", Provider: account.ProviderConsole},
	}}
	routes, err := resolveConsoleRoutes(context.Background(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	byUpstream := make(map[string]string, len(routes))
	for _, route := range routes {
		byUpstream[route.UpstreamModel] = route.PublicID
	}
	if _, exists := byUpstream["grok-4.5"]; exists {
		t.Fatal("grok-4.5 must not be exposed by the Console provider")
	}
	if byUpstream["grok-4.3"] != "grok-4.3-console" {
		t.Fatalf("conflicting grok-4.3 public id = %q", byUpstream["grok-4.3"])
	}
	if byUpstream["grok-4.20-0309"] != "grok-4.20-0309" {
		t.Fatalf("non-conflicting grok-4.20 public id = %q", byUpstream["grok-4.20-0309"])
	}
}

type publicModelLookupStub struct {
	values map[string]modeldomain.Route
}

func (s publicModelLookupStub) GetByPublicIDIncludingDisabled(_ context.Context, publicID string) (modeldomain.Route, error) {
	value, ok := s.values[publicID]
	if !ok {
		return modeldomain.Route{}, repository.ErrNotFound
	}
	return value, nil
}
