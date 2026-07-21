package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolTypes(body []byte) []string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	tools, _ := payload["tools"].([]any)
	out := make([]string, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, strings.TrimSpace(stringField(tool, "type")))
	}
	return out
}

func containsType(types []string, want string) bool {
	for _, item := range types {
		if item == want {
			return true
		}
	}
	return false
}

func TestInjectBuildSearchTools_AppendsMissingNativeTools(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":"hi","tools":[{"type":"function","name":"local_tool","parameters":{"type":"object"}}]}`)
	out, injected, err := injectBuildSearchTools(body)
	if err != nil {
		t.Fatal(err)
	}
	if !injected {
		t.Fatal("expected injection")
	}
	types := toolTypes(out)
	if !containsType(types, "web_search") || !containsType(types, "x_search") {
		t.Fatalf("types=%v body=%s", types, out)
	}
	if len(types) != 3 {
		t.Fatalf("tools len = %d types=%v", len(types), types)
	}
}

func TestInjectBuildSearchTools_NoDuplicateWhenPresent(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","tools":[{"type":"web_search"},{"type":"x_search"},{"type":"function","name":"f"}]}`)
	out, injected, err := injectBuildSearchTools(body)
	if err != nil {
		t.Fatal(err)
	}
	if injected {
		t.Fatalf("should not re-inject: %s", out)
	}
	if string(out) != string(body) {
		t.Fatalf("body changed without injection")
	}
}

func TestInjectBuildSearchTools_EmptyToolsCreatesArray(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":[{"role":"user","content":"x"}]}`)
	out, injected, err := injectBuildSearchTools(body)
	if err != nil {
		t.Fatal(err)
	}
	if !injected {
		t.Fatal("expected injection")
	}
	types := toolTypes(out)
	if len(types) != 2 || !containsType(types, "web_search") || !containsType(types, "x_search") {
		t.Fatalf("types=%v", types)
	}
}

func TestInjectBuildSearchTools_WebSearchPreviewCounts(t *testing.T) {
	body := []byte(`{"tools":[{"type":"web_search_preview_2025_03_11"}]}`)
	out, injected, err := injectBuildSearchTools(body)
	if err != nil {
		t.Fatal(err)
	}
	if !injected {
		t.Fatal("expected only x_search injection")
	}
	types := toolTypes(out)
	if !containsType(types, "x_search") {
		t.Fatalf("missing x_search: %v", types)
	}
	webCount := 0
	for _, kind := range types {
		if kind == "web_search" || strings.HasPrefix(kind, "web_search_") {
			webCount++
		}
	}
	if webCount != 1 {
		t.Fatalf("web_search variants = %d types=%v", webCount, types)
	}
}
