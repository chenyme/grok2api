package web

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func TestParseToolConfigurationSupportsChatAndResponsesSchemas(t *testing.T) {
	chatTools := json.RawMessage(`[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]`)
	chatChoice := json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`)
	chat, err := parseToolConfiguration(chatTools, chatChoice)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Functions) != 1 || chat.Functions[0].Name != "get_weather" || chat.ForcedName != "get_weather" || chat.Choice != "required" {
		t.Fatalf("chat tool config = %#v", chat)
	}

	responsesTools := json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"web_search_preview"}]`)
	responses, err := parseToolConfiguration(responsesTools, json.RawMessage(`"auto"`))
	if err != nil {
		t.Fatal(err)
	}
	if len(responses.Functions) != 1 || responses.Functions[0].Name != "lookup" || len(responses.ResponseTools) != 2 {
		t.Fatalf("responses tool config = %#v", responses)
	}
}

func TestNormalizeOpenAIInputReconstructsToolHistory(t *testing.T) {
	toolCalls := json.RawMessage(`[{"id":"call_old","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"xAI\"}"}}]`)
	assistantContent, _ := json.Marshal("")
	toolContent, _ := json.Marshal("result text")
	chat, err := normalizeOpenAIInput(openAIRequest{Messages: []chatMessage{
		{Role: "assistant", Content: assistantContent, ToolCalls: toolCalls},
		{Role: "tool", Content: toolContent, ToolCallID: "call_old"},
		{Role: "user", Content: json.RawMessage(`"continue"`)},
	}}, "chat")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"<tool_name>lookup</tool_name>", `{"query":"xAI"}`, "Tool result (call_old): result text", "[user]\ncontinue"} {
		if !strings.Contains(chat.Prompt, expected) {
			t.Fatalf("chat prompt missing %q: %s", expected, chat.Prompt)
		}
	}

	input := json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"query\":\"Grok\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"done"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"summarize"}]}
	]`)
	responses, err := normalizeOpenAIInput(openAIRequest{Input: input}, "responses")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"<tool_name>lookup</tool_name>", "[tool result for call_1]", "done", "summarize"} {
		if !strings.Contains(responses.Prompt, expected) {
			t.Fatalf("responses prompt missing %q: %s", expected, responses.Prompt)
		}
	}
}

func TestParseToolCallsBuildsStandardResults(t *testing.T) {
	available := map[string]struct{}{"lookup": {}}
	parsedResult := parseToolCalls(`prefix<tool_calls><tool_call><tool_name>lookup</tool_name><parameters>{"query":"Grok"}</parameters></tool_call></tool_calls>`, available)
	if len(parsedResult.Calls) != 1 || parsedResult.Calls[0].Name != "lookup" || parsedResult.Calls[0].Arguments != `{"query":"Grok"}` {
		t.Fatalf("tool parse = %#v", parsedResult)
	}
	parsed := parsedChat{ToolCalls: parsedResult.Calls, ParallelTools: true}
	chat := buildOpenAIResult("chat", "resp_test", "grok-chat-fast", parsed, false)
	choice := chat["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || len(message["tool_calls"].([]any)) != 1 || message["content"] != nil {
		t.Fatalf("chat tool result = %#v", chat)
	}
	responses := buildOpenAIResult("responses", "resp_test", "grok-chat-fast", parsed, false)
	output := responses["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["type"] != "function_call" {
		t.Fatalf("responses tool result = %#v", responses)
	}
	messages := buildOpenAIResult("messages", "resp_test", "grok-chat-fast", parsed, false)
	content := messages["content"].([]any)
	if messages["type"] != "message" || messages["stop_reason"] != "tool_use" || content[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("messages tool result = %#v", messages)
	}
}

func TestBuildMessagesResultPreservesThinkingAndStopSequence(t *testing.T) {
	parsed := parsedChat{}
	parsed.Reasoning.WriteString("thought")
	parsed.Text.WriteString("ABCSTOPXYZ")
	result := buildOpenAIResult("messages", "resp_test", "grok-chat-fast", parsed, false, conversation.ResponseOptions{
		AnthropicThinking: true, StopSequences: []string{"STOP"},
	})
	content := result["content"].([]any)
	if result["stop_reason"] != "stop_sequence" || result["stop_sequence"] != "STOP" || len(content) != 2 {
		t.Fatalf("result = %#v", result)
	}
	if content[0].(map[string]any)["type"] != "thinking" || content[1].(map[string]any)["text"] != "ABC" {
		t.Fatalf("content = %#v", content)
	}
}

func TestWebMessagesStreamPreservesThinkingAndSplitStopSequence(t *testing.T) {
	var output bytes.Buffer
	stream := newWebMessagesStream(&output, "resp_test", "grok-chat-fast", 3, conversation.ResponseOptions{
		AnthropicThinking: true, StopSequences: []string{"STOP"},
	})
	for _, delta := range []struct{ kind, value string }{
		{kind: "reasoning", value: "thought"},
		{kind: "text", value: "ABCST"},
		{kind: "text", value: "OPXYZ"},
	} {
		if err := stream.Delta(delta.kind, delta.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Finish(parsedChat{}, map[string]any{"usage": map[string]any{"output_tokens": int64(2)}}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "XYZ") || !strings.Contains(text, `"thinking":"thought"`) || !strings.Contains(text, `"text":"ABC"`) || !strings.Contains(text, `"stop_reason":"stop_sequence"`) {
		t.Fatalf("stream = %s", text)
	}
}

func TestToolStreamSieveHandlesSplitXML(t *testing.T) {
	sieve := newToolStreamSieve(map[string]struct{}{"lookup": {}})
	first := sieve.Feed("before <tool_ca")
	if first.SafeText != "before " || first.Complete {
		t.Fatalf("first = %#v", first)
	}
	second := sieve.Feed(`lls><tool_call><tool_name>lookup</tool_name><parameters>{"q":"x"}</parameters></tool_call></tool_calls>`)
	if !second.Complete || len(second.Calls) != 1 || second.Calls[0].Arguments != `{"q":"x"}` || strings.Contains(second.SafeText, "tool_calls") {
		t.Fatalf("second = %#v", second)
	}
}

func TestToolStreamSievePreservesInvalidSyntaxAndTrailingText(t *testing.T) {
	sieve := newToolStreamSieve(map[string]struct{}{"lookup": {}})
	result := sieve.Feed(`<tool_calls><tool_call><tool_name>unknown</tool_name><parameters>{}</parameters></tool_call></tool_calls> trailing`)
	if !result.Complete || len(result.Calls) != 0 || !strings.HasSuffix(result.Raw, " trailing") {
		t.Fatalf("result = %#v", result)
	}
}

func TestSearchSourcesAndServerToolsAreDeduplicated(t *testing.T) {
	parsed := &parsedChat{}
	response := map[string]any{
		"webSearchResults": map[string]any{"results": []any{
			map[string]any{"url": "https://example.com", "title": "Example"},
			map[string]any{"url": "https://example.com", "title": "Duplicate"},
		}},
		"xSearchResults": map[string]any{"results": []any{
			map[string]any{"username": "xai", "postId": "123", "text": "post"},
		}},
	}
	collectSearchSources(parsed, response)
	collectSearchSources(parsed, response)
	if len(parsed.SearchSources) != 2 {
		t.Fatalf("search sources = %#v", parsed.SearchSources)
	}
	toolCard := map[string]any{"rolloutId": "search", "messageStepId": 1.0}
	collectServerTool(parsed, toolCard)
	collectServerTool(parsed, toolCard)
	if parsed.ServerTools != 1 {
		t.Fatalf("server tools = %d", parsed.ServerTools)
	}
}

func TestGeneratedImageURLsCollectAllCandidates(t *testing.T) {
	parsed := &parsedChat{}
	frame := map[string]any{"result": map[string]any{"response": map[string]any{
		"modelResponse": map[string]any{"generatedImageUrls": []any{"one.jpg", "two.jpg"}},
	}}}
	data, _ := json.Marshal(frame)
	kind, delta, err := parseUpstreamFrame(data, parsed)
	if err != nil || kind != "image" || delta != "https://assets.grok.com/one.jpg" || len(parsed.Images) != 2 {
		t.Fatalf("kind=%q delta=%q images=%#v err=%v", kind, delta, parsed.Images, err)
	}
}

func TestGrokRenderCitationBecomesMarkdownAndAnnotation(t *testing.T) {
	parsed := &parsedChat{}
	cardFrame := map[string]any{
		"result": map[string]any{
			"response": map[string]any{
				"cardAttachment":   map[string]any{"jsonData": `{"id":"cite_1","url":"https://example.com"}`},
				"webSearchResults": map[string]any{"results": []any{map[string]any{"url": "https://example.com", "title": "Example"}}},
			},
		},
	}
	cardData, _ := json.Marshal(cardFrame)
	if _, _, err := parseUpstreamFrame(cardData, parsed); err != nil {
		t.Fatal(err)
	}
	tokenFrame := map[string]any{
		"result": map[string]any{
			"response": map[string]any{
				"token":      `Answer<grok:render card_id="cite_1" card_type="citation" type="render_inline_citation"></grok:render>`,
				"isThinking": false,
				"messageTag": "final",
			},
		},
	}
	tokenData, _ := json.Marshal(tokenFrame)
	kind, delta, err := parseUpstreamFrame(tokenData, parsed)
	if err != nil || kind != "text" || delta != "Answer [[1]](https://example.com)" || len(parsed.Annotations) != 1 {
		t.Fatalf("kind=%q delta=%q annotations=%#v err=%v", kind, delta, parsed.Annotations, err)
	}
	annotation := parsed.Annotations[0]
	if annotation["title"] != "Example" || annotation["start_index"] != 6 || annotation["end_index"] != len(delta) {
		t.Fatalf("annotation = %#v", annotation)
	}
}
