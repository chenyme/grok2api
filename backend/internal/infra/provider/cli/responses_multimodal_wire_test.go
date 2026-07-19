package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestForwardResponseCapturesNativeMultimodalFunctionOutput(t *testing.T) {
	var captured map[string]any
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_1"}`)), Request: request}, nil
	})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", Operation: conversation.OperationResponses, NormalizeBody: true,
		Body: []byte(`{"model":"public","input":[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"read"},{"type":"input_image","detail":"auto","image_url":"data:image/png;base64,AA=="}]}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	assertWireFunctionOutputArray(t, captured)
}

func TestForwardResponseCapturesChatAndAnthropicMultimodalToolResults(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		body      string
	}{
		{
			name:      "chat",
			operation: conversation.OperationChat,
			body:      `{"model":"public","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"read"},{"type":"image_url","image_url":"data:image/png;base64,AA=="}]}]}`,
		},
		{
			name:      "anthropic",
			operation: conversation.OperationMessages,
			body:      `{"model":"public","max_tokens":64,"tools":[{"name":"read_file","input_schema":{"type":"object"}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"read"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var captured map[string]any
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				t.Fatal(err)
			}
			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(key))
			if err != nil {
				t.Fatal(err)
			}
			encrypted, err := cipher.Encrypt("access-token")
			if err != nil {
				t.Fatal(err)
			}
			adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(body, &captured); err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_1"}`)), Request: request}, nil
			})
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: account.Credential{Provider: account.ProviderBuild, EncryptedAccessToken: encrypted}, Method: http.MethodPost, Path: "/responses", Model: "grok-4.5", Operation: test.operation, NormalizeBody: true, Body: []byte(test.body),
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			assertWireFunctionOutputArray(t, captured)
		})
	}
}

func assertWireFunctionOutputArray(t *testing.T, captured map[string]any) {
	t.Helper()
	items, ok := captured["input"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("captured input = %#v", captured["input"])
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "function_call_output" {
			continue
		}
		output, ok := item["output"].([]any)
		if !ok || len(output) != 2 {
			t.Fatalf("function output was stringified: %#v", item["output"])
		}
		image := output[1].(map[string]any)
		if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,AA==" {
			t.Fatalf("image wire = %#v", image)
		}
		return
	}
	t.Fatalf("function_call_output not found in %#v", items)
}
