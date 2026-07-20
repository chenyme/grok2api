package conversation

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestConvertChatRequestToResponses(t *testing.T) {
	body := []byte(`{
			"model":"public-chat","stream":true,"max_completion_tokens":512,
			"user":"client-user","presence_penalty":0,"frequency_penalty":0,
			"web_search_options":{"search_context_size":"medium"},
			"messages":[
			{"role":"system","content":"be concise"},
			{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"result"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}}
	}`)
	converted, err := ConvertRequest(body, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "grok-4.5" || payload["max_output_tokens"] != float64(512) || payload["stream"] != true || payload["safety_identifier"] != "client-user" {
		t.Fatalf("request fields = %#v", payload)
	}
	if _, exists := payload["user"]; exists {
		t.Fatalf("Chat user 不应原样转发到 Responses: %#v", payload)
	}
	input := payload["input"].([]any)
	if len(input) != 4 || input[2].(map[string]any)["type"] != "function_call" || input[3].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input = %#v", input)
	}
	content := input[1].(map[string]any)["content"].([]any)
	if content[1].(map[string]any)["image_url"] != "data:image/png;base64,AA==" {
		t.Fatalf("image content = %#v", content)
	}
	if content[1].(map[string]any)["detail"] != "auto" {
		t.Fatalf("image detail = %#v", content[1])
	}
	tools := payload["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "lookup" || tools[0].(map[string]any)["type"] != "function" || tools[1].(map[string]any)["type"] != "web_search" {
		t.Fatalf("tools = %#v", tools)
	}
}

func TestConvertChatToolImageResultToMultimodalFunctionOutput(t *testing.T) {
	body := []byte(`{
		"model":"public-chat",
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":[
				{"type":"text","text":"Read image file"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"high"}}
			]}
		]
	}`)
	converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	input := payload["input"].([]any)
	output := input[1].(map[string]any)["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("tool output = %#v", output)
	}
	textBlock := output[0].(map[string]any)
	imageBlock := output[1].(map[string]any)
	if textBlock["type"] != "input_text" || textBlock["text"] != "Read image file" ||
		imageBlock["type"] != "input_image" || imageBlock["detail"] != "high" ||
		imageBlock["image_url"] != "data:image/png;base64,AA==" {
		t.Fatalf("tool output = %#v", output)
	}
}

func TestConvertChatKeepsNonMultimodalToolJSONAsText(t *testing.T) {
	body := []byte(`{
		"model":"public-chat",
		"messages":[{"role":"tool","tool_call_id":"call_1","content":[{"name":"value","value":1}]}]
	}`)
	converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	output := payload["input"].([]any)[0].(map[string]any)["output"]
	if output != `[{"name":"value","value":1}]` {
		t.Fatalf("tool output = %#v", output)
	}
}

func TestConvertChatIgnoresSamplingFieldsWithoutResponsesEquivalent(t *testing.T) {
	body := []byte(`{
		"model":"public","messages":[{"role":"user","content":"hi"}],
		"presence_penalty":0.5,"frequency_penalty":-0.5,"seed":42
	}`)
	converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"presence_penalty", "frequency_penalty", "seed"} {
		if _, exists := payload[field]; exists {
			t.Fatalf("不受 Responses 支持的 %s 不应转发给上游: %#v", field, payload)
		}
	}
}

func TestConvertChatStopSequencesLocally(t *testing.T) {
	request := []byte(`{
		"model":"public-chat","stop":["STOP","END"],
		"messages":[{"role":"user","content":"continue"}]
	}`)
	converted, options, err := ConvertRequestWithOptions(request, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["stop"]; exists {
		t.Fatalf("Responses 请求不应包含不受支持的 stop 字段: %#v", payload)
	}
	if len(options.StopSequences) != 2 || options.StopSequences[0] != "STOP" || options.StopSequences[1] != "END" {
		t.Fatalf("stop options = %#v", options.StopSequences)
	}

	body := []byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ABCSTOPXYZ"}]}]}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationChat, options)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "ABC" {
		t.Fatalf("chat response = %#v", response)
	}
}

func TestConvertChatPreservesOpaqueToolArgumentsHistory(t *testing.T) {
	body := []byte(`{
		"model":"public","messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{partial"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"failed"}
		]
	}`)
	converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	call := payload["input"].([]any)[0].(map[string]any)
	if call["arguments"] != "{partial" {
		t.Fatalf("function call = %#v", call)
	}
}

