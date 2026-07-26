package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRewriteAliasedModelAppliesOperationEffort(t *testing.T) {
	publicModel := "grok-4.3"
	tests := []struct {
		name      string
		operation audit.Operation
		assert    func(*testing.T, map[string]any)
	}{
		{name: "responses", operation: audit.OperationResponses, assert: func(t *testing.T, payload map[string]any) {
			reasoning, _ := payload["reasoning"].(map[string]any)
			if reasoning["effort"] != "high" {
				t.Fatalf("reasoning = %#v", reasoning)
			}
		}},
		{name: "chat", operation: audit.OperationChat, assert: func(t *testing.T, payload map[string]any) {
			if payload["reasoning_effort"] != "high" {
				t.Fatalf("reasoning_effort = %#v", payload["reasoning_effort"])
			}
		}},
		{name: "messages", operation: audit.OperationMessages, assert: func(t *testing.T, payload map[string]any) {
			config, _ := payload["output_config"].(map[string]any)
			if config["effort"] != "high" {
				t.Fatalf("output_config = %#v", config)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := rewriteAliasedModel([]byte(`{"model":"grok-4.3-high"}`), publicModel, "high", test.operation)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["model"] != publicModel {
				t.Fatalf("model = %#v", payload["model"])
			}
			test.assert(t, payload)
		})
	}
}

type aliasRouteResolver struct {
	byPublic map[string][]modeldomain.Route
	byUp     map[string]modeldomain.Route
}

func (r *aliasRouteResolver) Get(context.Context, uint64) (modeldomain.Route, error) {
	return modeldomain.Route{}, repository.ErrNotFound
}
func (r *aliasRouteResolver) GetByPublicID(context.Context, string) (modeldomain.Route, error) {
	return modeldomain.Route{}, repository.ErrNotFound
}
func (r *aliasRouteResolver) GetByPublicIDCandidates(_ context.Context, publicID string) ([]modeldomain.Route, error) {
	for _, candidate := range modeldomain.PublicIDCandidates(publicID) {
		if routes, ok := r.byPublic[candidate]; ok {
			return routes, nil
		}
	}
	if routes, ok := r.byPublic[publicID]; ok {
		return routes, nil
	}
	return nil, repository.ErrNotFound
}
func (r *aliasRouteResolver) GetByProviderUpstream(_ context.Context, providerValue account.Provider, upstreamModel string) (modeldomain.Route, error) {
	key := string(providerValue) + "/" + upstreamModel
	if route, ok := r.byUp[key]; ok {
		return route, nil
	}
	return modeldomain.Route{}, repository.ErrNotFound
}

func TestResolvePublicModelRoutesGatesEffortAliases(t *testing.T) {
	route := modeldomain.Route{
		ID: 1, PublicID: "Build/grok-4.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	}
	service := &Service{
		models: &aliasRouteResolver{
			byPublic: map[string][]modeldomain.Route{"Build/grok-4.5": {route}},
		},
		providers: provider.NewRegistry(console.NewAdapter(console.Config{}, nil, nil)),
	}

	// Base model always works.
	routes, effort, err := service.resolvePublicModelRoutes(context.Background(), "grok-4.5", false)
	if err != nil || len(routes) != 1 || effort != "" {
		t.Fatalf("base resolve = %#v, %q, %v", routes, effort, err)
	}

	// Effort alias rejected when key disabled aliases.
	if _, _, err := service.resolvePublicModelRoutes(context.Background(), "grok-4.5-low", false); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("expected not found without aliases, got %v", err)
	}

	// Effort alias accepted when enabled and only real levels work.
	routes, effort, err = service.resolvePublicModelRoutes(context.Background(), "grok-4.5-low", true)
	if err != nil || len(routes) != 1 || effort != "low" {
		t.Fatalf("alias resolve = %#v, %q, %v", routes, effort, err)
	}
	if _, _, err := service.resolvePublicModelRoutes(context.Background(), "grok-4.5-none", true); err == nil {
		t.Fatal("grok-4.5-none must be rejected: grok-4.5 cannot disable reasoning")
	}

	// Console registered effort alias also gated.
	consoleRoute := modeldomain.Route{
		ID: 2, PublicID: "Console/grok-4.3", Provider: account.ProviderConsole, UpstreamModel: "grok-4.3",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	}
	service.models = &aliasRouteResolver{
		byPublic: map[string][]modeldomain.Route{"Console/grok-4.3": {consoleRoute}},
		byUp:     map[string]modeldomain.Route{string(account.ProviderConsole) + "/grok-4.3": consoleRoute},
	}
	if _, _, err := service.resolvePublicModelRoutes(context.Background(), "grok-4.3-high", false); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("console effort alias should be gated, got %v", err)
	}
	routes, effort, err = service.resolvePublicModelRoutes(context.Background(), "grok-4.3-high", true)
	if err != nil || len(routes) != 1 || effort != "high" {
		t.Fatalf("console alias resolve = %#v, %q, %v", routes, effort, err)
	}
}
