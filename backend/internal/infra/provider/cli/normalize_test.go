package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func TestNormalizeResponsesRequest(t *testing.T) {
	body := []byte(`{"model":"public-model","input":[{"type":"reasoning","id":"old","encrypted_content":"cipher","content":[{"text":"thought"}]},{"role":"user","content":"hello"}],"prompt_cache_key":"official-key","response_format":{"type":"json_object"}}`)
	normalized, _, err := normalizeResponsesRequest(body, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(normalized, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "grok-4.5" || payload["prompt_cache_key"] != "official-key" {
		t.Fatalf("模型或缓存键未正确改写: %#v", payload)
	}
	input := payload["input"].([]any)
	if len(input) != 2 || input[0].(map[string]any)["type"] != "reasoning" || input[0].(map[string]any)["encrypted_content"] != "cipher" {
		t.Fatalf("reasoning 回放项未保留: %#v", input)
	}
	reasoningContent := input[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if reasoningContent["type"] != "reasoning_text" || reasoningContent["text"] != "thought" {
		t.Fatalf("reasoning content discriminator 未修补: %#v", reasoningContent)
	}
	text := payload["text"].(map[string]any)
	if text["format"] == nil || payload["response_format"] != nil {
		t.Fatalf("response_format 未映射: %#v", payload)
	}
}

func TestNormalizeBuildReasoningEffort(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		effort string
		want   string
	}{
		{name: "4.5 max", model: "grok-4.5", effort: "max", want: "high"},
		{name: "4.5 xhigh", model: "grok-4.5", effort: "xhigh", want: "high"},
		{name: "4.5 uppercase max", model: "grok-4.5", effort: "MAX", want: "high"},
		{name: "multi-agent xhigh", model: "grok-4.20-multi-agent-0309", effort: "xhigh", want: "xhigh"},
		{name: "multi-agent uppercase xhigh", model: "grok-4.20-multi-agent-0309", effort: "XHIGH", want: "xhigh"},
		{name: "multi-agent max maps to highest supported effort", model: "grok-4.20-multi-agent-0309", effort: "max", want: "xhigh"},
		{name: "4.6 xhigh", model: "grok-4.6", effort: "xhigh", want: "xhigh"},
		{name: "4.6 uppercase xhigh", model: "grok-4.6", effort: "XHIGH", want: "xhigh"},
		{name: "4.6 max maps to xhigh", model: "grok-4.6", effort: "max", want: "xhigh"},
		{name: "prefixed 4.6 uppercase max maps to xhigh", model: "Build/grok-4.6", effort: "MAX", want: "xhigh"},
		{name: "unknown xhigh remains guarded", model: "future-model", effort: "xhigh", want: "high"},
		{name: "unknown max remains guarded", model: "future-model", effort: "max", want: "high"},
		{name: "high", model: "grok-4.5", effort: "high", want: "high"},
		{name: "medium", model: "grok-4.5", effort: "medium", want: "medium"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"reasoning":{"effort":"` + test.effort + `"},"input":"hello"}`)
			normalized, err := normalizeBuildRequest(body, test.model, conversation.OperationResponses)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(normalized, &payload); err != nil {
				t.Fatal(err)
			}
			reasoning, ok := payload["reasoning"].(map[string]any)
			if !ok || reasoning["effort"] != test.want {
				t.Fatalf("reasoning = %#v, want %q", payload["reasoning"], test.want)
			}
		})
	}
}

func TestNormalizeBuildChatRequestsVisibleReasoningSummary(t *testing.T) {
	tests := []struct {
		name        string
		operation   string
		body        string
		wantSummary string
		wantEffort  string
		wantObject  bool
	}{
		{name: "Chat model default", operation: conversation.OperationChat, body: `{"input":"hello"}`, wantSummary: "concise", wantObject: true},
		{name: "Chat explicit effort", operation: conversation.OperationChat, body: `{"input":"hello","reasoning":{"effort":"high"}}`, wantSummary: "concise", wantEffort: "high", wantObject: true},
		{name: "Chat explicit summary", operation: conversation.OperationChat, body: `{"input":"hello","reasoning":{"effort":"high","summary":"detailed"}}`, wantSummary: "detailed", wantEffort: "high", wantObject: true},
		{name: "native Responses remains caller controlled", operation: conversation.OperationResponses, body: `{"input":"hello"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, err := normalizeBuildRequest([]byte(test.body), "grok-4.6", test.operation)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(normalized, &payload); err != nil {
				t.Fatal(err)
			}
			reasoning, ok := payload["reasoning"].(map[string]any)
			if ok != test.wantObject {
				t.Fatalf("reasoning = %#v, want object %v", payload["reasoning"], test.wantObject)
			}
			if !ok {
				return
			}
			if reasoning["summary"] != test.wantSummary {
				t.Fatalf("summary = %#v, want %q", reasoning["summary"], test.wantSummary)
			}
			if effort, _ := reasoning["effort"].(string); effort != test.wantEffort {
				t.Fatalf("effort = %q, want %q", effort, test.wantEffort)
			}
		})
	}
}

func TestNormalizeBuildMaxAcrossCompatibilityProtocols(t *testing.T) {
	tests := []struct {
		name      string
		body      []byte
		operation string
	}{
		{name: "responses", operation: conversation.OperationResponses, body: []byte(`{"reasoning":{"effort":"max"},"input":"hello"}`)},
		{name: "chat completions", operation: conversation.OperationChat, body: func() []byte {
			converted, _, err := conversation.ConvertRequestWithOptions(
				[]byte(`{"model":"public","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"max"}`),
				"grok-4.6",
				conversation.OperationChat,
			)
			if err != nil {
				t.Fatal(err)
			}
			return converted
		}()},
		{name: "anthropic messages", operation: conversation.OperationMessages, body: func() []byte {
			converted, _, err := conversation.ConvertRequestWithOptions(
				[]byte(`{"model":"public","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`),
				"grok-4.6",
				conversation.OperationMessages,
			)
			if err != nil {
				t.Fatal(err)
			}
			return converted
		}()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, err := normalizeBuildRequest(test.body, "grok-4.6", test.operation)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(normalized, &payload); err != nil {
				t.Fatal(err)
			}
			reasoning, ok := payload["reasoning"].(map[string]any)
			if !ok || reasoning["effort"] != "xhigh" {
				t.Fatalf("reasoning = %#v, want max normalized to xhigh", payload["reasoning"])
			}
		})
	}
}

