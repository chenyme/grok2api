package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeBuildFunctionParametersRootLiftsNestedUnionRefs(t *testing.T) {
	schema := map[string]any{
		"$defs": map[string]any{
			"ID":   map[string]any{"type": "string"},
			"View": map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"enum": []any{"view"}, "type": "string"}, "id": map[string]any{"$ref": "#/$defs/ID"}}, "required": []any{"mode", "id"}, "additionalProperties": false},
			"Create": map[string]any{"oneOf": []any{
				map[string]any{"$ref": "#/$defs/CreateCron"},
				map[string]any{"$ref": "#/$defs/CreateHB"},
			}},
			"CreateCron": map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"enum": []any{"create"}, "type": "string"}, "kind": map[string]any{"enum": []any{"cron"}, "type": "string"}, "name": map[string]any{"$ref": "#/$defs/ID"}}, "required": []any{"mode", "kind", "name"}, "additionalProperties": false},
			"CreateHB":   map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"enum": []any{"create"}, "type": "string"}, "kind": map[string]any{"enum": []any{"heartbeat"}, "type": "string"}, "name": map[string]any{"$ref": "#/$defs/ID"}}, "required": []any{"mode", "kind", "name"}, "additionalProperties": false},
			"Delete":     map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"enum": []any{"delete"}, "type": "string"}, "id": map[string]any{"$ref": "#/$defs/ID"}}, "required": []any{"mode", "id"}, "additionalProperties": false},
		},
		"type":       "object",
		"properties": map[string]any{},
		"oneOf": []any{
			map[string]any{"$ref": "#/$defs/View"},
			map[string]any{"$ref": "#/$defs/Create"},
			map[string]any{"$ref": "#/$defs/Delete"},
		},
	}
	normalized, changed, err := normalizeBuildFunctionParametersRoot(schema, "tools[0].parameters")
	if err != nil {
		t.Fatal(err)
	}
	out, ok := normalized.(map[string]any)
	if !ok || !changed {
		t.Fatalf("normalized=%#v changed=%v", normalized, changed)
	}
	if _, exists := out["$ref"]; exists {
		t.Fatalf("root still has $ref: %#v", out)
	}
	branches, _ := out["oneOf"].([]any)
	if out["type"] != nil || len(branches) != 4 {
		t.Fatalf("want root oneOf[4] without type wrapper, got %#v", out)
	}
	for i, raw := range branches {
		branch, _ := raw.(map[string]any)
		if branch["type"] != "object" {
			t.Fatalf("branch[%d] type=%v %#v", i, branch["type"], branch)
		}
		if _, exists := branch["oneOf"]; exists {
			t.Fatalf("branch[%d] still nested oneOf: %#v", i, branch)
		}
		if _, exists := branch["$ref"]; exists {
			t.Fatalf("branch[%d] still $ref: %#v", i, branch)
		}
		props, _ := branch["properties"].(map[string]any)
		if id, ok := props["id"].(map[string]any); ok {
			if id["$ref"] != "#/$defs/ID" {
				t.Fatalf("property $ref was inlined: %#v", id)
			}
		}
		if name, ok := props["name"].(map[string]any); ok {
			if name["$ref"] != "#/$defs/ID" {
				t.Fatalf("property $ref was inlined: %#v", name)
			}
		}
	}
	defs, _ := out["$defs"].(map[string]any)
	if _, ok := defs["ID"]; !ok {
		t.Fatalf("$defs.ID dropped: %#v", defs)
	}
}

func TestNormalizeBuildFunctionParametersRootKeepsNestedPropertyUnion(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{"value": map[string]any{
			"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}},
		}},
	}
	normalized, changed, err := normalizeBuildFunctionParametersRoot(schema, "tools[0].parameters")
	if err != nil {
		t.Fatal(err)
	}
	out := normalized.(map[string]any)
	value := out["properties"].(map[string]any)["value"].(map[string]any)
	if changed || value["anyOf"] == nil {
		t.Fatalf("nested anyOf was rewritten: %#v changed=%v", out, changed)
	}
}

