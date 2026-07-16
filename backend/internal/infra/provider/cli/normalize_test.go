package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
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
	if len(input) != 2 || input[0].(map[string]any)["encrypted_content"] != "cipher" {
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
