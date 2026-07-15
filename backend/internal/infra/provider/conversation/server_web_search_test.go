package conversation

import (
	"encoding/json"
	"io"
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
}
