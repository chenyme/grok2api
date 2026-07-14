package cli

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestLegacyLocalShellUsesNativeLocalShellAndRestoresJSON(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"show cwd",
		"tools":[{"type":"local_shell"}],
		"tool_choice":{"type":"local_shell"}
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil || !compatibility.legacyLocalShell {
		t.Fatal("legacy local_shell 未启用兼容层")
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tool := request["tools"].([]any)[0].(map[string]any)
	environment := tool["environment"].(map[string]any)
	if tool["type"] != "shell" || environment["type"] != "local" || request["tool_choice"] != "required" {
		t.Fatalf("upstream shell = %#v, choice = %#v", tool, request["tool_choice"])
	}

	restored, err := compatibility.normalizeResponseJSON([]byte(`{
		"id":"resp_1","object":"response",
		"tools":[{"type":"shell","environment":{"type":"local"}}],
		"output":[{"id":"sh_1","type":"shell_call","call_id":"call_1","status":"completed","action":{"type":"exec","commands":["pwd"]}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(restored, &response); err != nil {
		t.Fatal(err)
	}
	call := response["output"].([]any)[0].(map[string]any)
	action := call["action"].(map[string]any)
	if call["type"] != "local_shell_call" || action["type"] != "exec" || action["command"] != "pwd" {
		t.Fatalf("legacy local_shell_call = %#v", call)
	}
	if response["tools"].([]any)[0].(map[string]any)["type"] != "local_shell" {
		t.Fatalf("visible tools = %#v", response["tools"])
	}
}

func TestLegacyLocalShellHistoryBecomesStructuredShellHistory(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"local_shell"}],"input":[
			{"type":"local_shell_call","id":"sh_1","call_id":"call_1","status":"completed","action":{"type":"exec","command":["printf","a b"],"working_directory":"/workspace","env":{"MODE":"test"}}},
			{"type":"local_shell_call_output","call_id":"call_1","status":"failed","exit_code":7,"output":"failure"}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	call := items[0].(map[string]any)
	commands := call["action"].(map[string]any)["commands"].([]any)
	if call["type"] != "shell_call" || len(commands) != 1 || commands[0] != `cd /workspace && env MODE=test printf 'a b'` {
		t.Fatalf("shell call history = %#v", call)
	}
	output := items[1].(map[string]any)
	outcome := output["output"].([]any)[0].(map[string]any)["outcome"].(map[string]any)
	if output["type"] != "shell_call_output" || outcome["exit_code"] != float64(7) {
		t.Fatalf("shell output history = %#v", output)
	}
}

func TestNativeShellOutputHistoryIsSanitizedForBuild(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"shell","environment":{"type":"local"}}],"input":[
			{"type":"shell_call_output","call_id":"call_1","status":"completed","output":[
				{"command":"pwd","stdout":"/workspace\n","stderr":"","outcome":{"type":"exit","exitCode":0}}
			],"max_output_length":2048}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	item := request["input"].([]any)[0].(map[string]any)
	block := item["output"].([]any)[0].(map[string]any)
	outcome := block["outcome"].(map[string]any)
	if item["type"] != "shell_call_output" || block["command"] != nil || outcome["exit_code"] != float64(0) || outcome["exitCode"] != nil {
		t.Fatalf("shell output history = %#v", item)
	}
}

func TestFunctionCallOutputHistoryEncodesStructuredOutput(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],"input":[
			{"type":"function_call_output","call_id":"call_1","status":"completed","output":{"exit_code":0,"stdout":"ok","stderr":""}}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	item := request["input"].([]any)[0].(map[string]any)
	output, ok := item["output"].(string)
	if item["type"] != "function_call_output" || !ok || !strings.Contains(output, `"stdout":"ok"`) {
		t.Fatalf("function output history = %#v", item)
	}
}

func TestAssistantOutputMessageHistoryBecomesInputMessage(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[
				{"type":"output_text","text":"Hi there","annotations":[]}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	first := items[0].(map[string]any)
	content := first["content"].([]any)[0].(map[string]any)
	if first["id"] != nil || first["status"] != nil || content["type"] != "input_text" || content["annotations"] != nil {
		t.Fatalf("assistant message history = %#v", first)
	}
}

func TestStripExternalClientToolsKeepsFunctionBackedTools(t *testing.T) {
	t.Setenv("GROK2API_STRIP_EXTERNAL_TOOLS", "true")
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"hello",
		"tools":[
			{"type":"function","name":"read_file","parameters":{"type":"object"}},
			{"type":"apply_patch"},
			{"type":"tool_search","execution":"client"},
			{"type":"web_search","external_web_access":false},
			{"type":"local_shell"}
		],
		"tool_choice":{"type":"apply_patch"}
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil || !strings.Contains(compatibility.warningHeader(), "external_tools_omitted") {
		t.Fatalf("compatibility warnings = %#v", compatibility)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tools := request["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("normalized tools = %#v choice=%#v", tools, request["tool_choice"])
	}
	seen := map[string]bool{}
	for _, rawTool := range tools {
		tool := rawTool.(map[string]any)
		switch tool["type"] {
		case "function":
			seen[tool["name"].(string)] = true
		case "shell":
			seen["shell"] = true
		}
		if tool["type"] == "web_search" {
			t.Fatalf("web_search should be omitted: %#v", tools)
		}
	}
	choice := request["tool_choice"].(map[string]any)
	if !seen["read_file"] || !seen["grok2api_apply_patch"] || !seen["grok2api_tool_search"] || !seen["shell"] ||
		choice["type"] != "function" || choice["name"] != "grok2api_apply_patch" {
		t.Fatalf("normalized tools = %#v choice=%#v", tools, request["tool_choice"])
	}
	input := request["input"].([]any)
	if input[0].(map[string]any)["role"] != "developer" || input[1].(map[string]any)["role"] != "user" {
		t.Fatalf("input boundary = %#v", input)
	}
}

func TestStripExternalClientToolsRemovesStringToolChoice(t *testing.T) {
	t.Setenv("GROK2API_STRIP_EXTERNAL_TOOLS", "true")
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"hello",
		"tools":[{"type":"web_search","external_web_access":false}],
		"tool_choice":"auto"
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	if request["tools"] != nil || request["tool_choice"] != nil {
		t.Fatalf("tools/tool_choice should be omitted: %#v", request)
	}
}

func TestStripExternalClientToolHistory(t *testing.T) {
	t.Setenv("GROK2API_STRIP_EXTERNAL_TOOLS", "true")
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"type":"web_search_call","id":"ws_1","status":"completed"},
			{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"done"},
			{"type":"message","role":"user","content":"next"}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil || !strings.Contains(compatibility.warningHeader(), "external_tool_history_omitted") {
		t.Fatalf("compatibility warnings = %#v", compatibility)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	if items[0].(map[string]any)["role"] != "developer" {
		t.Fatalf("input[0] = %#v", items[0])
	}
	if items[1].(map[string]any)["type"] != "function_call" ||
		items[2].(map[string]any)["type"] != "function_call_output" ||
		items[3].(map[string]any)["role"] != "user" {
		t.Fatalf("function history should be preserved: %#v", items)
	}
}

func TestRequestRejectsAmbiguousShellDeclarations(t *testing.T) {
	_, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"hello",
		"tools":[{"type":"shell","environment":{"type":"local"}},{"type":"local_shell"}]
	}`), "grok-4.5")
	requestErr, ok := err.(*responsesRequestError)
	if !ok || requestErr.Code != "invalid_parameter" || requestErr.Param != "tools[1].type" {
		t.Fatalf("error = %#v", err)
	}
}

func TestApplyPatchToolRequestHistoryAndJSONResponse(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"edit file",
		"tools":[{"type":"apply_patch"}],
		"tool_choice":{"type":"apply_patch"}
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil {
		t.Fatal("apply_patch 未启用兼容层")
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tool := request["tools"].([]any)[0].(map[string]any)
	choice := request["tool_choice"].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "grok2api_apply_patch" || choice["type"] != "function" || choice["name"] != "grok2api_apply_patch" {
		t.Fatalf("apply patch wrapper = %#v, choice = %#v", tool, choice)
	}

	restored, err := compatibility.normalizeResponseJSON([]byte(`{
		"id":"resp_1","object":"response",
		"tools":[{"type":"function","name":"grok2api_apply_patch"}],
		"output":[{"id":"fc_1","type":"function_call","call_id":"call_1","status":"completed","name":"grok2api_apply_patch","arguments":"{\"operation\":{\"type\":\"update_file\",\"path\":\"main.go\",\"diff\":\"@@\\n-old\\n+new\"}}"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(restored, &response); err != nil {
		t.Fatal(err)
	}
	call := response["output"].([]any)[0].(map[string]any)
	operation := call["operation"].(map[string]any)
	if call["type"] != "apply_patch_call" || call["name"] != nil || call["arguments"] != nil || operation["type"] != "update_file" || operation["path"] != "main.go" {
		t.Fatalf("apply_patch_call = %#v", call)
	}
	if response["tools"].([]any)[0].(map[string]any)["type"] != "apply_patch" {
		t.Fatalf("visible tools = %#v", response["tools"])
	}

	history, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"apply_patch"}],"input":[
			{"type":"apply_patch_call","id":"apc_1","call_id":"call_1","status":"completed","operation":{"type":"delete_file","path":"old.txt"}},
			{"type":"apply_patch_call_output","call_id":"call_1","status":"failed","output":"permission denied"}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(history, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	if items[0].(map[string]any)["type"] != "function_call" || !strings.Contains(items[0].(map[string]any)["arguments"].(string), `"delete_file"`) {
		t.Fatalf("apply patch call history = %#v", items[0])
	}
	if items[1].(map[string]any)["type"] != "function_call_output" || !strings.Contains(items[1].(map[string]any)["output"].(string), "failed") {
		t.Fatalf("apply patch output history = %#v", items[1])
	}
}

func TestApplyPatchStreamBuffersFunctionProtocolAndRestoresItems(t *testing.T) {
	_, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"edit","stream":true,"tools":[{"type":"apply_patch"}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"sequence_number":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","status":"in_progress","name":"grok2api_apply_patch","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"sequence_number":2,"delta":"{\"operation\":{\"type\":\"delete_file\","}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"sequence_number":3,"arguments":"{\"operation\":{\"type\":\"delete_file\",\"path\":\"old.txt\"}}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"sequence_number":4,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","status":"completed","name":"grok2api_apply_patch","arguments":"{\"operation\":{\"type\":\"delete_file\",\"path\":\"old.txt\"}}"}}`,
		``,
	}, "\n")
	stream := compatibility.normalizeResponseStream(io.NopCloser(strings.NewReader(source)))
	converted, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Contains(text, "function_call_arguments") || strings.Contains(text, "grok2api_apply_patch") || strings.Contains(text, `"arguments"`) {
		t.Fatalf("内部 function 协议泄露:\n%s", text)
	}
	for _, expected := range []string{
		`event: response.output_item.added`, `event: response.output_item.done`,
		`"type":"apply_patch_call"`, `"type":"delete_file"`, `"path":"old.txt"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("apply patch SSE 缺少 %s:\n%s", expected, text)
		}
	}
}

func TestAdditionalToolsAndCompactionBoundaryRemainVisible(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"function","name":"lookup","description":"old","parameters":{"type":"object"}}],
		"input":[
			{"type":"compaction_trigger"},
			{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"lookup","description":"new","parameters":{"type":"object"}},{"type":"apply_patch"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tools := request["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["description"] != "new" || tools[1].(map[string]any)["name"] != "grok2api_apply_patch" {
		t.Fatalf("normalized tools = %#v", tools)
	}
	items := request["input"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["role"] != "developer" || items[1].(map[string]any)["role"] != "developer" {
		t.Fatalf("boundary items = %#v", items)
	}
	first := items[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	second := items[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(first, "compaction boundary") || !strings.Contains(second, "lookup, apply_patch") {
		t.Fatalf("boundary text = %q / %q", first, second)
	}
}