func TestNormalizeBuildFunctionParametersRootLiftsOneOfNestedAnyOf(t *testing.T) {
	schema := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
			map[string]any{"anyOf": []any{
				map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}}},
				map[string]any{"type": "null"},
			}},
		},
	}
	normalized, changed, err := normalizeBuildFunctionParametersRoot(schema, "tools[0].parameters")
	if err != nil {
		t.Fatal(err)
	}
	out := normalized.(map[string]any)
	branches, _ := out["oneOf"].([]any)
	if !changed || len(branches) != 2 {
		t.Fatalf("want 2 object leaves, got %#v", out)
	}
	for i, raw := range branches {
		if raw.(map[string]any)["type"] != "object" {
			t.Fatalf("branch[%d]=%#v", i, raw)
		}
	}
}

func TestNormalizeBuildFunctionParametersRootRejectsRootCycle(t *testing.T) {
	schema := map[string]any{
		"$defs": map[string]any{
			"Loop": map[string]any{"$ref": "#/$defs/Loop"},
		},
		"oneOf": []any{map[string]any{"$ref": "#/$defs/Loop"}},
	}
	_, _, err := normalizeBuildFunctionParametersRoot(schema, "tools[0].parameters")
	requestErr, ok := err.(*responsesRequestError)
	if !ok || requestErr.Param != "tools[0].parameters" {
		t.Fatalf("error = %#v", err)
	}
}

func TestNormalizeBuildFunctionParametersRootKeepsPropertyCycle(t *testing.T) {
	schema := map[string]any{
		"$defs": map[string]any{
			"Node": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"child": map[string]any{"$ref": "#/$defs/Node"},
				},
			},
		},
		"type": "object",
		"properties": map[string]any{
			"root": map[string]any{"$ref": "#/$defs/Node"},
		},
	}
	normalized, changed, err := normalizeBuildFunctionParametersRoot(schema, "tools[0].parameters")
	if err != nil {
		t.Fatal(err)
	}
	out := normalized.(map[string]any)
	if changed {
		t.Fatalf("property cycle should not rewrite root: %#v", out)
	}
	root := out["properties"].(map[string]any)["root"].(map[string]any)
	if root["$ref"] != "#/$defs/Node" {
		t.Fatalf("property $ref rewritten: %#v", root)
	}
}

func TestNormalizeResponsesRequestLiftsAutomationUpdateStyleUnion(t *testing.T) {
	body := []byte(`{
		"model":"public","input":"hello",
		"tools":[{"type":"function","name":"mcp__codex_app__automation_update","parameters":{
			"$defs":{
				"ID":{"type":"string"},
				"View":{"type":"object","properties":{"mode":{"enum":["view"],"type":"string"},"id":{"$ref":"#/$defs/ID"}},"required":["mode","id"]},
				"Create":{"oneOf":[
					{"type":"object","properties":{"mode":{"enum":["create"],"type":"string"},"name":{"$ref":"#/$defs/ID"}},"required":["mode","name"]}
				]}
			},
			"type":"object",
			"properties":{},
			"oneOf":[{"$ref":"#/$defs/View"},{"$ref":"#/$defs/Create"}]
		}}]
	}`)
	normalized, compatibility, err := normalizeResponsesRequest(body, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil || !strings.Contains(compatibility.warningHeader(), "function_parameters_root_normalized") {
		t.Fatalf("compatibility = %#v", compatibility)
	}
	var payload map[string]any
	if err := json.Unmarshal(normalized, &payload); err != nil {
		t.Fatal(err)
	}
	parameters := payload["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	branches, _ := parameters["oneOf"].([]any)
	if len(branches) != 2 {
		t.Fatalf("upstream parameters = %#v", parameters)
	}
	visible := compatibility.visibleTools[0].(map[string]any)["parameters"].(map[string]any)
	if _, ok := visible["$defs"]; !ok {
		t.Fatalf("visible schema lost $defs: %#v", visible)
	}
}