func TestNormalizeBuildComposerStripsReasoningEffort(t *testing.T) {
	tests := []struct {
		name      string
		body      []byte
		operation string
	}{
		{name: "responses", operation: conversation.OperationResponses, body: []byte(`{"reasoning":{"effort":"high","summary":"auto"},"input":"hello"}`)},
		{name: "chat conversion", operation: conversation.OperationChat, body: func() []byte {
			converted, _, err := conversation.ConvertRequestWithOptions(
				[]byte(`{"model":"public","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"high"}`),
				modeldomain.GrokComposer25Fast,
				conversation.OperationChat,
			)
			if err != nil {
				t.Fatal(err)
			}
			return converted
		}()},
		{name: "messages conversion", operation: conversation.OperationMessages, body: func() []byte {
			converted, _, err := conversation.ConvertRequestWithOptions(
				[]byte(`{"model":"public","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}}`),
				modeldomain.GrokComposer25Fast,
				conversation.OperationMessages,
			)
			if err != nil {
				t.Fatal(err)
			}
			return converted
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, err := normalizeBuildRequest(test.body, modeldomain.GrokComposer25Fast, test.operation)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if json.Unmarshal(normalized, &payload) != nil {
				t.Fatalf("invalid normalized payload: %s", normalized)
			}
			if reasoning, ok := payload["reasoning"].(map[string]any); ok {
				if _, exists := reasoning["effort"]; exists {
					t.Fatalf("Composer reasoning effort leaked upstream: %#v", payload)
				}
			}
		})
	}

	stripped, err := normalizeBuildRequest([]byte(`{"reasoning":{"effort":"medium"},"input":"hello"}`), modeldomain.GrokComposer25Fast, conversation.OperationResponses)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if json.Unmarshal(stripped, &payload) != nil || payload["reasoning"] != nil {
		t.Fatalf("empty Composer reasoning object was not removed: %s", stripped)
	}
}

func TestNormalizeBuildRequestAppliesSafeDefaultsAndStripsCodexEnvelope(t *testing.T) {
	body := []byte(`{
		"model":"public",
		"input":"hello",
		"metadata":{"tenant":"keep"},
		"client_metadata":{"cwd":"/private/workspace","git_remote":"ssh://private/repo"},
		"include":["web_search_call.action.sources"]
	}`)
	normalized, err := normalizeBuildRequest(body, "grok-4.5", conversation.OperationResponses)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(normalized, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["client_metadata"] != nil {
		t.Fatalf("client_metadata crossed Build boundary: %#v", payload["client_metadata"])
	}
	if payload["metadata"].(map[string]any)["tenant"] != "keep" || payload["store"] != false {
		t.Fatalf("standard metadata or store default changed incorrectly: %#v", payload)
	}
	includes := payload["include"].([]any)
	if len(includes) != 2 || includes[0] != "web_search_call.action.sources" || includes[1] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", includes)
	}
}