func TestConvertChatStopSequencesStream(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5","status":"in_progress"}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"ABCST"}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"OPXYZ"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStreamWithOptions(
		io.NopCloser(strings.NewReader(stream)), OperationChat, ResponseOptions{StopSequences: []string{"STOP"}},
	))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Contains(text, "STOP") || strings.Contains(text, "XYZ") || !strings.Contains(text, `"content":"ABC"`) || !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("chat stream = %s", text)
	}
}

func TestConvertAnthropicMessagesRequestToResponses(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":1024,"stream":true,
		"system":[{"type":"text","text":"You are precise."}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		],
		"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true}
	}`)
	converted, err := ConvertRequest(body, "grok-chat-fast", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "grok-chat-fast" || payload["instructions"] != "You are precise." || payload["parallel_tool_calls"] != false {
		t.Fatalf("request = %#v", payload)
	}
	input := payload["input"].([]any)
	if len(input) != 3 || input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input = %#v", input)
	}
}

func TestConvertAnthropicMessagesInlineSystemRole(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":1024,
		"system":"Top-level rules.",
		"messages":[
			{"role":"system","content":"Inline directive."},
			{"role":"system","content":[{"type":"text","text":"Inline block."}]},
			{"role":"user","content":"hi"}
		]
	}`)
	converted, err := ConvertRequest(body, "grok-chat-fast", OperationMessages)
	if err != nil {
		t.Fatalf("inline system role should not fail: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if instructions, _ := payload["instructions"].(string); instructions != "Top-level rules.\n\nInline directive.\n\nInline block." {
		t.Fatalf("instructions = %q", instructions)
	}
	input := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input should only contain the user message, got %#v", input)
	}
	if role := input[0].(map[string]any)["role"]; role != "user" {
		t.Fatalf("remaining input role = %v", role)
	}
}

func TestConvertAnthropicMessagesInlineSystemOnly(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":256,
		"messages":[
			{"role":"system","content":"Only inline."},
			{"role":"user","content":"go"}
		]
	}`)
	converted, err := ConvertRequest(body, "grok-chat-fast", OperationMessages)
	if err != nil {
		t.Fatalf("inline-only system should not fail: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["instructions"] != "Only inline." {
		t.Fatalf("instructions = %#v", payload["instructions"])
	}
}

func TestConvertAnthropicMessagesRejectsUnknownRole(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":256,
		"messages":[{"role":"tool","content":"nope"}]
	}`)
	if _, err := ConvertRequest(body, "grok-chat-fast", OperationMessages); err == nil {
		t.Fatal("unknown role should be rejected")
	}
}

func TestConvertAnthropicRedactedThinkingIncludesEmptySummary(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":256,
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[
			{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque-reasoning"}]},
			{"role":"user","content":"continue"}
		]
	}`)
	converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []map[string]any `json:"input"`
	}
	if json.Unmarshal(converted, &payload) != nil || len(payload.Input) != 2 {
		t.Fatalf("converted = %s", converted)
	}
	reasoning := payload.Input[0]
	summary, ok := reasoning["summary"].([]any)
	if reasoning["type"] != "reasoning" || reasoning["encrypted_content"] != "opaque-reasoning" || !ok || len(summary) != 0 {
		t.Fatalf("reasoning = %#v", reasoning)
	}
}

func TestConvertAnthropicClaudeCodeRequestToResponses(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":4096,"stream":true,
		"system":[{"type":"text","text":"top-level system","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"system","content":"legacy system"},
			{"role":"developer","content":[{"type":"text","text":"developer context","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"prior thought","signature":"encrypted-reasoning"},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"README.md"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[
					{"type":"text","text":"failed"},
					{"type":"tool_reference","tool_name":"Read"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
				]},
				{"type":"document","title":"notes.txt","source":{"type":"text","data":"document text"}},
				{"type":"text","text":"continue"}
			]}
		],
		"metadata":{"user_id":"cc-user"},
		"thinking":{"type":"enabled","budget_tokens":12000},
		"tools":[{"name":"Read","description":"Read file","input_schema":{"type":"object"},"strict":true,"cache_control":{"type":"ephemeral"}}],
		"mcp_servers":[{"name":"github","url":"https://example.com/mcp","authorization_token":"token"}]
	}`)
	converted, options, err := ConvertRequestWithOptions(body, "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	if !options.AnthropicThinking {
		t.Fatal("thinking option 未保留")
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["instructions"] != "top-level system\n\nlegacy system\n\ndeveloper context" || payload["safety_identifier"] != "cc-user" || payload["store"] != false {
		t.Fatalf("request metadata = %#v", payload)
	}
	reasoning := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || payload["include"].([]any)[0] != "reasoning.encrypted_content" {
		t.Fatalf("reasoning = %#v, include = %#v", reasoning, payload["include"])
	}
	input := payload["input"].([]any)
	if len(input) != 4 || input[0].(map[string]any)["type"] != "reasoning" || input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input = %#v", input)
	}
	output := input[2].(map[string]any)["output"].([]any)
	if len(output) != 4 || !strings.Contains(output[0].(map[string]any)["text"].(string), "failed") ||
		!strings.Contains(output[2].(map[string]any)["text"].(string), `"Read"`) || output[3].(map[string]any)["type"] != "input_image" || output[3].(map[string]any)["detail"] != "auto" {
		t.Fatalf("tool result = %#v", output)
	}
	tools := payload["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["type"] != "function" || tools[0].(map[string]any)["strict"] != true || tools[1].(map[string]any)["type"] != "mcp" {
		t.Fatalf("tools = %#v", tools)
	}
}

