package gateway

import (
	"encoding/json"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
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
