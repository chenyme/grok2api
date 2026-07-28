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

func TestNormalizeIntegralNumberUsesExactBoundedDecimalArithmetic(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		expected     string
		shouldChange bool
	}{
		{name: "decimal", input: "60000.0", expected: "60000", shouldChange: true},
		{name: "exponent", input: "6e4", expected: "60000", shouldChange: true},
		{name: "negative exponent integer", input: "1000e-3", expected: "1", shouldChange: true},
		{name: "fraction with exponent", input: "1.2300e2", expected: "123", shouldChange: true},
		{name: "negative zero", input: "-0.0", expected: "0", shouldChange: true},
		{name: "positive exact limit", input: "9007199254740991.0", expected: "9007199254740991", shouldChange: true},
		{name: "negative exact limit", input: "-9007199254740991.0", expected: "-9007199254740991", shouldChange: true},
		{name: "fraction", input: "1e-1", expected: "1e-1", shouldChange: false},
		{name: "rounded fraction", input: "9007199254740990.5", expected: "9007199254740990.5", shouldChange: false},
		{name: "outside exact limit", input: "9007199254740992.0", expected: "9007199254740992.0", shouldChange: false},
		{name: "huge exponent", input: "1e1000000000", expected: "1e1000000000", shouldChange: false},
		{name: "zero huge exponent", input: "0e1000000000", expected: "0", shouldChange: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, changed := normalizeIntegralNumber(json.Number(test.input))
			if changed != test.shouldChange || normalized.String() != test.expected {
				t.Fatalf("normalizeIntegralNumber(%q) = (%q, %v), want (%q, %v)", test.input, normalized, changed, test.expected, test.shouldChange)
			}
		})
	}
}

func TestNormalizeFunctionArgumentsPreservesUnsafeValues(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"fraction":        map[string]any{"type": "integer"},
		"roundedFraction": map[string]any{"type": "integer"},
		"large":           map[string]any{"type": "integer"},
		"hugeExponent":    map[string]any{"type": "integer"},
	}}
	arguments := `{"fraction":1.5,"roundedFraction":9007199254740990.5,"large":9007199254740992.0,"hugeExponent":1e1000000000}`
	if normalized, changed := normalizeFunctionArguments(arguments, schema); changed || normalized != arguments {
		t.Fatalf("unsafe arguments changed: changed=%v value=%s", changed, normalized)
	}
}

func TestSchemaContainsIntegerOnlyFollowsReachableConstraints(t *testing.T) {
	unreferenced := map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
		"$defs":      map[string]any{"unused": map[string]any{"type": "integer"}},
	}
	if schemaContainsInteger(unreferenced) {
		t.Fatal("unreferenced integer definition enabled argument normalization")
	}
	referenced := map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"$ref": "#/$defs/count"}},
		"$defs":      map[string]any{"count": map[string]any{"type": "integer"}},
	}
	if !schemaContainsInteger(referenced) {
		t.Fatal("referenced integer definition was not detected")
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

func TestResponsesIntegerArgumentsParallelStreamSequenceIsMonotonic(t *testing.T) {
	request := []byte(`{
		"model":"public",
		"stream":true,
		"parallel_tool_calls":true,
		"tools":[{"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{"timeout_ms":{"type":"integer"}}}}]
	}`)
	_, compatibility, err := normalizeResponsesRequest(request, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","sequence_number":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"wait_agent","arguments":""}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","sequence_number":1,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"wait_agent","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":"fc_1","delta":"{\"timeout_ms\":1.0}"}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","sequence_number":3,"item_id":"fc_2","delta":"{\"timeout_ms\":2.0}"}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","sequence_number":4,"item_id":"fc_1","arguments":"{\"timeout_ms\":1.0}"}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","sequence_number":5,"item_id":"fc_2","arguments":"{\"timeout_ms\":2.0}"}`,
		``,
	}, "\n")
	body, err := io.ReadAll(compatibility.normalizeResponseStream(io.NopCloser(strings.NewReader(stream))))
	if err != nil {
		t.Fatal(err)
	}
	lastSequence := int64(-1)
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatal(err)
		}
		sequence, ok := payload["sequence_number"].(float64)
		if !ok {
			continue
		}
		current := int64(sequence)
		if current <= lastSequence {
			t.Fatalf("sequence_number is not strictly increasing: previous=%d current=%d\n%s", lastSequence, current, body)
		}
		lastSequence = current
	}
}

func TestResponsesIntegerArgumentsBufferOverflowFallsBackToStreaming(t *testing.T) {
	request := []byte(`{
		"model":"public",
		"stream":true,
		"tools":[{"type":"function","name":"write","parameters":{"type":"object","properties":{"content":{"type":"string"},"mode":{"type":"integer"}}}}]
	}`)
	_, compatibility, err := normalizeResponsesRequest(request, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	added := []byte(`{"type":"response.output_item.added","sequence_number":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"write","arguments":""}}`)
	if _, err := compatibility.rewriteStreamData("response.output_item.added", added); err != nil {
		t.Fatal(err)
	}
	first := strings.Repeat("a", maxBufferedFunctionArgumentsBytes)
	firstPayload, _ := json.Marshal(map[string]any{"type": "response.function_call_arguments.delta", "sequence_number": 1, "item_id": "fc_1", "delta": first})
	outputs, err := compatibility.rewriteStreamData("response.function_call_arguments.delta", firstPayload)
	if err != nil || len(outputs) != 0 {
		t.Fatalf("first buffered delta: outputs=%d err=%v", len(outputs), err)
	}
	overflowPayload := []byte(`{"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":"fc_1","delta":"b"}`)
	outputs, err = compatibility.rewriteStreamData("response.function_call_arguments.delta", overflowPayload)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 2 {
		t.Fatalf("overflow outputs = %d", len(outputs))
	}
	flushed := outputs[0].Payload
	if delta, _ := flushed["delta"].(string); len(delta) != maxBufferedFunctionArgumentsBytes {
		t.Fatalf("flushed delta length = %d", len(delta))
	}
	current := outputs[1].Payload
	if current["delta"] != "b" {
		t.Fatalf("current delta = %q", current["delta"])
	}
	state := compatibility.streamCalls["fc_1"]
	if state == nil || !state.passthrough || state.arguments.Len() != 0 || compatibility.streamArgumentBytes != 0 {
		t.Fatal("overflow did not release the buffered arguments")
	}
}