func TestConvertAnthropicToolReferenceValidatesDeclaredTool(t *testing.T) {
	body := []byte(`{
		"model":"public","max_tokens":64,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"SearchTools","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_reference","tool_name":"Missing"}]}]}
		],
		"tools":[{"name":"SearchTools","input_schema":{"type":"object"}}]
	}`)
	_, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationMessages)
	if err == nil || !strings.Contains(err.Error(), `未声明的工具 "Missing"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestConvertAnthropicMessagesValidatesToolRelationships(t *testing.T) {
	tests := []struct {
		name     string
		messages string
		want     string
	}{
		{name: "orphan result", messages: `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"missing","content":"x"}]}]`, want: "未匹配"},
		{name: "missing result", messages: `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]}]`, want: "提供 tool_result"},
		{name: "result after text", messages: `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"text","text":"late"},{"type":"tool_result","tool_use_id":"toolu_1","content":"x"}]}]`, want: "必须位于"},
		{name: "user tool use", messages: `[{"role":"user","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]}]`, want: "只允许"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"model":"public","max_tokens":64,"messages":` + test.messages + `}`)
			_, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationMessages)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestConvertAnthropicMessagesIgnoresUnrepresentableTopK(t *testing.T) {
	converted, _, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"top_k":10,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["top_k"]; exists {
		t.Fatalf("top_k 不应转发到 Responses: %#v", payload)
	}
}

func TestConvertAnthropicWebSearchControls(t *testing.T) {
	converted, _, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"messages":[{"role":"user","content":"search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3,"allowed_domains":["example.com"],"user_location":{"type":"approximate","country":"US"}}]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(converted, &payload)
	tool := payload["tools"].([]any)[0].(map[string]any)
	domains := tool["filters"].(map[string]any)["allowed_domains"].([]any)
	if tool["type"] != "web_search" || len(domains) != 1 || domains[0] != "example.com" || len(tool) != 2 {
		t.Fatalf("tool = %#v", tool)
	}

	converted, _, err = ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"messages":[{"role":"user","content":"search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","search_context_size":"high"}]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	payload = nil
	_ = json.Unmarshal(converted, &payload)
	tool = payload["tools"].([]any)[0].(map[string]any)
	if len(tool) != 1 || tool["type"] != "web_search" {
		t.Fatalf("downgraded tool = %#v", tool)
	}
}

func TestConvertResponsesJSONToChatAndMessages(t *testing.T) {
	body := []byte(`{
		"id":"resp_1","object":"response","created_at":123,"model":"grok-4.5","status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"reason"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example"}]}]},
			{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}
		],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1},"cost_in_usd_ticks":158500,"num_sources_used":3,"num_server_side_tools_used":2,"context_details":{"input_tokens":9,"output_tokens":4}}
	}`)
	chatData, err := ConvertResponseJSON(body, OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	_ = json.Unmarshal(chatData, &chat)
	choice := chat["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if chat["object"] != "chat.completion" || choice["finish_reason"] != "tool_calls" || message["reasoning_content"] != "reason" {
		t.Fatalf("chat = %#v", chat)
	}
	if annotations := message["annotations"].([]any); len(annotations) != 1 || annotations[0].(map[string]any)["url"] != "https://example.com" {
		t.Fatalf("chat annotations = %#v", message)
	}
	chatUsage := chat["usage"].(map[string]any)
	if chatUsage["cost_in_usd_ticks"] != float64(158500) || chatUsage["num_sources_used"] != float64(3) || chatUsage["context_details"].(map[string]any)["input_tokens"] != float64(9) {
		t.Fatalf("chat usage = %#v", chatUsage)
	}

	messagesData, err := ConvertResponseJSON(body, OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var messages map[string]any
	_ = json.Unmarshal(messagesData, &messages)
	content := messages["content"].([]any)
	if messages["type"] != "message" || messages["stop_reason"] != "tool_use" || content[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("messages = %#v", messages)
	}
	messagesUsage := messages["usage"].(map[string]any)
	if messagesUsage["cost_in_usd_ticks"] != float64(158500) || messagesUsage["num_server_side_tools_used"] != float64(2) || messagesUsage["context_details"].(map[string]any)["output_tokens"] != float64(4) {
		t.Fatalf("messages usage = %#v", messagesUsage)
	}
}

func TestConvertResponsesJSONToMessagesThinkingAndStop(t *testing.T) {
	body := []byte(`{
		"id":"response-1","model":"grok-4.5","status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"signature"},
			{"type":"message","content":[{"type":"output_text","text":"ABCSTOPXYZ"}]},
			{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{}"}
		],
		"usage":{"input_tokens":10,"output_tokens":5}
	}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, ResponseOptions{AnthropicThinking: true, StopSequences: []string{"STOP"}})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	content := response["content"].([]any)
	if response["id"] != "msg_response-1" || response["stop_reason"] != "tool_use" || len(content) != 3 {
		t.Fatalf("response = %#v", response)
	}
	thinking := content[0].(map[string]any)
	tool := content[2].(map[string]any)
	if thinking["type"] != "thinking" || thinking["signature"] != "signature" || content[1].(map[string]any)["text"] != "ABC" || tool["id"] != "toolu_call_1" {
		t.Fatalf("content = %#v", content)
	}
}

func TestConvertResponsesJSONUsesRawReasoningContentBeforeSummary(t *testing.T) {
	body := []byte(`{
		"id":"resp_reasoning","model":"grok-4.5","status":"completed",
		"output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"raw thought"}],"summary":[{"type":"summary_text","text":"summary"}]}]
	}`)
	data, err := ConvertResponseJSON(body, OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["reasoning_content"] != "raw thought" {
		t.Fatalf("message = %#v", message)
	}
}

func TestConvertResponsesRefusalAcrossChatAndMessages(t *testing.T) {
	body := []byte(`{"id":"resp_refusal","status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"Cannot comply"}]}]}`)
	chatData, err := ConvertResponseJSON(body, OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	_ = json.Unmarshal(chatData, &chat)
	choice := chat["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "content_filter" || choice["message"].(map[string]any)["refusal"] != "Cannot comply" {
		t.Fatalf("chat refusal = %#v", chat)
	}
	messagesData, err := ConvertResponseJSON(body, OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var messages map[string]any
	_ = json.Unmarshal(messagesData, &messages)
	if messages["stop_reason"] != "refusal" {
		t.Fatalf("messages refusal = %#v", messages)
	}
}

func TestConvertResponsesStreamRefusalAcrossChatAndMessages(t *testing.T) {
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_refusal","model":"grok-4.5"}}`, "",
		`event: response.refusal.delta`,
		`data: {"type":"response.refusal.delta","delta":"Cannot comply"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	chat, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(source)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	if text := string(chat); !strings.Contains(text, `"refusal":"Cannot comply"`) || !strings.Contains(text, `"finish_reason":"content_filter"`) {
		t.Fatalf("chat refusal stream = %s", text)
	}
	messages, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(source)), OperationMessages))
	if err != nil {
		t.Fatal(err)
	}
	if text := string(messages); !strings.Contains(text, `"text":"Cannot comply"`) || !strings.Contains(text, `"stop_reason":"refusal"`) {
		t.Fatalf("messages refusal stream = %s", text)
	}
}

func TestConvertResponsesJSONToMessagesStopSequence(t *testing.T) {
	body := []byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ABCSTOPXYZ"}]}]}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, ResponseOptions{StopSequences: []string{"STOP"}})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	_ = json.Unmarshal(data, &response)
	if response["stop_reason"] != "stop_sequence" || response["stop_sequence"] != "STOP" || response["content"].([]any)[0].(map[string]any)["text"] != "ABC" {
		t.Fatalf("response = %#v", response)
	}
}

func TestConvertResponsesJSONToMessagesNormalizesErrorType(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		want        string
		wantMessage string
	}{
		{name: "preserve anthropic type", body: `{"error":{"message":"auth","type":"authentication_error"}}`, want: "authentication_error"},
		{name: "map openai type", body: `{"error":{"message":"invalid","type":"unsupported_parameter"}}`, want: "invalid_request_error"},
		{name: "map upstream code", body: `{"error":{"message":"limited","code":"rate_limit_exceeded"}}`, want: "rate_limit_error"},
		{name: "hide private type", body: `{"error":{"message":"failed","type":"private_internal"}}`, want: "api_error"},
		{name: "preserve string message", body: `{"error":"plain upstream failure"}`, want: "api_error", wantMessage: "plain upstream failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := ConvertResponseJSON([]byte(test.body), OperationMessages)
			if err != nil {
				t.Fatal(err)
			}
			var response map[string]any
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatal(err)
			}
			errorObject := response["error"].(map[string]any)
			if errorObject["type"] != test.want {
				t.Fatalf("error = %#v", response)
			}
			if test.wantMessage != "" && errorObject["message"] != test.wantMessage {
				t.Fatalf("error message = %#v", response)
			}
		})
	}
}

func TestConvertResponsesStream(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5","status":"in_progress"}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hi"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`, "", "",
	}, "\n")
	for _, operation := range []string{OperationChat, OperationMessages} {
		converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), operation))
		if err != nil {
			t.Fatal(err)
		}
		value := string(converted)
		if operation == OperationChat && (!strings.Contains(value, `"object":"chat.completion.chunk"`) || !strings.Contains(value, "data: [DONE]")) {
			t.Fatalf("chat stream = %s", value)
		}
		if operation == OperationMessages && (!strings.Contains(value, "event: message_start") || !strings.Contains(value, "event: content_block_delta") || !strings.Contains(value, "event: message_stop")) {
			t.Fatalf("messages stream = %s", value)
		}
	}
}

func TestConvertResponsesStreamChatErrorIsTerminal(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5","status":"in_progress"}}`, "",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"message":"upstream failed"}}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"late delta"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	value := string(converted)
	if !strings.Contains(value, `"error":{"message":"upstream failed","type":"api_error"}`) {
		t.Fatalf("missing normalized upstream failure: %s", value)
	}
	if strings.Contains(value, `"finish_reason":"stop"`) {
		t.Fatalf("error stream must not end successfully: %s", value)
	}
	if strings.Contains(value, "late delta") {
		t.Fatalf("events after an error must be ignored: %s", value)
	}
	if strings.Count(value, "data: [DONE]") != 1 {
		t.Fatalf("error stream must send one terminator: %s", value)
	}
}

func TestConvertResponsesStreamMessagesNormalizesTerminalError(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"message":"quota denied","code":"forbidden"}}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationMessages))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if !strings.Contains(text, `event: error`) || !strings.Contains(text, `"type":"permission_error"`) || !strings.Contains(text, `"message":"quota denied"`) {
		t.Fatalf("messages error stream = %s", text)
	}
	if strings.Contains(text, "message_stop") {
		t.Fatalf("failed stream must not emit message_stop: %s", text)
	}
}

