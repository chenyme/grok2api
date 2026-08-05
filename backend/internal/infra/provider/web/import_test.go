package web

import (
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestParseImportedCredentialsAcceptsOneSSOTokenPerLine(t *testing.T) {
	adapter := &Adapter{}
	values, err := adapter.ParseImportedCredentials([]byte("token-one\nsso=token-two; other=drop\n\ntoken-one\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 {
		t.Fatalf("credentials = %#v", values)
	}
	if values[0].AccessToken != "token-one" || values[1].AccessToken != "token-two" {
		t.Fatalf("tokens = %q, %q", values[0].AccessToken, values[1].AccessToken)
	}
	for _, value := range values {
		if value.Provider != account.ProviderWeb || value.AuthType != account.AuthTypeSSO || value.WebTier != account.WebTierAuto {
			t.Fatalf("credential = %#v", value)
		}
	}
}

func TestParseImportedCredentialsRejectsOversizedPlainToken(t *testing.T) {
	adapter := &Adapter{}
	_, err := adapter.ParseImportedCredentials([]byte(strings.Repeat("x", maxSSOTokenBytes+1)))
	if err == nil {
		t.Fatal("expected oversized token error")
	}
}

func TestWebCredentialJSONUsesCurrentDocumentShape(t *testing.T) {
	adapter := &Adapter{}
	values, err := adapter.ParseImportedCredentials([]byte(`{"provider":"grok_web","accounts":[{"name":"primary","sso_token":"token-one","tier":"super","cloudflare_cookies":"cf_clearance=abc; sso=drop"}]}`))
	if err != nil || len(values) != 1 || values[0].WebTier != account.WebTierSuper {
		t.Fatalf("credentials = %#v, err = %v", values, err)
	}
	if values[0].CloudflareCookies != "cf_clearance=abc; sso=drop" {
		t.Fatalf("cloudflare cookies = %q", values[0].CloudflareCookies)
	}
	data, err := adapter.MarshalCredentials(values)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"version"`) {
		t.Fatalf("export contains version metadata: %s", data)
	}
	if _, err := adapter.ParseImportedCredentials([]byte(`{"basic":["token-one"]}`)); err == nil {
		t.Fatal("legacy tier pools were accepted")
	}
}

func TestWebCredentialJSONLinesPreserveMetadata(t *testing.T) {
	adapter := &Adapter{}
	data := []byte("\xef\xbb\xbf{\"name\":\"first\",\"sso_token\":\"token-one\",\"tier\":\"super\",\"email\":\"one@example.com\"}\r\n\r\n" +
		"{\"name\":\"second\",\"token\":\"token-two\",\"user_id\":\"user-two\"}\r\n")
	values, err := adapter.ParseImportedCredentials(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].AccessToken != "token-one" || values[0].WebTier != account.WebTierSuper || values[0].Email != "one@example.com" || values[1].AccessToken != "token-two" || values[1].UserID != "user-two" {
		t.Fatalf("credentials = %#v", values)
	}
}

func TestWebCredentialJSONLinesRejectMalformedLine(t *testing.T) {
	adapter := &Adapter{}
	_, err := adapter.ParseImportedCredentials([]byte("{\"sso_token\":\"token-one\"}\ninvalid-secret\n"))
	if err == nil || !strings.Contains(err.Error(), "第 2 行") || strings.Contains(err.Error(), "invalid-secret") {
		t.Fatalf("error = %v", err)
	}
}

// 「[」为 JSON 保留前缀：顶层裸数组必须走 JSON 解析，不得落入纯文本路径静默导入。
func TestWebCredentialBareArrayUsesJSONPath(t *testing.T) {
	adapter := &Adapter{}
	values, err := adapter.ParseImportedCredentials([]byte(`[{"name":"primary","sso_token":"token-one","tier":"super","email":"one@example.com"},{"token":"token-two"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Name != "primary" || values[0].AccessToken != "token-one" || values[0].WebTier != account.WebTierSuper || values[0].Email != "one@example.com" || values[1].AccessToken != "token-two" {
		t.Fatalf("bare array values = %#v", values)
	}
}

func TestWebCredentialBareArrayWithBOM(t *testing.T) {
	adapter := &Adapter{}
	values, err := adapter.ParseImportedCredentials([]byte("\xef\xbb\xbf[{\"sso_token\":\"token-one\"}]"))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].AccessToken != "token-one" {
		t.Fatalf("bare array values = %#v", values)
	}
}

func TestWebCredentialBareArrayErrors(t *testing.T) {
	adapter := &Adapter{}
	// 空数组：JSON 路径解析出 0 个账号，而不是被当成空文本导入。
	if _, err := adapter.ParseImportedCredentials([]byte("[]")); err == nil || !strings.Contains(err.Error(), "没有 Grok Web 账号") {
		t.Fatalf("empty array error = %v", err)
	}
	// null 元素：归一化阶段带账号序号报错。
	if _, err := adapter.ParseImportedCredentials([]byte(`[{"sso_token":"token-one"},null]`)); err == nil || !strings.Contains(err.Error(), "第 2 个账号缺少 sso_token") {
		t.Fatalf("null element error = %v", err)
	}
	// 非法 [ 开头输入：明确 JSON 报错，禁止静默当纯文本导入。
	if _, err := adapter.ParseImportedCredentials([]byte("[not-json")); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("malformed array error = %v", err)
	}
}
