package cli

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestResponsesCustomToolRequestHistoryAndJSONResponse(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"run code",
		"tools":[{"type":"custom","name":"code","description":"Run code","format":{"type":"text"}}],
		"tool_choice":{"type":"custom","name":"code"}
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil {
		t.Fatal("custom tool 未启用兼容层")
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tool := request["tools"].([]any)[0].(map[string]any)
	parameters := tool["parameters"].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "code" || !strings.Contains(tool["description"].(string), "input string field") || parameters["additionalProperties"] != false {
		t.Fatalf("上游 custom 包装 = %#v", tool)
	}
	choice := request["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "code" {
		t.Fatalf("上游 custom tool_choice = %#v", choice)
	}

	restored, err := compatibility.normalizeResponseJSON([]byte(`{
		"id":"resp_1","object":"response",
		"tools":[{"type":"function","name":"code"}],
		"output":[{"id":"item_1","type":"function_call","call_id":"call_1","name":"code","arguments":"{\"input\":\"print(1)\"}"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(restored, &response); err != nil {
		t.Fatal(err)
	}
	call := response["output"].([]any)[0].(map[string]any)
	if call["type"] != "custom_tool_call" || call["name"] != "code" || call["input"] != "print(1)" || call["arguments"] != nil {
		t.Fatalf("下游 custom_tool_call = %#v", call)
	}
	visible := response["tools"].([]any)[0].(map[string]any)
	if visible["type"] != "custom" || visible["format"].(map[string]any)["type"] != "text" {
		t.Fatalf("下游 custom tool = %#v", visible)
	}

	history, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"type":"custom_tool_call","call_id":"call_1","name":"code","input":"print(1)"},
			{"type":"custom_tool_call_output","call_id":"call_1","output":"1"}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var next map[string]any
	if err := json.Unmarshal(history, &next); err != nil {
		t.Fatal(err)
	}
	items := next["input"].([]any)
	historyCall := items[0].(map[string]any)
	historyOutput := items[1].(map[string]any)
	if historyCall["type"] != "function_call" || historyCall["arguments"] != `{"input":"print(1)"}` || historyCall["input"] != nil {
		t.Fatalf("custom call 历史 = %#v", historyCall)
	}
	if historyOutput["type"] != "function_call_output" || historyOutput["call_id"] != "call_1" || historyOutput["output"] != "1" {
		t.Fatalf("custom output 历史 = %#v", historyOutput)
	}
}

func TestResponsesCustomToolStreamUsesCustomEvents(t *testing.T) {
	_, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"run",
		"tools":[{"type":"custom","name":"code"}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"code","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","output_index":0,"delta":"{\"input\":\"pri"}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","output_index":0,"delta":"nt(1)\"}"}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"item_1","output_index":0,"arguments":"{\"input\":\"print(1)\"}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"code","arguments":"{\"input\":\"print(1)\"}"}}`,
		``,
	}, "\n")
	stream := compatibility.normalizeResponseStream(io.NopCloser(strings.NewReader(source)))
	converted, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	for _, expected := range []string{
		`"type":"custom_tool_call"`,
		`event: response.custom_tool_call_input.delta`,
		`"delta":"print(1)"`,
		`event: response.custom_tool_call_input.done`,
		`"input":"print(1)"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("custom SSE 缺少 %s:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "response.function_call_arguments") || strings.Contains(text, `"arguments"`) {
		t.Fatalf("上游 function 参数事件泄露到 custom SSE:\n%s", text)
	}
}

func TestResponsesCustomGrammarDowngradesWithoutRejectingRequest(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"run",
		"tools":[{"type":"custom","name":"code","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tool := request["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "code" {
		t.Fatalf("tool = %#v", tool)
	}
	if compatibility == nil || !strings.Contains(compatibility.warningHeader(), "custom_tool_format_downgraded") {
		t.Fatalf("compatibility = %#v", compatibility)
	}
}

func TestResponsesWebSearchAliasesAndOptions(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"search",
		"tools":[{"type":"web_search_preview"}],
		"tool_choice":{"type":"web_search_preview"}
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil {
		t.Fatal("web search alias 未启用兼容层")
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tool := request["tools"].([]any)[0].(map[string]any)
	if len(tool) != 1 || tool["type"] != "web_search" || request["tool_choice"] != "required" {
		t.Fatalf("web search alias = %#v, choice = %#v", tool, request["tool_choice"])
	}

	normalized, compatibility, err = normalizeResponsesRequest([]byte(`{
		"model":"public","input":"search",
		"tools":[{"type":"web_search","external_web_access":true,"indexed_web_access":true,"search_content_types":["text"],"search_context_size":"low","user_location":{"type":"approximate","country":"CN"},"filters":{"allowed_domains":[]}}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil {
		t.Fatal("Codex web_search 控制字段未启用兼容层")
	}
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tool = request["tools"].([]any)[0].(map[string]any)
	if len(tool) != 1 || tool["type"] != "web_search" {
		t.Fatalf("Codex web search 未降级: %#v", tool)
	}
	if !strings.Contains(compatibility.warningHeader(), "web_search_controls_downgraded") {
		t.Fatalf("compatibility warnings = %q", compatibility.warningHeader())
	}

	for _, supported := range []string{
		`"filters":{"allowed_domains":["example.com"]}`,
		`"allowed_domains":["example.com"]`,
	} {
		normalized, compatibility, err = normalizeResponsesRequest([]byte(`{
			"model":"public","input":"search","tools":[{"type":"web_search",`+supported+`}]
		}`), "grok-4.5")
		if err != nil {
			t.Fatal(err)
		}
		request = nil
		if err := json.Unmarshal(normalized, &request); err != nil {
			t.Fatal(err)
		}
		tool = request["tools"].([]any)[0].(map[string]any)
		domains := tool["filters"].(map[string]any)["allowed_domains"].([]any)
		if len(domains) != 1 || domains[0] != "example.com" {
			t.Fatalf("allowed_domains 未保留: %#v", tool)
		}
		if strings.Contains(supported, `"allowed_domains"`) && !strings.Contains(supported, `"filters"`) && (compatibility == nil || !strings.Contains(compatibility.warningHeader(), "web_search_allowed_domains_normalized")) {
			t.Fatalf("top-level allowed_domains warning = %#v", compatibility)
		}
	}

	for _, restricted := range []string{
		`"search_content_types":["image"]`,
		`"filters":{"blocked_domains":["example.com"]}`,
	} {
		normalized, compatibility, err = normalizeResponsesRequest([]byte(`{
			"model":"public","input":"search","tools":[{"type":"web_search",`+restricted+`}]
		}`), "grok-4.5")
		if err != nil {
			t.Fatal(err)
		}
		request = nil
		if err := json.Unmarshal(normalized, &request); err != nil {
			t.Fatal(err)
		}
		tool = request["tools"].([]any)[0].(map[string]any)
		if len(tool) != 1 || tool["type"] != "web_search" || compatibility == nil || compatibility.warningHeader() == "" {
			t.Fatalf("restricted web search should downgrade: tool=%#v compatibility=%#v", tool, compatibility)
		}
	}

	normalized, compatibility, err = normalizeResponsesRequest([]byte(`{
		"model":"public","input":"do not access the internet",
		"tools":[{"type":"web_search","external_web_access":false}],
		"tool_choice":{"type":"web_search"}
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	request = nil
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	if _, exists := request["tools"]; exists {
		t.Fatalf("disabled web search request = %#v", request)
	}
	if _, exists := request["tool_choice"]; exists {
		t.Fatalf("disabled web search tool_choice = %#v", request["tool_choice"])
	}
	warnings := compatibility.warningHeader()
	if !strings.Contains(warnings, "web_search_disabled_no_external_access") || !strings.Contains(warnings, "tool_choice_without_tools_ignored") {
		t.Fatalf("compatibility warnings = %q", warnings)
	}

	normalized, compatibility, err = normalizeResponsesRequest([]byte(`{
		"model":"public","input":"use local tools only",
		"tools":[
			{"type":"web_search","external_web_access":false},
			{"type":"function","name":"local_lookup","parameters":{"type":"object"}}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	request = nil
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tools := request["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "local_lookup" {
		t.Fatalf("local-only tools = %#v", tools)
	}
	if !strings.Contains(compatibility.warningHeader(), "web_search_disabled_no_external_access") {
		t.Fatalf("compatibility warnings = %q", compatibility.warningHeader())
	}

	normalized, _, err = normalizeResponsesRequest([]byte(`{
		"model":"public","input":"offline only",
		"tools":[{"type":"web_search","external_web_access":false}],
		"tool_choice":"auto"
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	request = nil
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	if _, exists := request["tools"]; exists {
		t.Fatalf("disabled automatic web search request = %#v", request)
	}
	if _, exists := request["tool_choice"]; exists {
		t.Fatalf("disabled automatic web search tool_choice = %#v", request["tool_choice"])
	}

	normalized, compatibility, err = normalizeResponsesRequest([]byte(`{
		"model":"public","input":"search",
		"tools":[{"type":"web_search","unknown_control":true}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	request = nil
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tool = request["tools"].([]any)[0].(map[string]any)
	if len(tool) != 1 || tool["type"] != "web_search" || compatibility == nil || !strings.Contains(compatibility.warningHeader(), "web_search_unknown_controls_ignored") {
		t.Fatalf("unknown web search option should downgrade: tool=%#v compatibility=%#v", tool, compatibility)
	}
}

func TestResponsesBuild02101NativeAndUnsupportedToolMatrix(t *testing.T) {
	native := []string{"x_search", "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter", "mcp", "shell"}
	for _, kind := range native {
		t.Run("native_"+kind, func(t *testing.T) {
			body := []byte(`{"model":"public","input":"hello","tools":[{"type":"` + kind + `"}]}`)
			normalized, _, err := normalizeResponsesRequest(body, "grok-4.5")
			if err != nil {
				t.Fatal(err)
			}
			var request map[string]any
			if err := json.Unmarshal(normalized, &request); err != nil {
				t.Fatal(err)
			}
			tool := request["tools"].([]any)[0].(map[string]any)
			if tool["type"] != kind {
				t.Fatalf("tool = %#v", tool)
			}
		})
	}

	compatible := []string{"local_shell", "apply_patch"}
	for _, kind := range compatible {
		t.Run("compatible_"+kind, func(t *testing.T) {
			body := []byte(`{"model":"public","input":"hello","tools":[{"type":"` + kind + `"}]}`)
			if _, compatibility, err := normalizeResponsesRequest(body, "grok-4.5"); err != nil || compatibility == nil {
				t.Fatalf("compatibility=%#v error=%v", compatibility, err)
			}
		})
	}

	unsupported := []string{"computer_use_preview", "unknown_tool"}
	for _, kind := range unsupported {
		t.Run("unsupported_"+kind, func(t *testing.T) {
			body := []byte(`{"model":"public","input":"hello","tools":[{"type":"` + kind + `"}]}`)
			_, _, err := normalizeResponsesRequest(body, "grok-4.5")
			requestErr, ok := err.(*responsesRequestError)
			if !ok || requestErr.Code != "unsupported_parameter" || requestErr.Param != "tools[0].type" || !strings.Contains(requestErr.Message, "0.2.101") {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestResponsesHostedToolChoiceNarrowsToMatchingTool(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"draw",
		"tools":[{"type":"image_generation"},{"type":"web_search"}],
		"tool_choice":{"type":"image_generation"}
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tools := request["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "image_generation" || request["tool_choice"] != "required" {
		t.Fatalf("request = %#v", request)
	}
	if compatibility == nil || !strings.Contains(compatibility.warningHeader(), "hosted_tool_choice_tools_narrowed") {
		t.Fatalf("compatibility = %#v", compatibility)
	}
}

func TestResponsesMCPDeferLoadingUsesClientToolSearch(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"find tools",
		"tools":[
			{"type":"mcp","server_label":"github","server_url":"https://example.com/mcp","description":"GitHub tools","defer_loading":true},
			{"type":"tool_search","execution":"client"}
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
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" || !strings.Contains(tools[0].(map[string]any)["description"].(string), "github: GitHub tools") {
		t.Fatalf("上游 tools = %#v", tools)
	}

	loaded, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"type":"tool_search_call","execution":"client","call_id":"search_1","arguments":{}},
			{"type":"tool_search_output","execution":"client","call_id":"search_1","tools":[
				{"type":"mcp","server_label":"github","server_url":"https://example.com/mcp","defer_loading":true}
			]}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(loaded, &request); err != nil {
		t.Fatal(err)
	}
	loadedTool := request["tools"].([]any)[0].(map[string]any)
	if loadedTool["type"] != "mcp" || loadedTool["defer_loading"] != nil {
		t.Fatalf("已加载 MCP = %#v", loadedTool)
	}
}

func TestResponsesCodexHistoryItemsAreStructuredOrVisible(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"type":"agent_message","author":"worker","recipient":"root","content":[{"type":"input_text","text":"analysis result"}]},
			{"type":"local_shell_call","call_id":"shell_1","status":"completed","action":{"type":"exec","command":"pwd"}},
			{"type":"local_shell_call_output","call_id":"shell_1","status":"completed","output":"/workspace\n"},
			{"type":"mcp_tool_call_output","call_id":"mcp_1","output":{"content":"done"}}
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
	if items[0].(map[string]any)["role"] != "developer" || items[1].(map[string]any)["type"] != "shell_call" || items[2].(map[string]any)["type"] != "shell_call_output" || items[3].(map[string]any)["role"] != "developer" {
		t.Fatalf("Codex 历史转换 = %#v", items)
	}
	for index, expected := range map[int]string{0: "Agent message", 3: "MCP tool output"} {
		content := items[index].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(content, expected) {
			t.Fatalf("input[%d] = %q", index, content)
		}
	}

	normalized, _, err = normalizeResponsesRequest([]byte(`{
		"model":"public","input":[{"type":"agent_message","content":[{"type":"encrypted_text","encrypted_content":"opaque"}]}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	marker := request["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(marker, "encrypted inter-agent message") {
		t.Fatalf("opaque agent marker = %q", marker)
	}
}

func TestResponsesStreamFiltersPrivateEventsAndPreservesSSEFields(t *testing.T) {
	_, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"lookup",
		"tools":[{"type":"namespace","name":"crm","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Join([]string{
		`: keep-alive`,
		`id: event-1`,
		`retry: 1500`,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"crm__lookup","arguments":"{}"}}`,
		``,
		`event: xai.internal.trace`,
		`data: {"type":"xai.internal.trace","secret":"private"}`,
		``,
		`data: {"type":"xai.internal.metrics","value":1}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	stream := compatibility.normalizeResponseStream(io.NopCloser(strings.NewReader(source)))
	converted, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	for _, expected := range []string{": keep-alive", "id: event-1", "retry: 1500", `"namespace":"crm"`, "data: [DONE]"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("SSE 缺少 %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "xai.internal") || strings.Contains(text, "private") {
		t.Fatalf("私有事件泄露:\n%s", text)
	}
}

func TestResponsesToolAliasesAreUniqueAndBounded(t *testing.T) {
	longNamespace := strings.Repeat("n", 100)
	longName := strings.Repeat("t", 100)
	body := []byte(`{
		"model":"public","input":"hello","tools":[
			{"type":"function","name":"a__bc","parameters":{"type":"object"}},
			{"type":"namespace","name":"a","tools":[{"type":"function","name":"bc","parameters":{"type":"object"}}]},
			{"type":"namespace","name":"` + longNamespace + `","tools":[{"type":"function","name":"` + longName + `","parameters":{"type":"object"}}]}
		]
	}`)
	normalized, _, err := normalizeResponsesRequest(body, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tools := request["tools"].([]any)
	aliases := map[string]bool{}
	for _, rawTool := range tools {
		name := rawTool.(map[string]any)["name"].(string)
		if len(name) > maxBuildToolAliasLength {
			t.Fatalf("alias 长度 = %d: %q", len(name), name)
		}
		if aliases[name] {
			t.Fatalf("alias 冲突: %q", name)
		}
		aliases[name] = true
	}
	if tools[0].(map[string]any)["name"] != "a__bc" || tools[1].(map[string]any)["name"] == "a__bc" || !strings.Contains(tools[1].(map[string]any)["name"].(string), "__") {
		t.Fatalf("碰撞 alias = %#v", tools)
	}
}