func TestConvertResponsesStreamToMessagesThinkingToolsAndStop(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"response-1","model":"grok-4.5","usage":{"input_tokens":3}}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"reasoning-1","type":"reasoning"}}`, "",
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"reasoning-1","delta":"thought"}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"reasoning-1","type":"reasoning","encrypted_content":"signature"}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"ABCST"}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"OPXYZ"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(stream)), OperationMessages, ResponseOptions{AnthropicThinking: true, StopSequences: []string{"STOP"}}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	ordered := []string{"message_start", "thinking_delta", "signature_delta", "text_delta", "message_delta", "message_stop"}
	position := 0
	for _, expected := range ordered {
		index := strings.Index(text[position:], expected)
		if index < 0 {
			t.Fatalf("%q 缺失或乱序:\n%s", expected, text)
		}
		position += index + len(expected)
	}
	if strings.Contains(text, "XYZ") || !strings.Contains(text, `"text":"ABC"`) || !strings.Contains(text, `"stop_reason":"stop_sequence"`) || !strings.Contains(text, `"stop_sequence":"STOP"`) {
		t.Fatalf("stream = %s", text)
	}
}

func TestConvertResponsesStreamEmitsDoneOnlyToolArguments(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"Read","arguments":""}}`, "",
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"item_1","arguments":"{\"path\":\"README.md\"}"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(stream)), OperationMessages, ResponseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if !strings.Contains(text, `"id":"toolu_call_1"`) || !strings.Contains(text, `"partial_json":"{\"path\":\"README.md\"}"`) || !strings.Contains(text, `"stop_reason":"tool_use"`) {
		t.Fatalf("stream = %s", text)
	}
	if strings.Count(text, `"type":"content_block_stop"`) != 1 {
		t.Fatalf("tool block closed multiple times: %s", text)
	}
}