func TestNormalizeBuildRequestPreservesExplicitStoreAndEncryptedInclude(t *testing.T) {
	body := []byte(`{"input":"hello","store":true,"include":["reasoning.encrypted_content"],"stream_tool_calls":true}`)
	normalized, err := normalizeBuildRequest(body, "grok-4.5", conversation.OperationResponses)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(normalized, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["store"] != true || len(payload["include"].([]any)) != 1 || payload["stream_tool_calls"] != true {
		t.Fatalf("explicit Build defaults were overwritten: %#v", payload)
	}
}

func TestNormalizeBuildRequestDoesNotInventStreamToolCalls(t *testing.T) {
	normalized, err := normalizeBuildRequest([]byte(`{"input":"hello"}`), "grok-4.5", conversation.OperationResponses)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(normalized, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["stream_tool_calls"]; exists {
		t.Fatalf("stream_tool_calls must remain client/model controlled: %#v", payload)
	}
}

func TestNormalizeResponsesRequestPreservesExplicitPromptCacheKey(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{"model":"public","input":"hello","prompt_cache_key":"official-key"}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(normalized, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["prompt_cache_key"] != "official-key" {
		t.Fatalf("prompt_cache_key = %#v", payload["prompt_cache_key"])
	}
}

func TestNormalizeResponsesRequestDoesNotInventPromptCacheKey(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{"model":"public","input":"hello"}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(normalized, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["prompt_cache_key"]; exists {
		t.Fatalf("unexpected prompt_cache_key: %#v", payload)
	}
}

func TestNormalizeResponsesRequestAddsEmptySummaryToEncryptedReasoning(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{"model":"public","input":[{"type":"reasoning","encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []map[string]any `json:"input"`
	}
	if json.Unmarshal(normalized, &payload) != nil || len(payload.Input) != 2 {
		t.Fatalf("normalized = %s", normalized)
	}
	summary, ok := payload.Input[0]["summary"].([]any)
	if !ok || len(summary) != 0 || payload.Input[0]["encrypted_content"] != "opaque" {
		t.Fatalf("reasoning = %#v", payload.Input[0])
	}
}

func TestNormalizeResponsesRequestFlattensJSONSchema(t *testing.T) {
	body := []byte(`{"model":"public","input":"hello","response_format":{"type":"json_schema","json_schema":{"type":"object","name":"answer","strict":true,"schema":{"type":"object"}}}}`)
	normalized, _, err := normalizeResponsesRequest(body, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Text struct {
			Format map[string]any `json:"format"`
		} `json:"text"`
	}
	if err := json.Unmarshal(normalized, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Text.Format["type"] != "json_schema" || payload.Text.Format["name"] != "answer" || payload.Text.Format["json_schema"] != nil {
		t.Fatalf("format = %#v", payload.Text.Format)
	}
}

func TestParseImportedCredentialsBatch(t *testing.T) {
	data := []byte(`{"accounts":[{"provider":"grok_build","name":"primary","client_id":"client-1","access_token":"access-1","refresh_token":"refresh-1","email":"user@example.com","user_id":"user-1","expires_at":"2026-07-11T00:00:00Z"},{"refresh_token":"refresh-2"}]}`)
	values, err := parseImportedCredentials(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Name != "primary" || values[0].UserID != "user-1" || values[0].OIDCClientID != "client-1" || values[1].RefreshToken != "refresh-2" {
		t.Fatalf("导入结果不正确: %#v", values)
	}
	if values[0].SourceKey == values[1].SourceKey {
		t.Fatal("不同账号生成了相同来源标识")
	}
}

func TestParseImportedCredentialsJSONSequence(t *testing.T) {
	data := []byte("\xef\xbb\xbf{\n  \"access_token\": \"access-1\",\n  \"sub\": \"user-1\"\n}\r\n\r\n" +
		"{\"refresh_token\":\"refresh-2\",\"email\":\"two@example.com\"}\r\n")
	values, err := parseImportedCredentials(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].AccessToken != "access-1" || values[0].UserID != "user-1" || values[1].RefreshToken != "refresh-2" {
		t.Fatalf("JSON sequence import = %#v", values)
	}
}

// 批量注册工具导出的顶层裸数组形态。
func TestParseImportedCredentialsBareArray(t *testing.T) {
	data := []byte(`[{"type":"xai","access_token":"access-1","refresh_token":"refresh-1","email":"user@example.com","user_id":"user-1","expires_at":"2026-08-01T00:00:00Z"}` +
		`,{"type":"xai","refresh_token":"refresh-2"}]`)
	values, err := parseImportedCredentials(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].AccessToken != "access-1" || values[0].Email != "user@example.com" || values[0].UserID != "user-1" || values[0].ExpiresAt.IsZero() || values[1].RefreshToken != "refresh-2" {
		t.Fatalf("bare array import = %#v", values)
	}
	if values[0].SourceKey == values[1].SourceKey {
		t.Fatal("不同账号生成了相同来源标识")
	}
}

// null 元素不在解码层拦截，归一化阶段须带账号序号报错。
func TestParseImportedCredentialsBareArrayRejectsNullElement(t *testing.T) {
	_, err := parseImportedCredentials([]byte(`[{"refresh_token":"refresh-1"},null]`))
	if err == nil || !strings.Contains(err.Error(), "第 2 个账号") {
		t.Fatalf("error = %v, want indexed normalize error", err)
	}
}

func TestParseImportedCredentialsLooseAccountsDocument(t *testing.T) {
	data := []byte("{\n  \"accounts\": [\n" +
		"{\"access_token\":\"access-1\",\"sub\":\"user-1\"}\n" +
		"{\"refresh_token\":\"refresh-2\"}\n")
	values, err := parseImportedCredentials(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].UserID != "user-1" || values[1].RefreshToken != "refresh-2" {
		t.Fatalf("loose accounts import = %#v", values)
	}
}

func TestParseImportedCredentialsLooseAccountsDocumentReportsLine(t *testing.T) {
	data := []byte("{\n  \"accounts\": [\n{\"access_token\":\"access-1\"}\nnot-json\n")
	_, err := parseImportedCredentials(data)
	if err == nil || !strings.Contains(err.Error(), "第 4 行") {
		t.Fatalf("error = %v, want line number", err)
	}
}

func TestMarshalCredentialsUsesImportDocument(t *testing.T) {
	expiresAt := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	data, err := marshalCredentials([]provider.CredentialSeed{{
		Name: "primary", Email: "user@example.com", UserID: "user-1", TeamID: "team-1",
		OIDCClientID: "client-1", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: expiresAt,
	}})
	if err != nil {
		t.Fatal(err)
	}
	values, err := parseImportedCredentials(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].AccessToken != "access" || values[0].RefreshToken != "refresh" || !values[0].ExpiresAt.Equal(expiresAt) {
		t.Fatalf("round-trip values = %#v", values)
	}
}

func TestParseImportedCredentialsRejectsAccountLimit(t *testing.T) {
	data, err := json.Marshal(credentialImportDocument{Accounts: make([]importedCredentialEntry, maxCredentialImportAccounts+1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseImportedCredentials(data); !errors.Is(err, provider.ErrCredentialLimit) {
		t.Fatalf("error = %v, want credential limit", err)
	}
}

func TestParseImportedCredentialsOfficialOAuthResponse(t *testing.T) {
	expiresAt := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	claims, _ := json.Marshal(map[string]any{"sub": "user-1", "email": "user@example.com", "team_id": "team-1", "exp": expiresAt.Unix()})
	idToken := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	data, _ := json.Marshal(map[string]any{"access_token": "access", "refresh_token": "refresh", "expires_in": 3600, "id_token": idToken, "token_type": "Bearer"})

	values, err := parseImportedCredentials(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].UserID != "user-1" || values[0].Email != "user@example.com" || values[0].TeamID != "team-1" || !values[0].ExpiresAt.Equal(expiresAt) {
		t.Fatalf("OAuth 导入结果不正确: %#v", values)
	}
}

func TestParseImportedCredentialsRejectsUnsupportedMap(t *testing.T) {
	_, err := parseImportedCredentials([]byte(`{"https://auth.x.ai::client":{"key":"access","refresh_token":"refresh"}}`))
	if err == nil {
		t.Fatal("旧 Map 格式不应继续被接受")
	}
}
