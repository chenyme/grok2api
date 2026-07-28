package cli

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestNormalizeFunctionArgumentsCoercesSchemaIntegers(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"timeout_ms": map[string]any{"type": "integer"},
			"ratio":      map[string]any{"type": "number"},
			"items": map[string]any{
				"type":  "array",
				"items": map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}},
			},
		},
	}
	arguments := `{"timeout_ms":60000.0,"ratio":2.0,"items":[1.0,null,2.5]}`
	normalized, changed := normalizeFunctionArguments(arguments, schema)
	if !changed {
		t.Fatal("expected integer arguments to be normalized")
	}
	if normalized != `{"items":[1,null,2.5],"ratio":2.0,"timeout_ms":60000}` {
		t.Fatalf("normalized arguments = %s", normalized)
	}
}

func TestNormalizeFunctionArgumentsPreservesUnsafeValues(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"fraction": map[string]any{"type": "integer"},
		"large":    map[string]any{"type": "integer"},
	}}
	arguments := `{"fraction":1.5,"large":9007199254740992.0}`
	if normalized, changed := normalizeFunctionArguments(arguments, schema); changed || normalized != arguments {
		t.Fatalf("unsafe arguments changed: changed=%v value=%s", changed, normalized)
	}
}

func TestNormalizeFunctionArgumentsFollowsLocalRefs(t *testing.T) {
	schema := map[string]any{
		"$ref": "#/$defs/arguments",
		"$defs": map[string]any{"arguments": map[string]any{
			"type":       "object",
			"properties": map[string]any{"timeout_ms": map[string]any{"type": "integer"}},
		}},
	}
	normalized, changed := normalizeFunctionArguments(`{"timeout_ms":6e4}`, schema)
	if !changed || normalized != `{"timeout_ms":60000}` {
		t.Fatalf("referenced integer was not normalized: changed=%v value=%s", changed, normalized)
	}
}

func TestResponsesIntegerArgumentsNormalizedInJSONResponse(t *testing.T) {
	request := []byte(`{
		"model":"public",
		"tools":[{"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{"timeout_ms":{"type":"integer"}}}}]
	}`)
	_, compatibility, err := normalizeResponsesRequest(request, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil {
		t.Fatal("integer schema did not enable response compatibility")
	}
	response, err := compatibility.normalizeResponseJSON([]byte(`{
		"id":"resp_1","object":"response",
		"output":[{"type":"function_call","call_id":"call_1","name":"wait_agent","arguments":"{\"timeout_ms\":60000.0}"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(response, &payload); err != nil {
		t.Fatal(err)
	}
	call := payload["output"].([]any)[0].(map[string]any)
	if call["arguments"] != `{"timeout_ms":60000}` {
		t.Fatalf("function arguments = %q", call["arguments"])
	}
}

func TestResponsesIntegerArgumentsNormalizedInStream(t *testing.T) {
	request := []byte(`{
		"model":"public",
		"stream":true,
		"tools":[{"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{"timeout_ms":{"type":"integer"}}}}]
	}`)
	_, compatibility, err := normalizeResponsesRequest(request, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"wait_agent","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"timeout_ms\":60000"}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":".0}"}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"timeout_ms\":60000.0}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"wait_agent","arguments":"{\"timeout_ms\":60000.0}"}}`,
		``,
	}, "\n")
	body, err := io.ReadAll(compatibility.normalizeResponseStream(io.NopCloser(strings.NewReader(stream))))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, `60000.0`) {
		t.Fatalf("stream retained floating integer: %s", text)
	}
	if strings.Count(text, `response.function_call_arguments.delta`) != 2 {
		// One occurrence is the event line and one is the JSON type field.
		t.Fatalf("unexpected normalized delta count: %s", text)
	}
	if !strings.Contains(text, `{\"timeout_ms\":60000}`) {
		t.Fatalf("normalized arguments missing: %s", text)
	}
}
