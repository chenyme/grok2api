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
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
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
			thinking, _ := payload["thinking"].(map[string]any)
			if thinking["type"] != "adaptive" {
				t.Fatalf("thinking = %#v", thinking)
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

func TestRewriteAliasedMessagesPinsAndDisablesReasoningEndToEnd(t *testing.T) {
	request := []byte(`{
		"model":"grok-4.3-high",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"disabled"},
		"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}
	}`)
	rewritten, err := rewriteAliasedModel(request, "grok-4.3", "high", audit.OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	converted, options, err := conversation.ConvertRequestWithOptions(rewritten, "grok-4.3", conversation.OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || !options.AnthropicThinking {
		t.Fatalf("reasoning = %#v, options = %#v", reasoning, options)
	}
	text, _ := payload["text"].(map[string]any)
	if text["format"] == nil {
		t.Fatalf("output format was not preserved: %#v", payload)
	}

	rewritten, err = rewriteAliasedModel(rewritten, "grok-4.3", "none", audit.OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	converted, options, err = conversation.ConvertRequestWithOptions(rewritten, "grok-4.3", conversation.OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	payload = nil
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["reasoning"] != nil || options.AnthropicThinking {
		t.Fatalf("reasoning should be disabled: payload = %#v, options = %#v", payload, options)
	}
	text, _ = payload["text"].(map[string]any)
	if text["format"] == nil {
		t.Fatalf("output format was not preserved after disabling reasoning: %#v", payload)
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

func TestResolvePublicModelRoutesGatesDynamicAliasesAndPreservesCompatibility(t *testing.T) {
	route := modeldomain.Route{
		ID: 1, PublicID: "Build/grok-4.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	}
	service := &Service{
		models: &aliasRouteResolver{
			byPublic: map[string][]modeldomain.Route{"Build/grok-4.5": {route}},
		},
		providers: provider.NewRegistry(console.NewAdapter(console.Config{}, nil, nil, nil)),
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

	// Registered aliases existed before the per-key switch and remain callable for compatibility.
	consoleRoute := modeldomain.Route{
		ID: 2, PublicID: "Console/grok-4.3", Provider: account.ProviderConsole, UpstreamModel: "grok-4.3",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	}
	service.models = &aliasRouteResolver{
		byPublic: map[string][]modeldomain.Route{"Console/grok-4.3": {consoleRoute}},
		byUp:     map[string]modeldomain.Route{string(account.ProviderConsole) + "/grok-4.3": consoleRoute},
	}
	routes, effort, err = service.resolvePublicModelRoutes(context.Background(), "grok-4.3-high", false)
	if err != nil || len(routes) != 1 || effort != "high" {
		t.Fatalf("legacy console alias resolve = %#v, %q, %v", routes, effort, err)
	}

	// Newly generated aliases still require an opted-in key.
	if _, _, err := service.resolvePublicModelRoutes(context.Background(), "grok-4.3-none", false); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("dynamic console alias should be gated, got %v", err)
	}
	routes, effort, err = service.resolvePublicModelRoutes(context.Background(), "grok-4.3-none", true)
	if err != nil || len(routes) != 1 || effort != "none" {
		t.Fatalf("dynamic console alias resolve = %#v, %q, %v", routes, effort, err)
	}

	// The Console 4.20 reasoning variant reasons intrinsically but rejects
	// reasoningEffort. Keep old suffix calls routable for compatibility; the
	// Console normalizer removes the pinned effort before the upstream request.
	fixedReasoningRoute := modeldomain.Route{
		ID: 3, PublicID: "Console/grok-4.20-0309-reasoning", Provider: account.ProviderConsole,
		UpstreamModel: "grok-4.20-0309-reasoning", Capability: modeldomain.CapabilityResponses, Enabled: true,
	}
	service.models = &aliasRouteResolver{byPublic: map[string][]modeldomain.Route{
		"Console/grok-4.20-0309-reasoning": {fixedReasoningRoute},
	}}
	routes, effort, err = service.resolvePublicModelRoutes(context.Background(), "grok-4.20-0309-reasoning-low", true)
	if err != nil || len(routes) != 1 || routes[0].Provider != account.ProviderConsole || effort != "low" {
		t.Fatalf("fixed Console compatibility alias resolve = %#v, %q, %v", routes, effort, err)
	}
}
