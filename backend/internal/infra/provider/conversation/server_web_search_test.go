package conversation

import (
	"bufio"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
)

func TestParseAndMapBuildWebSearchCall(t *testing.T) {
	body := []byte(`{
		"id":"resp_ws1","model":"grok-4.5","status":"completed","created_at":123,
		"output":[
			{"type":"web_search_call","id":"ws_abc","status":"completed","action":{
				"type":"search","query":"Claude Fable 5",
				"sources":[
					{"type":"url","url":"https://example.com/a"},
					{"type":"url","url":"https://example.com/b","title":"Beta"}
				]
			}},
			{"type":"web_search_call","id":"ws_abc","status":"completed","action":{"type":"search"}},
			{"type":"web_search_call","id":"ws_abc","status":"completed","action":{"type":"search","query":""}},
			{"type":"message","role":"assistant","content":[
				{"type":"output_text","text":"Fable 5 is public.","annotations":[
					{"type":"url_citation","url":"https://EXAMPLE.com/a","title":"Alpha Title","start_index":0,"end_index":5}
				]}
			]}
		],
		"usage":{"input_tokens":10,"output_tokens":5}
	}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, ResponseOptions{AnthropicWebSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %#v", msg["stop_reason"])
	}
	content, _ := msg["content"].([]any)
	if len(content) < 3 {
		t.Fatalf("content = %#v", content)
	}
	use := content[0].(map[string]any)
	if use["type"] != "server_tool_use" || use["name"] != "web_search" {
		t.Fatalf("server_tool_use = %#v", use)
	}
	input := use["input"].(map[string]any)
	if input["query"] != "Claude Fable 5" {
		t.Fatalf("query = %#v", input)
	}
	result := content[1].(map[string]any)
	if result["type"] != "web_search_tool_result" || result["tool_use_id"] != use["id"] {
		t.Fatalf("result = %#v", result)
	}
	hits, _ := result["content"].([]any)
	if len(hits) != 2 {
		t.Fatalf("hits = %#v", hits)
	}
	h0 := hits[0].(map[string]any)
	if h0["url"] != "https://example.com/a" || h0["title"] != "Alpha Title" {
		t.Fatalf("hit0 title from annotation expected Alpha Title, got %#v", h0)
	}
	text := content[2].(map[string]any)
	if text["type"] != "text" || text["text"] != "Fable 5 is public." {
		t.Fatalf("text = %#v", text)
	}
	usage := msg["usage"].(map[string]any)
	stu := usage["server_tool_use"].(map[string]any)
	if stu["web_search_requests"] != float64(1) {
		t.Fatalf("usage = %#v", usage)
	}
	// Duplicate empty web_search_call items must collapse to one pair of blocks.
	serverUses := 0
	for _, raw := range content {
		if block, _ := raw.(map[string]any); block["type"] == "server_tool_use" {
			serverUses++
		}
	}
	if serverUses != 1 {
		t.Fatalf("expected 1 deduped server_tool_use, got %d in %#v", serverUses, content)
	}
}

func TestMergeAnnotationTitlesSkipsFootnoteLabels(t *testing.T) {
	for _, title := range []string{"123", "Source 1", "citation 2"} {
		calls := []webSearchCall{{Hits: []webSearchHit{{Title: "example.com", URL: "https://example.com/a"}}}}
		merged := mergeAnnotationTitles(calls, []map[string]any{{
			"type": "url_citation", "url": "https://example.com/a", "title": title,
		}})
		if got := merged[0].Hits[0].Title; got != "example.com" {
			t.Fatalf("footnote title %q replaced fallback with %q", title, got)
		}
	}
}

func TestUnrequestedWebSearchItemsAreNotExposed(t *testing.T) {
	body := []byte(`{
		"id":"resp_ws_unrequested","model":"grok-4.5","status":"completed",
		"output":[
			{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"private internal search","sources":[{"url":"https://example.com"}]}},
			{"type":"message","content":[{"type":"output_text","text":"Plain answer."}]}
		],
		"usage":{"input_tokens":3,"output_tokens":2}
	}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, ResponseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	content := msg["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("unrequested server search was exposed: %#v", content)
	}
	if _, exists := msg["usage"].(map[string]any)["server_tool_use"]; exists {
		t.Fatalf("unrequested search usage was exposed: %#v", msg["usage"])
	}
}

