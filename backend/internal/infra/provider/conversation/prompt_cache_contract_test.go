package conversation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestConvertMessagesDropsCacheControlAndDoesNotInventPromptCacheKey 验证 Claude Code
// 常见的 cache_control 断点不会进入 Grok Responses 请求，billing header 被过滤，
// 且转换层不凭空发明 prompt_cache_key(该 key 由 gateway 层 prompt_cache.go 推导)。
func TestConvertMessagesDropsCacheControlAndDoesNotInventPromptCacheKey(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5","max_tokens":1024,
		"system":[
			{"type":"text","text":"x-anthropic-billing-header: drop-me"},
			{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}
		],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"first question","cache_control":{"type":"ephemeral"}}
			]}
		],
		"tools":[
			{"name":"Bash","description":"run shell","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}
		]
	}`)
	converted, err := ConvertRequest(body, "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["prompt_cache_key"]; exists {
		t.Fatalf("conversation convert must not invent prompt_cache_key, got %#v", payload["prompt_cache_key"])
	}
	if payload["instructions"] != "You are Claude Code." {
		t.Fatalf("instructions should keep system text and drop billing header, got %#v", payload["instructions"])
	}
	raw, _ := json.Marshal(payload)
	if strings.Contains(string(raw), "cache_control") {
		t.Fatalf("cache_control must not leak into Responses payload: %s", raw)
	}
	if strings.Contains(string(raw), "x-anthropic-billing-header") {
		t.Fatalf("billing header must be dropped: %s", raw)
	}
}

// TestConvertMessagesToolListChangeBreaksStablePrefix 验证 MCP/工具列表变化会改变
// 发给 Grok 的 tools 前缀——这是比 cache_control 更可能打穿 Grok 缓存的因素。
func TestConvertMessagesToolListChangeBreaksStablePrefix(t *testing.T) {
	base := `{
		"model":"claude-sonnet-4-5","max_tokens":128,
		"system":"stable",
		"messages":[{"role":"user","content":"hi"}],
		"tools":%s
	}`
	toolsA := `[{"name":"Read","description":"read","input_schema":{"type":"object"}},{"name":"Bash","description":"bash","input_schema":{"type":"object"}}]`
	toolsB := `[{"name":"Read","description":"read","input_schema":{"type":"object"}},{"name":"Bash","description":"bash","input_schema":{"type":"object"}},{"name":"mcp__x__tool","description":"mcp","input_schema":{"type":"object"}}]`
	c1, err := ConvertRequest([]byte(fmt.Sprintf(base, toolsA)), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := ConvertRequest([]byte(fmt.Sprintf(base, toolsB)), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var p1, p2 map[string]any
	_ = json.Unmarshal(c1, &p1)
	_ = json.Unmarshal(c2, &p2)
	t1, _ := json.Marshal(p1["tools"])
	t2, _ := json.Marshal(p2["tools"])
	if string(t1) == string(t2) {
		t.Fatalf("expected tools prefix to change when MCP tool is added")
	}
	if !strings.Contains(string(t2), "mcp__x__tool") {
		t.Fatalf("expected mcp tool in converted tools: %s", t2)
	}
}

// TestConvertAnthropicToolsDedup 验证同名工具去重(F7)，规避上游 Grok "Duplicate function definition"。
func TestConvertAnthropicToolsDedup(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5","max_tokens":128,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"name":"Read","description":"a","input_schema":{"type":"object"}},
			{"name":"Read","description":"b","input_schema":{"type":"object"}},
			{"name":"Bash","description":"c","input_schema":{"type":"object"}}
		]
	}`)
	converted, err := ConvertRequest(body, "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(converted, &payload)
	tools, _ := payload["tools"].([]any)
	names := map[string]int{}
	for _, tv := range tools {
		if m, ok := tv.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				names[n]++
			}
		}
	}
	if names["Read"] != 1 {
		t.Fatalf("duplicate tool Read should be deduped to 1, got %d", names["Read"])
	}
	if names["Bash"] != 1 {
		t.Fatalf("Bash should be present once, got %d", names["Bash"])
	}
}
