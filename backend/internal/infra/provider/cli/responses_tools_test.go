package cli

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestNormalizeResponsesRequestFlattensNamespaceAndRestoresResponse(t *testing.T) {
	body := []byte(`{
		"model":"public",
		"input":"List calendar events",
		"tools":[
			{"type":"namespace","name":"mcp__calendar__","description":"Calendar tools","tools":[
				{"type":"function","name":"create","defer_loading":true,"parameters":{"type":"object"}},
				{"type":"function","name":"list","parameters":{"type":"object"}}
			]},
			{"type":"tool_search","execution":"client","description":"Find a calendar tool","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"function","name":"list","namespace":"mcp__calendar__"}
	}`)

	normalized, compatibility, err := normalizeResponsesRequest(body, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil {
		t.Fatal("namespace 请求未建立响应恢复映射")
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	if parallel, ok := request["parallel_tool_calls"].(bool); !ok || parallel {
		t.Fatalf("客户端 tool_search 必须串行执行: %#v", request["parallel_tool_calls"])
	}
	tools := request["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("上游 tools = %#v", tools)
	}
	listTool := tools[0].(map[string]any)
	searchTool := tools[1].(map[string]any)
	if listTool["type"] != "function" || listTool["name"] != "mcp__calendar__list" {
		t.Fatalf("namespace 函数未展平: %#v", listTool)
	}
	if _, exists := listTool["defer_loading"]; exists {
		t.Fatalf("defer_loading 泄露到上游: %#v", listTool)
	}
	if searchTool["type"] != "function" || searchTool["name"] != "grok2api_tool_search" || !strings.Contains(searchTool["description"].(string), "Calendar tools") {
		t.Fatalf("客户端 tool_search 未转换: %#v", searchTool)
	}
	choice := request["tool_choice"].(map[string]any)
	if choice["name"] != "mcp__calendar__list" || choice["namespace"] != nil {
		t.Fatalf("tool_choice 未转换: %#v", choice)
	}

	upstream := []byte(`{
		"id":"resp_1","object":"response",
		"tools":[{"type":"function","name":"mcp__calendar__list"},{"type":"function","name":"grok2api_tool_search"}],
		"output":[
			{"type":"function_call","call_id":"call_1","name":"mcp__calendar__list","arguments":"{}"},
			{"type":"function_call","call_id":"call_2","name":"grok2api_tool_search","arguments":"{\"goal\":\"calendar\"}"}
		]
	}`)
	restored, err := compatibility.normalizeResponseJSON(upstream)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(restored, &response); err != nil {
		t.Fatal(err)
	}
	output := response["output"].([]any)
	namespaced := output[0].(map[string]any)
	if namespaced["name"] != "list" || namespaced["namespace"] != "mcp__calendar__" {
		t.Fatalf("namespace 调用未恢复: %#v", namespaced)
	}
	searchCall := output[1].(map[string]any)
	arguments, ok := searchCall["arguments"].(map[string]any)
	if searchCall["type"] != "tool_search_call" || searchCall["execution"] != "client" || searchCall["name"] != nil || !ok || arguments["goal"] != "calendar" {
		t.Fatalf("tool_search_call 未恢复: %#v", searchCall)
	}
	visibleTools := response["tools"].([]any)
	if len(visibleTools) != 2 || visibleTools[0].(map[string]any)["type"] != "namespace" || visibleTools[1].(map[string]any)["type"] != "tool_search" {
		t.Fatalf("响应 tools 未恢复为客户端声明: %#v", visibleTools)
	}
}

func TestNormalizeResponsesRequestLoadsClientToolSearchOutput(t *testing.T) {
	body := []byte(`{
		"model":"public",
		"input":[
			{"type":"tool_search_call","execution":"client","call_id":"search_1","arguments":{"goal":"shipping"}},
			{"type":"tool_search_output","execution":"client","call_id":"search_1","status":"completed","tools":[
				{"type":"namespace","name":"shipping","tools":[
					{"type":"function","name":"get_eta","defer_loading":true,"parameters":{"type":"object"}}
				]}
			]}
		]
	}`)

	normalized, compatibility, err := normalizeResponsesRequest(body, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil {
		t.Fatal("tool_search 续轮未建立响应恢复映射")
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tools := request["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("已加载 tools = %#v", tools)
	}
	loaded := tools[0].(map[string]any)
	if loaded["name"] != "shipping__get_eta" || loaded["defer_loading"] != nil {
		t.Fatalf("已加载工具未转换: %#v", loaded)
	}
	input := request["input"].([]any)
	call := input[0].(map[string]any)
	output := input[1].(map[string]any)
	if call["type"] != "function_call" || call["name"] != "grok2api_tool_search" || call["arguments"] != `{"goal":"shipping"}` {
		t.Fatalf("tool_search_call 历史未转换: %#v", call)
	}
	if output["type"] != "function_call_output" || output["call_id"] != "search_1" {
		t.Fatalf("tool_search_output 历史未转换: %#v", output)
	}
}

func TestNormalizeResponsesRequestRejectsHostedToolSearch(t *testing.T) {
	_, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"hello",
		"tools":[{"type":"tool_search"}]
	}`), "grok-4.5")
	requestErr, ok := err.(*responsesRequestError)
	if !ok || requestErr.Code != "unsupported_parameter" || requestErr.Param != "tools[0].execution" {
		t.Fatalf("error = %#v", err)
	}
}

func TestNormalizeResponsesRequestRejectsParallelClientToolSearch(t *testing.T) {
	_, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"hello","parallel_tool_calls":true,
		"tools":[{"type":"tool_search","execution":"client"}]
	}`), "grok-4.5")
	requestErr, ok := err.(*responsesRequestError)
	if !ok || requestErr.Code != "unsupported_parameter" || requestErr.Param != "parallel_tool_calls" {
		t.Fatalf("error = %#v", err)
	}
}

func TestNormalizeResponsesRequestRejectsDeferredToolWithoutSearch(t *testing.T) {
	_, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"hello",
		"tools":[{"type":"function","name":"lookup","defer_loading":true,"parameters":{"type":"object"}}]
	}`), "grok-4.5")
	requestErr, ok := err.(*responsesRequestError)
	if !ok || requestErr.Code != "invalid_parameter" || requestErr.Param != "tools[0].defer_loading" {
		t.Fatalf("error = %#v", err)
	}
}

func TestNormalizeResponsesRequestKeepsOrdinaryFunctionsOnNativePath(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"hello",
		"tools":[{"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object"}}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility != nil {
		t.Fatal("普通函数请求不应启用响应兼容层")
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tool := request["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "lookup" || tool["description"] != "Lookup" {
		t.Fatalf("普通函数被意外改写: %#v", tool)
	}
}

func TestResponsesCompatibilityRestoresNamespaceAndToolSearchStream(t *testing.T) {
	_, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"hello",
		"tools":[
			{"type":"namespace","name":"crm","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
			{"type":"tool_search","execution":"client","parameters":{"type":"object"}}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"crm__lookup","arguments":""}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"item_2","type":"function_call","call_id":"call_2","name":"grok2api_tool_search","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_2","delta":"{\"goal\":"}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"item_2","arguments":"{\"goal\":\"crm\"}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"item_2","type":"function_call","call_id":"call_2","name":"grok2api_tool_search","arguments":"{\"goal\":\"crm\"}"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"object":"response","tools":[{"type":"function","name":"crm__lookup"}],"output":[{"type":"function_call","call_id":"call_1","name":"crm__lookup","arguments":"{}"}]}}`,
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
	if strings.Contains(text, "response.function_call_arguments") {
		t.Fatalf("内部 Tool Search 参数事件未隐藏:\n%s", text)
	}
	for _, expected := range []string{`"namespace":"crm"`, `"type":"tool_search_call"`, `"goal":"crm"`, `"type":"namespace"`, `data: [DONE]`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("流式响应缺少 %s:\n%s", expected, text)
		}
	}
}