func TestUnrequestedWebSearchStreamItemsAreNotExposed(t *testing.T) {
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5"}}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"private internal search","sources":[{"url":"https://example.com"}]}}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Plain answer."}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"private internal search","sources":[{"url":"https://example.com"}]}},{"type":"message","content":[{"type":"output_text","text":"Plain answer."}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
		``,
	}, "\n")
	raw, err := io.ReadAll(ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(source)), OperationMessages, ResponseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "server_tool_use") || strings.Contains(text, "web_search_tool_result") || !strings.Contains(text, "Plain answer.") {
		t.Fatalf("unrequested stream search was exposed:\n%s", text)
	}
}

func TestRequestedBuildSearchWithoutCallEmitsUnavailableResult(t *testing.T) {
	_, options, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,
		"messages":[{"role":"user","content":"Perform a web search for the query: rust tutorials"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
		"id":"resp_no_call","model":"grok-4.5","status":"completed",
		"output":[{"type":"message","content":[{"type":"output_text","text":"Search was unavailable."}]}],
		"usage":{"input_tokens":3,"output_tokens":2}
	}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, options)
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	content := message["content"].([]any)
	if len(content) != 3 || content[0].(map[string]any)["type"] != "server_tool_use" || content[1].(map[string]any)["type"] != "web_search_tool_result" || content[2].(map[string]any)["text"] != "Search was unavailable." {
		t.Fatalf("content = %#v", content)
	}
	use := content[0].(map[string]any)
	if use["input"].(map[string]any)["query"] != "rust tutorials" || content[1].(map[string]any)["tool_use_id"] != use["id"] {
		t.Fatalf("fallback linkage = %#v", content)
	}
	errorContent := content[1].(map[string]any)["content"].(map[string]any)
	if errorContent["type"] != "web_search_tool_result_error" || errorContent["error_code"] != "unavailable" {
		t.Fatalf("fallback error = %#v", errorContent)
	}
	usage := message["usage"].(map[string]any)["server_tool_use"].(map[string]any)
	if usage["web_search_requests"] != float64(1) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestOptionalBuildSearchWithoutCallRemainsPlainText(t *testing.T) {
	_, options, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,
		"messages":[{"role":"user","content":"answer with or without search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
		"id":"resp_optional","model":"grok-4.5","status":"completed",
		"output":[{"type":"message","content":[{"type":"output_text","text":"Plain answer."}]}],
		"usage":{"input_tokens":3,"output_tokens":2}
	}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, options)
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	content := message["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "Plain answer." {
		t.Fatalf("optional search fabricated server blocks: %#v", content)
	}
	if _, exists := message["usage"].(map[string]any)["server_tool_use"]; exists {
		t.Fatalf("optional search fabricated usage: %#v", message["usage"])
	}
}

func TestRequestedBuildSearchStreamWithoutCallEmitsUnavailableBeforeText(t *testing.T) {
	_, options, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"stream":true,
		"messages":[{"role":"user","content":"Perform a web search for the query: rust tutorials"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_no_call","model":"grok-4.5"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Search was unavailable."}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_no_call","model":"grok-4.5","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Search was unavailable."}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
		``,
	}, "\n")
	raw, err := io.ReadAll(ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(source)), OperationMessages, options))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if got, want := contentBlockStartTypes(t, text), []string{"server_tool_use", "web_search_tool_result", "text"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("content block order = %v, want %v\n%s", got, want, text)
	}
	if !strings.Contains(text, `"error_code":"unavailable"`) || !strings.Contains(text, `"web_search_requests":1`) || !strings.Contains(text, "rust tutorials") {
		t.Fatalf("missing unavailable fallback: %s", text)
	}
}