func TestConvertResponsesStreamChatUsesContiguousToolIndexes(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"reasoning_1","type":"reasoning"}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{}"}}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":2,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{}"}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if !strings.Contains(text, `"tool_calls":[{"function":{"arguments":"","name":"Read"},"id":"call_1","index":0`) || strings.Contains(text, `"index":2`) {
		t.Fatalf("chat tool stream = %s", text)
	}
}

func TestConvertResponsesStreamChatPreservesAnnotations(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5"}}`, "",
		`event: response.output_text.annotation.added`,
		`data: {"type":"response.output_text.annotation.added","annotation":{"type":"url_citation","url":"https://example.com","title":"Example"}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	if text := string(converted); !strings.Contains(text, `"annotations":[{"title":"Example","type":"url_citation","url":"https://example.com"}]`) {
		t.Fatalf("chat annotation stream = %s", text)
	}
}

func TestConvertResponsesStreamMessagesInputTokens(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5","status":"in_progress"}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","usage":{"input_tokens":194,"output_tokens":7,"cost_in_usd_ticks":9000,"context_details":{"input_tokens":180,"output_tokens":6}}}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationMessages))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)

	if !strings.Contains(text, `"input_tokens":194`) {
		t.Fatalf("message_delta should contain input_tokens from response.completed usage:\n%s", text)
	}
	if !strings.Contains(text, `"cost_in_usd_ticks":9000`) || !strings.Contains(text, `"input_tokens":180`) {
		t.Fatalf("message_delta should retain upstream usage extensions:\n%s", text)
	}
}

func TestConvertResponsesStreamMergesPartialUsageFrames(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_usage","model":"grok-4.5","status":"in_progress","usage":{"input_tokens":120,"output_tokens":30,"total_tokens":150,"cost_in_usd_ticks":9000,"output_tokens_details":{"reasoning_tokens":12},"context_details":{"input_tokens":110,"output_tokens":25}}}}`, "",
		`event: response.in_progress`,
		`data: {"type":"response.in_progress","response":{"usage":{"input_tokens_details":{"cached_tokens":80}}}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_usage","model":"grok-4.5","status":"completed"}}`, "", "",
	}, "\n")

	tests := []struct {
		operation string
		want      []string
	}{
		{operation: OperationChat, want: []string{`"prompt_tokens":120`, `"completion_tokens":30`, `"cached_tokens":80`, `"reasoning_tokens":12`, `"cost_in_usd_ticks":9000`}},
		{operation: OperationMessages, want: []string{`"input_tokens":120`, `"output_tokens":30`, `"cache_read_input_tokens":80`, `"cost_in_usd_ticks":9000`}},
	}
	for _, test := range tests {
		converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), test.operation))
		if err != nil {
			t.Fatalf("%s conversion: %v", test.operation, err)
		}
		text := string(converted)
		for _, want := range test.want {
			if !strings.Contains(text, want) {
				t.Fatalf("%s partial usage lost %s:\n%s", test.operation, want, text)
			}
		}
	}
}
