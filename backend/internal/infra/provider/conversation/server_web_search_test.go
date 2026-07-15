package conversation

import (
	"bufio"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
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
					{"type":"url_citation","url":"https://example.com/a","title":"Alpha Title","start_index":0,"end_index":5}
				]}
			]}
		],
		"usage":{"input_tokens":10,"output_tokens":5}
	}`)
	data, err := ConvertResponseJSON(body, OperationMessages)
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

func TestConvertAnthropicWebSearchToolChoiceRequired(t *testing.T) {
	converted, _, err := ConvertRequestWithOptions([]byte(`{
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
	data, err := ConvertResponseJSON(body, OperationMessages)
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
	data, err := ConvertResponseJSON(body, OperationMessages)
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
	stream := ConvertResponseStream(io.NopCloser(strings.NewReader(source)), OperationMessages)
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
	stream := ConvertResponseStream(io.NopCloser(strings.NewReader(source)), OperationMessages)
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
	stream := ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(source)), OperationMessages, ResponseOptions{AnthropicThinking: true})
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