func TestConvertAnthropicWebSearchToolChoiceRequired(t *testing.T) {
	converted, options, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,
		"messages":[{"role":"user","content":"Perform a web search for the query: x"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(converted, &payload)
	tools := payload["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search" {
		t.Fatalf("tools = %#v", tools)
	}
	if payload["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %#v", payload["tool_choice"])
	}
	if !options.AnthropicWebSearchRequired {
		t.Fatalf("forced hosted search was not retained in response options: %#v", options)
	}
}

func TestClientWebSearchFunctionNotPromoted(t *testing.T) {
	converted, _, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,
		"messages":[{"role":"user","content":"search"}],
		"tools":[{"name":"WebSearch","description":"Search","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(converted, &payload)
	tools := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", tools)
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "WebSearch" {
		t.Fatalf("client WebSearch must remain function, got %#v", tool)
	}
}

func TestClientLowercaseWebSearchToolChoiceRemainsFunction(t *testing.T) {
	converted, _, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,
		"messages":[{"role":"user","content":"search"}],
		"tools":[{"name":"web_search","description":"Search","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "function", "name": "web_search"}
	if got := payload["tool_choice"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("client function tool_choice = %#v, want %#v", got, want)
	}
}

func TestResponseOptionsRetainOnlyExplicitWebSearchQuery(t *testing.T) {
	_, searchOptions, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: rust tutorials"}]}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	if !searchOptions.AnthropicWebSearch || searchOptions.AnthropicWebSearchRequired || searchOptions.AnthropicWebSearchQuery != "rust tutorials" {
		t.Fatalf("search options = %#v", searchOptions)
	}

	_, regularOptions, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,
		"messages":[{"role":"user","content":"private user prompt"}]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	if regularOptions.AnthropicWebSearch || regularOptions.AnthropicWebSearchQuery != "" {
		t.Fatalf("regular request retained prompt data: %#v", regularOptions)
	}
}

func TestWebSearchToolChoiceNoneDisablesResponseMapping(t *testing.T) {
	_, options, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,
		"messages":[{"role":"user","content":"Perform a web search for the query: private query"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"tool_choice":{"type":"none"}
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	if options.AnthropicWebSearch || options.AnthropicWebSearchQuery != "" {
		t.Fatalf("tool_choice none retained web search state: %#v", options)
	}
}

func TestMapBuildWebSearchFiltersEmptyDistinctCalls(t *testing.T) {
	body := []byte(`{
		"id":"resp_ws_empty","model":"grok-4.5","status":"completed",
		"output":[
			{"type":"web_search_call","id":"ws_real","status":"completed","action":{
				"type":"search","query":"rust tutorials",
				"sources":[{"type":"url","url":"https://doc.rust-lang.org"}]
			}},
			{"type":"web_search_call","id":"ws_empty_1","status":"completed","action":{"type":"search"}},
			{"type":"web_search_call","id":"ws_empty_2","status":"completed","action":{"type":"search","query":""}},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Here you go."}]}
		],
		"usage":{"input_tokens":3,"output_tokens":2}
	}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, ResponseOptions{AnthropicWebSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	content := msg["content"].([]any)
	serverUses := 0
	results := 0
	for _, raw := range content {
		block := raw.(map[string]any)
		switch block["type"] {
		case "server_tool_use":
			serverUses++
		case "web_search_tool_result":
			results++
		}
	}
	if serverUses != 1 || results != 1 {
		t.Fatalf("expected one real search pair, got uses=%d results=%d content=%#v", serverUses, results, content)
	}
	usage := msg["usage"].(map[string]any)["server_tool_use"].(map[string]any)
	if usage["web_search_requests"] != float64(1) {
		t.Fatalf("usage must count only real searches: %#v", usage)
	}
}

func TestMapBuildWebSearchDerivesDistinctMissingIDs(t *testing.T) {
	body := []byte(`{
		"id":"resp_ws_missing_ids","model":"grok-4.5","status":"completed",
		"output":[
			{"type":"web_search_call","status":"completed","action":{"type":"search","query":"rust","sources":[{"url":"https://www.rust-lang.org"}]}},
			{"type":"web_search_call","status":"completed","action":{"type":"search","query":"go","sources":[{"url":"https://go.dev"}]}}
		],
		"usage":{"input_tokens":3,"output_tokens":2}
	}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, ResponseOptions{AnthropicWebSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	content := msg["content"].([]any)
	var ids []string
	for _, raw := range content {
		block := raw.(map[string]any)
		if block["type"] == "server_tool_use" {
			ids = append(ids, block["id"].(string))
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("missing upstream IDs must derive distinct stable IDs, got %v content=%#v", ids, content)
	}
	usage := msg["usage"].(map[string]any)["server_tool_use"].(map[string]any)
	if usage["web_search_requests"] != float64(2) {
		t.Fatalf("usage = %#v, want two searches", usage)
	}
}

func TestAnthropicServerToolUseIDDoesNotCollideOnLongCommonSuffix(t *testing.T) {
	suffix := strings.Repeat("z", 48)
	first := anthropicServerToolUseID("first-prefix-"+suffix, nil)
	second := anthropicServerToolUseID("second-prefix-"+suffix, nil)
	if first == second {
		t.Fatalf("long upstream IDs collided: %q", first)
	}
}

func TestAnthropicServerToolUseIDNormalizesPrefixedUntrustedID(t *testing.T) {
	raw := "srvtoolu_" + strings.Repeat("a", 80) + " bad\n"
	got := anthropicServerToolUseID(raw, nil)
	if !strings.HasPrefix(got, "srvtoolu_") || len(got) > 64 {
		t.Fatalf("normalized id = %q (len=%d)", got, len(got))
	}
	if strings.IndexFunc(got, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-')
	}) >= 0 {
		t.Fatalf("normalized id retained unsafe characters: %q", got)
	}
	if again := anthropicServerToolUseID(raw, nil); again != got {
		t.Fatalf("normalization is unstable: first=%q second=%q", got, again)
	}
}

func TestMapBuildWebSearchFiltersUnsafeAndDuplicateSources(t *testing.T) {
	body := []byte(`{
		"id":"resp_ws_sources","model":"grok-4.5","status":"completed",
		"output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{
			"type":"search","query":"security",
			"sources":[
				{"url":"https://example.com/a","title":"Example"},
				{"url":"https://example.com/a","title":"Duplicate"},
				{"url":"javascript:alert(1)","title":"Script"},
				{"url":"file:///etc/passwd","title":"File"},
				{"url":"/relative/path","title":"Relative"},
				{"url":"https://user:secret@example.com/private","title":"Credential URL"}
			]
		}}],
		"usage":{"input_tokens":3,"output_tokens":2}
	}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, ResponseOptions{AnthropicWebSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	content := msg["content"].([]any)
	hits := content[1].(map[string]any)["content"].([]any)
	if len(hits) != 1 {
		t.Fatalf("unsafe or duplicate sources leaked into hits: %#v", hits)
	}
	hit := hits[0].(map[string]any)
	if hit["url"] != "https://example.com/a" || hit["title"] != "Example" {
		t.Fatalf("hit = %#v", hit)
	}
}

func TestParseBuildWebSearchBoundsResultsAndTitles(t *testing.T) {
	sources := make([]any, 0, 60)
	for index := 0; index < 60; index++ {
		sources = append(sources, map[string]any{
			"url":   "https://example.com/" + strconv.Itoa(index),
			"title": strings.Repeat("界", 600),
		})
	}
	call, ok := parseWebSearchCallItem(responseItem{
		ID: "ws_bounds", Type: "web_search_call", Status: "completed",
		Action: map[string]any{"query": "bounds", "sources": sources},
	})
	if !ok {
		t.Fatal("web search call was not parsed")
	}
	if len(call.Hits) != 50 {
		t.Fatalf("hits = %d, want 50", len(call.Hits))
	}
	if got := utf8.RuneCountInString(call.Hits[0].Title); got != 512 {
		t.Fatalf("title runes = %d, want 512", got)
	}
}

func TestParseResponseCapsWebSearchCandidatesBeforeDedupe(t *testing.T) {
	output := make([]any, 0, maxWebSearchCalls+20)
	for index := 0; index < maxWebSearchCalls+20; index++ {
		output = append(output, map[string]any{
			"type": "web_search_call", "id": "ws_" + strconv.Itoa(index), "status": "completed",
			"action": map[string]any{
				"type": "search", "query": "q" + strconv.Itoa(index),
				"sources": []any{map[string]any{"type": "url", "url": "https://example.com/" + strconv.Itoa(index)}},
			},
		})
	}
	body, err := json.Marshal(map[string]any{
		"id": "resp_cap", "model": "grok-4.5", "status": "completed", "created_at": 1,
		"output": output, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, ResponseOptions{AnthropicWebSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	content, _ := msg["content"].([]any)
	uses := 0
	for _, block := range content {
		m, _ := block.(map[string]any)
		if m["type"] == "server_tool_use" {
			uses++
		}
	}
	if uses != maxWebSearchCalls {
		t.Fatalf("server_tool_use count = %d, want %d", uses, maxWebSearchCalls)
	}
}

func TestDedupeWebSearchCallsBoundsCallCount(t *testing.T) {
	calls := make([]webSearchCall, 0, 40)
	for index := 0; index < 40; index++ {
		calls = append(calls, webSearchCall{
			ID:    "srvtoolu_" + strconv.Itoa(index),
			Query: "query " + strconv.Itoa(index),
			Hits:  []webSearchHit{{Title: "Example", URL: "https://example.com/" + strconv.Itoa(index)}},
		})
	}
	if got := len(dedupeWebSearchCalls(calls)); got != 32 {
		t.Fatalf("deduped calls = %d, want 32", got)
	}
}

func TestDedupeWebSearchCallsBoundsMergedHits(t *testing.T) {
	first := webSearchCall{ID: "srvtoolu_merge", Query: "query"}
	second := webSearchCall{ID: "srvtoolu_merge", Query: "query"}
	for index := 0; index < searchresult.MaxResults; index++ {
		first.Hits = append(first.Hits, webSearchHit{Title: "First", URL: "https://example.com/a/" + strconv.Itoa(index)})
		second.Hits = append(second.Hits, webSearchHit{Title: "Second", URL: "https://example.com/b/" + strconv.Itoa(index)})
	}
	merged := dedupeWebSearchCalls([]webSearchCall{first, second})
	if len(merged) != 1 || len(merged[0].Hits) != searchresult.MaxResults {
		t.Fatalf("merged calls = %#v", merged)
	}
}

func TestParseWebSearchCallBoundsUpstreamQuery(t *testing.T) {
	call, ok := parseWebSearchCallItem(responseItem{
		ID: "ws_long_query", Type: "web_search_call", Status: "completed",
		Action: map[string]any{"type": "search", "query": strings.Repeat("界", 5000)},
	})
	if !ok {
		t.Fatal("web search call was not parsed")
	}
	if got := utf8.RuneCountInString(call.Query); got != 4096 {
		t.Fatalf("query runes = %d, want 4096", got)
	}
}

func TestStreamCapsWebSearchCallsBeforeEmission(t *testing.T) {
	var output strings.Builder
	converter := newStreamConverter(&output, OperationMessages, ResponseOptions{AnthropicWebSearch: true})
	for index := 0; index < maxWebSearchCalls+10; index++ {
		call := webSearchCall{
			ID:    "srvtoolu_stream_" + strconv.Itoa(index),
			Query: "query " + strconv.Itoa(index),
			Hits:  []webSearchHit{{Title: "Example", URL: "https://example.com/" + strconv.Itoa(index)}},
		}
		if err := converter.noteWebSearch(call, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(converter.webSearch) != maxWebSearchCalls {
		t.Fatalf("tracked web search calls = %d, want %d", len(converter.webSearch), maxWebSearchCalls)
	}
	if got := strings.Count(output.String(), `"type":"server_tool_use"`); got != maxWebSearchCalls {
		t.Fatalf("emitted server tool uses = %d, want %d", got, maxWebSearchCalls)
	}
}

func TestStreamRejectsOversizedDeferredSearchText(t *testing.T) {
	converter := newStreamConverter(io.Discard, OperationMessages, ResponseOptions{AnthropicWebSearch: true})
	data, err := json.Marshal(map[string]any{
		"type":  "response.output_text.delta",
		"delta": strings.Repeat("x", (8<<20)+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = converter.handle("response.output_text.delta", data)
	if err == nil || !strings.Contains(err.Error(), "缓冲") {
		t.Fatalf("oversized deferred text error = %v", err)
	}
}

func TestStreamEmitsServerWebSearchBlocks(t *testing.T) {
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"ws_1","type":"web_search_call","status":"in_progress","action":{"type":"search","query":"rust tutorials"}}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Here you go."}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"rust tutorials","sources":[{"type":"url","url":"https://doc.rust-lang.org"}]}},{"type":"message","content":[{"type":"output_text","text":"Here you go."}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
		``,
	}, "\n")
	stream := ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(source)), OperationMessages, ResponseOptions{AnthropicWebSearch: true})
	raw, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"type":"server_tool_use"`) {
		t.Fatalf("missing server_tool_use in stream:\n%s", text)
	}
	if !strings.Contains(text, `"type":"web_search_tool_result"`) {
		t.Fatalf("missing web_search_tool_result in stream:\n%s", text)
	}
	if !strings.Contains(text, `https://doc.rust-lang.org`) {
		t.Fatalf("missing hit url in stream:\n%s", text)
	}
	if !strings.Contains(text, `"query":"rust tutorials"`) && !strings.Contains(text, `"query\": \"rust tutorials\"`) {
		// partial_json embeds query
		if !strings.Contains(text, "rust tutorials") {
			t.Fatalf("missing query in stream:\n%s", text)
		}
	}
	if !strings.Contains(text, `"stop_reason":"end_turn"`) {
		t.Fatalf("expected end_turn:\n%s", text)
	}
	assertSequentialContentBlocks(t, text)
	wantOrder := []string{"server_tool_use", "web_search_tool_result", "text"}
	if got := contentBlockStartTypes(t, text); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("content block order = %v, want %v\n%s", got, wantOrder, text)
	}
}

func TestStreamFiltersEmptyDistinctWebSearchCalls(t *testing.T) {
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"ws_real","type":"web_search_call","status":"in_progress","action":{"type":"search","query":"rust tutorials"}}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"ws_empty_1","type":"web_search_call","status":"in_progress","action":{"type":"search"}}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"ws_empty_2","type":"web_search_call","status":"in_progress","action":{"type":"search","query":""}}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Here you go."}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","output":[{"type":"web_search_call","id":"ws_real","status":"completed","action":{"type":"search","query":"rust tutorials","sources":[{"type":"url","url":"https://doc.rust-lang.org"}]}},{"type":"web_search_call","id":"ws_empty_1","status":"completed","action":{"type":"search"}},{"type":"web_search_call","id":"ws_empty_2","status":"completed","action":{"type":"search","query":""}},{"type":"message","content":[{"type":"output_text","text":"Here you go."}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
		``,
	}, "\n")
	stream := ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(source)), OperationMessages, ResponseOptions{AnthropicWebSearch: true})
	raw, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if got := strings.Count(text, `"type":"server_tool_use"`); got != 1 {
		t.Fatalf("expected one real server_tool_use, got %d\n%s", got, text)
	}
	if got := strings.Count(text, `"type":"web_search_tool_result"`); got != 1 {
		t.Fatalf("expected one real web_search_tool_result, got %d\n%s", got, text)
	}
	if !strings.Contains(text, `"web_search_requests":1`) {
		t.Fatalf("usage must count only real searches\n%s", text)
	}
	assertSequentialContentBlocks(t, text)
}

func TestStreamDefersTextWhenWebSearchArrivesDuringThinking(t *testing.T) {
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"reasoning_1","type":"reasoning"}}`,
		``,
		`event: response.reasoning_text.delta`,
		`data: {"type":"response.reasoning_text.delta","delta":"Need current sources."}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"ws_1","type":"web_search_call","status":"in_progress","action":{"type":"search","query":"rust tutorials"}}}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"reasoning_1","type":"reasoning","encrypted_content":"sig"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Here you go."}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"rust tutorials","sources":[{"type":"url","url":"https://doc.rust-lang.org"}]}},{"type":"message","content":[{"type":"output_text","text":"Here you go."}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
		``,
	}, "\n")
	stream := ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(source)), OperationMessages, ResponseOptions{AnthropicThinking: true, AnthropicWebSearch: true})
	raw, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	assertSequentialContentBlocks(t, text)
	wantOrder := []string{"thinking", "server_tool_use", "web_search_tool_result", "text"}
	if got := contentBlockStartTypes(t, text); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("content block order = %v, want %v\n%s", got, wantOrder, text)
	}
}

