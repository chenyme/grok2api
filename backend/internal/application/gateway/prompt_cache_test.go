package gateway

import "testing"

func TestResolvePromptCacheKeyStableAcrossAppendOnlyTurns(t *testing.T) {
	round1 := []byte(`{
		"model":"claude-sonnet-4-5","max_tokens":256,
		"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"first question","cache_control":{"type":"ephemeral"}}]}],
		"tools":[{"name":"Read","description":"read","input_schema":{"type":"object"}}]
	}`)
	round2 := []byte(`{
		"model":"claude-sonnet-4-5","max_tokens":256,
		"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"first question","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":[{"type":"text","text":"first answer"}]},
			{"role":"user","content":[{"type":"text","text":"second question","cache_control":{"type":"ephemeral"}}]}
		],
		"tools":[{"name":"Read","description":"read","input_schema":{"type":"object"}}]
	}`)

	first := resolvePromptCacheKey(42, "claude-sonnet-4-5", "grok-4.5", "", "", round1)
	second := resolvePromptCacheKey(42, "claude-sonnet-4-5", "grok-4.5", "", "", round2)
	if first == "" || second == "" {
		t.Fatalf("expected non-empty keys: first=%q second=%q", first, second)
	}
	if first != second {
		t.Fatalf("append-only turns should keep identity stable:\nfirst=%s\nsecond=%s", first, second)
	}
	if len(first) != 36 {
		t.Fatalf("identity should be UUID-shaped, got %q len=%d", first, len(first))
	}
}

func TestResolvePromptCacheKeyIsolatesClientAndModel(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","system":"stable","messages":[{"role":"user","content":"hi"}],"tools":[]}`)
	base := resolvePromptCacheKey(1, "claude-sonnet-4-5", "grok-4.5", "", "", body)
	otherClient := resolvePromptCacheKey(2, "claude-sonnet-4-5", "grok-4.5", "", "", body)
	otherModel := resolvePromptCacheKey(1, "claude-sonnet-4-5", "grok-4.3", "", "", body)
	if base == "" || base == otherClient || base == otherModel {
		t.Fatalf("base=%q otherClient=%q otherModel=%q", base, otherClient, otherModel)
	}
}

func TestResolvePromptCacheKeyPrefersSessionHeader(t *testing.T) {
	bodyA := []byte(`{"model":"m","messages":[{"role":"user","content":"one"}]}`)
	bodyB := []byte(`{"model":"m","messages":[{"role":"user","content":"totally different"}]}`)
	first := resolvePromptCacheKey(9, "m", "grok-4.5", "body-key", "session-abc", bodyA)
	second := resolvePromptCacheKey(9, "m", "grok-4.5", "other-body-key", "session-abc", bodyB)
	if first == "" || first != second {
		t.Fatalf("session header should dominate: first=%q second=%q", first, second)
	}
}

func TestEnsurePromptCacheKeyInBodyOverwritesClientValue(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","prompt_cache_key":"raw-client","input":"hi"}`)
	out, err := ensurePromptCacheKeyInBody(body, "isolated-id")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) == string(body) {
		t.Fatal("expected body rewrite")
	}
	if !containsJSONStringField(out, "prompt_cache_key", "isolated-id") {
		t.Fatalf("body = %s", out)
	}
}

func TestResolvePromptCacheKeyIgnoresToolDescriptionNoise(t *testing.T) {
	// Claude Code 有时只改 tool description / schema 细节；会话身份应保持稳定。
	a := []byte(`{
		"model":"claude-sonnet-4-5",
		"system":"stable system",
		"messages":[{"role":"user","content":"hello session"}],
		"tools":[
			{"name":"Read","description":"read file v1","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}},
			{"name":"Bash","description":"bash v1","input_schema":{"type":"object"}}
		]
	}`)
	b := []byte(`{
		"model":"claude-sonnet-4-5",
		"system":"stable system",
		"messages":[{"role":"user","content":"hello session"}],
		"tools":[
			{"name":"Bash","description":"bash v2 with different schema","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}},
			{"name":"Read","description":"read file v2","input_schema":{"type":"object","properties":{"file":{"type":"string"}}}}
		]
	}`)
	ka := resolvePromptCacheKey(7, "claude-sonnet-4-5", "grok-4.5", "", "", a)
	kb := resolvePromptCacheKey(7, "claude-sonnet-4-5", "grok-4.5", "", "", b)
	if ka == "" || ka != kb {
		t.Fatalf("tool description noise should not change identity: %q vs %q", ka, kb)
	}
}

func TestResolvePromptCacheKeyChangesWhenToolNameSetChanges(t *testing.T) {
	a := []byte(`{"model":"m","system":"s","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`)
	b := []byte(`{"model":"m","system":"s","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read","input_schema":{"type":"object"}},{"name":"mcp__x__y","input_schema":{"type":"object"}}]}`)
	ka := resolvePromptCacheKey(7, "m", "grok-4.5", "", "", a)
	kb := resolvePromptCacheKey(7, "m", "grok-4.5", "", "", b)
	if ka == "" || kb == "" || ka == kb {
		t.Fatalf("tool name set change should change identity: %q vs %q", ka, kb)
	}
}

func TestResolvePromptCacheKeyUsesMetadataUserID(t *testing.T) {
	a := []byte(`{"model":"m","metadata":{"user_id":"sess-1"},"messages":[{"role":"user","content":"aaa"}]}`)
	b := []byte(`{"model":"m","metadata":{"user_id":"sess-1"},"messages":[{"role":"user","content":"totally different first user"}]}`)
	ka := resolvePromptCacheKey(3, "m", "grok-4.5", "", "", a)
	kb := resolvePromptCacheKey(3, "m", "grok-4.5", "", "", b)
	if ka == "" || ka != kb {
		t.Fatalf("metadata.user_id should dominate first_user: %q vs %q", ka, kb)
	}
}

func containsJSONStringField(body []byte, key, want string) bool {
	// lightweight check without importing encoding/json helpers again in assertion path
	needle := `"` + key + `":"` + want + `"`
	return stringContains(string(body), needle) || stringContains(string(body), `"`+key+`": "`+want+`"`)
}

func stringContains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
