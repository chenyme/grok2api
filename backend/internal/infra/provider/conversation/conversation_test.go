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
	if payload["model"] != "grok-4.5" || payload["max_output_tokens"] != float64(512) || payload["stream"] != true {
		t.Fatalf("request fields = %#v", payload)
	}
	input := payload["input"].([]any)
	if len(input) != 4 || input[2].(map[string]any)["type"] != "function_call" || input[3].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input = %#v", input)
	}
	content := input[1].(map[string]any)["content"].([]any)
	if content[1].(map[string]any)["image_url"] != "data:image/png;base64,AA==" {
		t.Fatalf("image content = %#v", content)
	}
	tools := payload["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "lookup" || tools[0].(map[string]any)["type"] != "function" {
		t.Fatalf("tools = %#v", tools)
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
		!strings.Contains(output[2].(map[string]any)["text"].(string), `"Read"`) || output[3].(map[string]any)["type"] != "input_image" {
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

func TestConvertAnthropicMessagesRejectsUnrepresentableTopK(t *testing.T) {
	_, _, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"top_k":10,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.5", OperationMessages)
	if err == nil || !strings.Contains(err.Error(), "top_k") {
		t.Fatalf("error = %v", err)
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
	if tool["type"] != "web_search" || tool["max_uses"] != float64(3) || len(tool["allowed_domains"].([]any)) != 1 || tool["user_location"].(map[string]any)["country"] != "US" {
		t.Fatalf("tool = %#v", tool)
	}

	_, _, err = ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"messages":[{"role":"user","content":"search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","search_context_size":"high"}]
	}`), "grok-4.5", OperationMessages)
	if err == nil || !strings.Contains(err.Error(), "search_context_size") {
		t.Fatalf("error = %v", err)
	}
}

func TestConvertResponsesJSONToChatAndMessages(t *testing.T) {
	body := []byte(`{
		"id":"resp_1","object":"response","created_at":123,"model":"grok-4.5","status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"reason"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},
			{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}
		],
		"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}
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
	if !strings.Contains(value, `"type":"response.failed"`) {
		t.Fatalf("missing upstream failure: %s", value)
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

func TestConvertResponsesStreamMessagesInputTokens(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5","status":"in_progress"}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","usage":{"input_tokens":194,"output_tokens":7}}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationMessages))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)

	if !strings.Contains(text, `"input_tokens":194`) {
		t.Fatalf("message_delta should contain input_tokens from response.completed usage:\n%s", text)
	}
}