func TestStreamUsesRequestContextWhenTextArrivesBeforeWebSearchItem(t *testing.T) {
	_, options, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"stream":true,
		"messages":[{"role":"user","content":"Perform a web search for the query: rust tutorials"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Here you go."}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"rust tutorials","sources":[{"type":"url","url":"https://doc.rust-lang.org"}]}}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"rust tutorials","sources":[{"type":"url","url":"https://doc.rust-lang.org"}]}},{"type":"message","content":[{"type":"output_text","text":"Here you go."}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
		``,
	}, "\n")
	stream := ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(source)), OperationMessages, options)
	raw, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	assertSequentialContentBlocks(t, text)
	wantOrder := []string{"server_tool_use", "web_search_tool_result", "text"}
	if got := contentBlockStartTypes(t, text); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("content block order = %v, want %v\n%s", got, wantOrder, text)
	}
}

func TestStreamFinalEnvelopeDoesNotOrphanEarlierWebSearchUse(t *testing.T) {
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"ws_early","type":"web_search_call","status":"in_progress","action":{"type":"search","query":"early query"}}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Answer."}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","output":[{"type":"web_search_call","id":"ws_final","status":"completed","action":{"type":"search","query":"final query","sources":[{"url":"https://example.com/final"}]}},{"type":"message","content":[{"type":"output_text","text":"Answer."}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
		``,
	}, "\n")
	raw, err := io.ReadAll(ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(source)), OperationMessages, ResponseOptions{AnthropicWebSearch: true}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	uses := strings.Count(text, `"type":"server_tool_use"`)
	results := strings.Count(text, `"type":"web_search_tool_result"`)
	if uses != 2 || results != uses {
		t.Fatalf("uses=%d results=%d; earlier use was orphaned\n%s", uses, results, text)
	}
	if !strings.Contains(text, "early query") || !strings.Contains(text, "final query") || !strings.Contains(text, `"web_search_requests":2`) {
		t.Fatalf("merged stream lost a search call\n%s", text)
	}
}

func contentBlockStartTypes(t *testing.T, stream string) []string {
	t.Helper()
	var types []string
	scanner := bufio.NewScanner(strings.NewReader(stream))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		if event.Type == "content_block_start" {
			types = append(types, event.ContentBlock.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return types
}

func assertSequentialContentBlocks(t *testing.T, stream string) {
	t.Helper()
	openIndex := -1
	scanner := bufio.NewScanner(strings.NewReader(stream))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		switch event.Type {
		case "content_block_start":
			if openIndex >= 0 {
				t.Fatalf("content block %d started before block %d stopped\n%s", event.Index, openIndex, stream)
			}
			openIndex = event.Index
		case "content_block_delta":
			if openIndex != event.Index {
				t.Fatalf("delta for block %d while block %d is open\n%s", event.Index, openIndex, stream)
			}
		case "content_block_stop":
			if openIndex != event.Index {
				t.Fatalf("stop for block %d while block %d is open\n%s", event.Index, openIndex, stream)
			}
			openIndex = -1
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if openIndex >= 0 {
		t.Fatalf("content block %d never stopped\n%s", openIndex, stream)
	}
}
