package gateway

import (
	"encoding/json"
	"testing"
)

func TestMaybeInjectAutoWebSearch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     string
		injected bool
	}{
		{name: "empty tools", body: `{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}`, injected: true},
		{name: "null tools", body: `{"model":"grok-4.5","tools":null}`, injected: true},
		{name: "existing web_search", body: `{"tools":[{"type":"web_search"}]}`, injected: false},
		{name: "existing preview", body: `{"tools":[{"type":"web_search_preview"}]}`, injected: false},
		{name: "web_search_options", body: `{"web_search_options":{}}`, injected: false},
		{name: "tool_choice none string", body: `{"tool_choice":"none"}`, injected: false},
		{name: "tool_choice none object", body: `{"tool_choice":{"type":"none"}}`, injected: false},
		{name: "function tools append", body: `{"tools":[{"type":"function","function":{"name":"lookup"}}]}`, injected: true},
		{name: "invalid json", body: `{not-json`, injected: false},
		{name: "invalid tools type", body: `{"tools":"oops"}`, injected: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := maybeInjectAutoWebSearch([]byte(tc.body))
			if !tc.injected {
				if string(got) != tc.body {
					t.Fatalf("body changed: %s", got)
				}
				return
			}
			var payload map[string]any
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatalf("injected body is not JSON: %v", err)
			}
			tools, _ := payload["tools"].([]any)
			if !containsWebSearchTool(tools) {
				t.Fatalf("missing web_search: %s", got)
			}
		})
	}
}

func TestMaybeInjectAutoWebSearchPreservesExistingFunctions(t *testing.T) {
	t.Parallel()
	got := maybeInjectAutoWebSearch([]byte(`{"tools":[{"type":"function","function":{"name":"lookup"}}]}`))
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %#v", tools)
	}
	first, _ := tools[0].(map[string]any)
	if first["type"] != "function" {
		t.Fatalf("original function tool lost: %#v", tools)
	}
}
