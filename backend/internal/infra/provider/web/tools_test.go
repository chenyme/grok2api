package web

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
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

func TestParseToolConfigurationAllowsRequiredHostedWebSearch(t *testing.T) {
	configuration, err := parseToolConfiguration(
		json.RawMessage(`[{"type":"web_search","max_uses":8}]`),
		json.RawMessage(`"required"`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Choice != "required" || len(configuration.Functions) != 0 || len(configuration.ResponseTools) != 1 {
		t.Fatalf("configuration = %#v", configuration)
	}
}

func TestInjectToolPromptDoesNotForceClientFunctionWhenHostedSearchIsRequired(t *testing.T) {
	configuration, err := parseToolConfiguration(
		json.RawMessage(`[
			{"type":"web_search"},
			{"type":"function","name":"lookup","description":"Look up local data","parameters":{"type":"object"}}
		]`),
		json.RawMessage(`"required"`),
	)
	if err != nil {
		t.Fatal(err)
	}
	prompt := injectToolPrompt("find the latest release", configuration)
	if !strings.Contains(prompt, "Tool: lookup") {
		t.Fatalf("client function definition missing from prompt: %s", prompt)
	}
	if strings.Contains(prompt, "You MUST call at least one available tool") {
		t.Fatalf("hosted-search required incorrectly forced a client function: %s", prompt)
	}
	if !strings.Contains(prompt, "Call a tool when it is clearly needed") {
		t.Fatalf("mixed hosted/client tool guidance is not optional: %s", prompt)
	}
}

func TestAnthropicWebSearchRequestConvertsForWebProvider(t *testing.T) {
	converted, options, err := conversation.ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"stream":true,
		"messages":[{"role":"user","content":"Perform a web search for the query: rust tutorials"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`), "grok-chat-fast", conversation.OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var input openAIRequest
	if err := json.Unmarshal(converted, &input); err != nil {
		t.Fatal(err)
	}
	configuration, err := parseToolConfiguration(input.Tools, input.ToolChoice)
	if err != nil {
		t.Fatal(err)
	}
	if !options.AnthropicWebSearch || !options.AnthropicWebSearchRequired || options.AnthropicWebSearchQuery != "rust tutorials" || !configuration.HostedWebSearch || configuration.Choice != "required" {
		t.Fatalf("options=%#v configuration=%#v", options, configuration)
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

func TestBuildMessagesResultEmitsServerWebSearchBlocks(t *testing.T) {
	options := webSearchResponseOptions(t)
	parsed := parsedChat{ServerTools: 1, SearchSources: []map[string]any{
		{"url": "https://doc.rust-lang.org", "title": "The Rust Book", "type": "web"},
	}}
	parsed.Text.WriteString("Here you go.")
	result := buildOpenAIResult("messages", "resp_test", "grok-chat-fast", parsed, false, options)
	content := result["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %#v", content)
	}
	use := content[0].(map[string]any)
	if use["type"] != "server_tool_use" || use["name"] != "web_search" || use["input"].(map[string]any)["query"] != "rust tutorials" {
		t.Fatalf("server_tool_use = %#v", use)
	}
	searchResult := content[1].(map[string]any)
	if searchResult["type"] != "web_search_tool_result" || searchResult["tool_use_id"] != use["id"] {
		t.Fatalf("web_search_tool_result = %#v", searchResult)
	}
	hits := searchResult["content"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["url"] != "https://doc.rust-lang.org" {
		t.Fatalf("hits = %#v", hits)
	}
	if content[2].(map[string]any)["type"] != "text" || result["stop_reason"] != "end_turn" {
		t.Fatalf("result = %#v", result)
	}
	usage := result["usage"].(map[string]any)["server_tool_use"].(map[string]any)
	if usage["web_search_requests"] != int64(1) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestBuildMessagesResultEmitsUnavailableWebSearchError(t *testing.T) {
	result := buildOpenAIResult("messages", "resp_test", "grok-chat-fast", parsedChat{}, false, webSearchResponseOptions(t))
	content := result["content"].([]any)
	errorContent := content[1].(map[string]any)["content"].(map[string]any)
	if errorContent["type"] != "web_search_tool_result_error" || errorContent["error_code"] != "unavailable" {
		t.Fatalf("error content = %#v", errorContent)
	}
}

func TestBuildMessagesResultDoesNotFabricateOptionalWebSearch(t *testing.T) {
	parsed := parsedChat{}
	parsed.Text.WriteString("Plain answer.")
	result := buildOpenAIResult("messages", "resp_test", "grok-chat-fast", parsed, false, conversation.ResponseOptions{
		AnthropicWebSearch: true, AnthropicWebSearchQuery: "optional query",
	})
	content := result["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "Plain answer." {
		t.Fatalf("optional search fabricated server blocks: %#v", content)
	}
	if _, exists := result["usage"].(map[string]any)["server_tool_use"]; exists {
		t.Fatalf("optional search fabricated usage: %#v", result["usage"])
	}
}

func TestBuildMessagesResultDoesNotTreatOtherServerToolsAsWebSearch(t *testing.T) {
	parsed := parsedChat{ServerTools: 1}
	parsed.Text.WriteString("Generated an image instead.")
	result := buildOpenAIResult("messages", "resp_test", "grok-chat-fast", parsed, false, conversation.ResponseOptions{
		AnthropicWebSearch: true, AnthropicWebSearchQuery: "optional query",
	})
	content := result["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("non-search server tool fabricated web search blocks: %#v", content)
	}
	if _, exists := result["usage"].(map[string]any)["server_tool_use"]; exists {
		t.Fatalf("non-search server tool fabricated web search usage: %#v", result["usage"])
	}

	required := buildOpenAIResult("messages", "resp_test", "grok-chat-fast", parsed, false, webSearchResponseOptions(t))
	requiredContent := required["content"].([]any)
	errorContent, ok := requiredContent[1].(map[string]any)["content"].(map[string]any)
	if !ok || errorContent["type"] != "web_search_tool_result_error" || errorContent["error_code"] != "unavailable" {
		t.Fatalf("forced search with only a non-search server tool = %#v", requiredContent)
	}
}

func TestBuildMessagesResultFiltersUnsafeAndDuplicateSearchSources(t *testing.T) {
	options := webSearchResponseOptions(t)
	parsed := parsedChat{ServerTools: 1, SearchSources: []map[string]any{
		{"url": "https://example.com/a", "title": "Example"},
		{"url": "https://example.com/a", "title": "Duplicate"},
		{"url": "javascript:alert(1)", "title": "Script"},
		{"url": "file:///etc/passwd", "title": "File"},
		{"url": "/relative/path", "title": "Relative"},
		{"url": "https://user:secret@example.com/private", "title": "Credential URL"},
	}}
	result := buildOpenAIResult("messages", "resp_test", "grok-chat-fast", parsed, false, options)
	content := result["content"].([]any)
	hits := content[1].(map[string]any)["content"].([]any)
	if len(hits) != 1 {
		t.Fatalf("unsafe or duplicate sources leaked into hits: %#v", hits)
	}
	if hits[0].(map[string]any)["url"] != "https://example.com/a" {
		t.Fatalf("hits = %#v", hits)
	}
}

func TestBuildMessagesResultBoundsSearchSourcesAndTitles(t *testing.T) {
	parsed := parsedChat{ServerTools: 1, SearchSources: make([]map[string]any, 0, 60)}
	for index := 0; index < 60; index++ {
		parsed.SearchSources = append(parsed.SearchSources, map[string]any{
			"url":   "https://example.com/" + strconv.Itoa(index),
			"title": strings.Repeat("界", 600),
		})
	}
	blocks := webMessagesSearchBlocks("srvtoolu_test", parsed, webSearchResponseOptions(t))
	hits := blocks[1].(map[string]any)["content"].([]any)
	if len(hits) != 50 {
		t.Fatalf("hits = %d, want 50", len(hits))
	}
	if got := utf8.RuneCountInString(hits[0].(map[string]any)["title"].(string)); got != 512 {
		t.Fatalf("title runes = %d, want 512", got)
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

func TestWebMessagesStreamEmitsServerWebSearchBeforeBufferedText(t *testing.T) {
	var output bytes.Buffer
	stream := newWebMessagesStream(&output, "resp_test", "grok-chat-fast", 3, webSearchResponseOptions(t))
	if err := stream.Delta("text", "Here you go."); err != nil {
		t.Fatal(err)
	}
	parsed := parsedChat{ServerTools: 1, SearchSources: []map[string]any{
		{"url": "https://doc.rust-lang.org", "title": "The Rust Book", "type": "web"},
	}}
	if err := stream.Finish(parsed, map[string]any{"usage": map[string]any{"output_tokens": int64(2)}}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	useAt := strings.Index(text, `"type":"server_tool_use"`)
	resultAt := strings.Index(text, `"type":"web_search_tool_result"`)
	textAt := strings.Index(text, `"content_block":{"text":"","type":"text"}`)
	if useAt < 0 || resultAt < 0 || textAt < 0 || !(useAt < resultAt && resultAt < textAt) {
		t.Fatalf("expected server_tool_use -> result -> text, got:\n%s", text)
	}
	if !strings.Contains(text, "rust tutorials") || !strings.Contains(text, `"web_search_requests":1`) {
		t.Fatalf("missing query or usage:\n%s", text)
	}
}

func TestWebMessagesStreamKeepsThinkingBeforeServerWebSearch(t *testing.T) {
	options := webSearchResponseOptions(t)
	options.AnthropicThinking = true
	var output bytes.Buffer
	stream := newWebMessagesStream(&output, "resp_test", "grok-chat-fast", 3, options)
	if err := stream.Delta("reasoning", "Need sources."); err != nil {
		t.Fatal(err)
	}
	if err := stream.Delta("text", "Here you go."); err != nil {
		t.Fatal(err)
	}
	parsed := parsedChat{ServerTools: 1, SearchSources: []map[string]any{
		{"url": "https://doc.rust-lang.org", "title": "The Rust Book"},
	}}
	if err := stream.Finish(parsed, map[string]any{"usage": map[string]any{"output_tokens": int64(2)}}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	thinkingAt := strings.Index(text, `"type":"thinking"`)
	useAt := strings.Index(text, `"type":"server_tool_use"`)
	resultAt := strings.Index(text, `"type":"web_search_tool_result"`)
	textAt := strings.Index(text, `"content_block":{"text":"","type":"text"}`)
	if thinkingAt < 0 || useAt < 0 || resultAt < 0 || textAt < 0 || !(thinkingAt < useAt && useAt < resultAt && resultAt < textAt) {
		t.Fatalf("expected thinking -> server_tool_use -> result -> text, got:\n%s", text)
	}
}

func TestWebMessagesStreamAppliesStopSequenceAfterServerWebSearch(t *testing.T) {
	options := webSearchResponseOptions(t)
	options.StopSequences = []string{"STOP"}
	var output bytes.Buffer
	stream := newWebMessagesStream(&output, "resp_test", "grok-chat-fast", 3, options)
	if err := stream.Delta("text", "ABCST"); err != nil {
		t.Fatal(err)
	}
	if err := stream.Delta("text", "OPXYZ"); err != nil {
		t.Fatal(err)
	}
	parsed := parsedChat{ServerTools: 1, SearchSources: []map[string]any{{"url": "https://example.com", "title": "Example"}}}
	if err := stream.Finish(parsed, map[string]any{"usage": map[string]any{"output_tokens": int64(2)}}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "XYZ") || !strings.Contains(text, `"text":"ABC"`) || !strings.Contains(text, `"stop_reason":"stop_sequence"`) || !strings.Contains(text, `"stop_sequence":"STOP"`) {
		t.Fatalf("stream stop sequence = %s", text)
	}
	useAt := strings.Index(text, `"type":"server_tool_use"`)
	resultAt := strings.Index(text, `"type":"web_search_tool_result"`)
	textAt := strings.Index(text, `"content_block":{"text":"","type":"text"}`)
	if useAt < 0 || resultAt < 0 || textAt < 0 || !(useAt < resultAt && resultAt < textAt) {
		t.Fatalf("web search blocks are out of order: %s", text)
	}
}

func TestWebMessagesStreamRejectsOversizedDeferredText(t *testing.T) {
	var output bytes.Buffer
	stream := newWebMessagesStream(&output, "resp_test", "grok-chat-fast", 3, conversation.ResponseOptions{AnthropicWebSearch: true})
	err := stream.Delta("text", strings.Repeat("x", (8<<20)+1))
	if err == nil || !strings.Contains(err.Error(), "缓冲") {
		t.Fatalf("oversized deferred text error = %v", err)
	}
}

func webSearchResponseOptions(t *testing.T) conversation.ResponseOptions {
	t.Helper()
	_, options, err := conversation.ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"stream":true,
		"messages":[{"role":"user","content":"Perform a web search for the query: rust tutorials"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`), "grok-chat-fast", conversation.OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	return options
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

func TestWebMessagesUsageDoesNotCountNonSearchServerTools(t *testing.T) {
	parsed := parsedChat{
		ServerTools:   3,
		SearchSources: []map[string]any{{"url": "https://example.com", "title": "Example"}},
	}
	if got := webMessagesSearchRequests(parsed); got != 1 {
		t.Fatalf("web_search_requests = %d, want 1", got)
	}
}

func TestCollectServerToolCountsOnlyNamedWebSearchForAnthropicUsage(t *testing.T) {
	parsed := &parsedChat{}
	webCard := map[string]any{
		"rolloutId": "web-1", "messageTag": "tool_usage_card",
		"token": `<xai:tool_usage_card><xai:tool_name>web_search</xai:tool_name><xai:tool_args>{"query":"x"}</xai:tool_args></xai:tool_usage_card>`,
	}
	collectServerTool(parsed, webCard)
	collectServerTool(parsed, webCard)
	collectServerTool(parsed, map[string]any{
		"rolloutId": "image-1", "messageTag": "tool_usage_card",
		"token": `<xai:tool_usage_card><xai:tool_name>search_images</xai:tool_name></xai:tool_usage_card>`,
	})
	collectServerTool(parsed, map[string]any{
		"rolloutId": "web-2", "messageTag": "tool_usage_card", "toolName": "web_search",
	})
	if parsed.ServerTools != 3 || parsed.WebSearchTools != 2 || webMessagesSearchRequests(*parsed) != 2 {
		t.Fatalf("server=%d web=%d usage=%d", parsed.ServerTools, parsed.WebSearchTools, webMessagesSearchRequests(*parsed))
	}
}

func TestCollectServerToolEnrichesEarlierUnknownCard(t *testing.T) {
	parsed := &parsedChat{}
	collectServerTool(parsed, map[string]any{
		"rolloutId": "web-late", "messageTag": "tool_usage_card",
	})
	collectServerTool(parsed, map[string]any{
		"rolloutId": "web-late", "messageTag": "tool_usage_card",
		"token": `<xai:tool_usage_card><xai:tool_name>web_search</xai:tool_name></xai:tool_usage_card>`,
	})
	if parsed.ServerTools != 1 || parsed.WebSearchTools != 1 || webMessagesSearchRequests(*parsed) != 1 {
		t.Fatalf("server=%d web=%d usage=%d", parsed.ServerTools, parsed.WebSearchTools, webMessagesSearchRequests(*parsed))
	}
}

func TestCollectServerToolDistinguishesTokenCardsWithoutRolloutID(t *testing.T) {
	parsed := &parsedChat{}
	for _, name := range []string{"web_search", "search_images"} {
		collectServerTool(parsed, map[string]any{
			"messageStepId": 1.0, "messageTag": "tool_usage_card",
			"token": `<xai:tool_usage_card><xai:tool_name>` + name + `</xai:tool_name></xai:tool_usage_card>`,
		})
	}
	if parsed.ServerTools != 2 || parsed.WebSearchTools != 1 {
		t.Fatalf("server=%d web=%d", parsed.ServerTools, parsed.WebSearchTools)
	}
}

func TestCollectServerToolBoundsTrackingState(t *testing.T) {
	parsed := &parsedChat{}
	for index := 0; index < maxTrackedServerTools+10; index++ {
		collectServerTool(parsed, map[string]any{
			"rolloutId": "web-" + strconv.Itoa(index), "messageTag": "tool_usage_card", "toolName": "web_search",
		})
	}
	if len(parsed.serverToolKeys) != maxTrackedServerTools || len(parsed.webSearchKeys) != maxTrackedServerTools || parsed.ServerTools != maxTrackedServerTools || parsed.WebSearchTools != maxTrackedServerTools {
		t.Fatalf("server keys=%d web keys=%d server=%d web=%d", len(parsed.serverToolKeys), len(parsed.webSearchKeys), parsed.ServerTools, parsed.WebSearchTools)
	}
}

func TestCollectSearchSourcesNormalizesFiltersAndBoundsInput(t *testing.T) {
	results := make([]any, 0, searchresult.MaxResults+12)
	results = append(results,
		map[string]any{"url": "javascript:alert(1)", "title": "Script"},
		map[string]any{"url": "https://user:secret@example.com/private", "title": "Credential"},
	)
	for index := 0; index < searchresult.MaxResults+10; index++ {
		results = append(results, map[string]any{
			"url":   "HTTPS://Example.COM/" + strconv.Itoa(index),
			"title": strings.Repeat("界", searchresult.MaxTitleRunes+10),
		})
	}
	parsed := &parsedChat{}
	collectSearchSources(parsed, map[string]any{"webSearchResults": map[string]any{"results": results}})
	if len(parsed.SearchSources) != searchresult.MaxResults || len(parsed.sourceKeys) != searchresult.MaxResults {
		t.Fatalf("sources=%d keys=%d, want %d", len(parsed.SearchSources), len(parsed.sourceKeys), searchresult.MaxResults)
	}
	first := parsed.SearchSources[0]
	if first["url"] != "https://example.com/0" || utf8.RuneCountInString(first["title"].(string)) != searchresult.MaxTitleRunes {
		t.Fatalf("first normalized source = %#v", first)
	}
	for _, source := range parsed.SearchSources {
		if strings.Contains(source["url"].(string), "@") || strings.HasPrefix(source["url"].(string), "javascript:") {
			t.Fatalf("unsafe source retained: %#v", source)
		}
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

func TestGrokRenderCitationRejectsUnsafeURLs(t *testing.T) {
	for _, rawURL := range []string{
		"javascript:alert(1)",
		"file:///etc/passwd",
		"https://user:secret@example.com/private",
		"/relative/path",
	} {
		parsed := &parsedChat{cardCache: map[string]map[string]any{
			"cite_unsafe": {"id": "cite_unsafe", "url": rawURL},
		}}
		replacement, annotation := renderChatCard(parsed, "cite_unsafe", "render_inline_citation")
		if replacement != "" || annotation != nil {
			t.Fatalf("unsafe citation %q rendered as %q, %#v", rawURL, replacement, annotation)
		}
	}
}
