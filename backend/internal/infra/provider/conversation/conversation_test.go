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
