package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestGatewayCompactionLifecycle(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	codec := newGatewayCompactionCodec(cipher)
	rawSummary := healthyCompactionSummary()
	upstream := compactionSampleSSE("resp_upstream", rawSummary)
	sample, err := parseGatewayCompactionStream([]byte(upstream))
	if err != nil {
		t.Fatal(err)
	}
	continuation := gatewayCompactionContinuation(sample.summary)
	if !strings.HasPrefix(continuation, "This session is being continued") || strings.Contains(continuation, "<summary>") || !strings.Contains(continuation, "Summary:\n1. Primary") {
		t.Fatalf("continuation = %q", continuation)
	}
	blob, err := codec.encode("session-1", continuation)
	if err != nil {
		t.Fatal(err)
	}
	stream, contentType, err := buildGatewayCompactionResponse(sample.response, blob, "grok-4.5", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.completed"} {
		if !strings.Contains(string(stream), "event: "+event) {
			t.Fatalf("compaction stream missing %s: %s", event, stream)
		}
	}
	if contentType != "text/event-stream" {
		t.Fatalf("content type = %q", contentType)
	}
	blob = compactionBlobFromSSE(t, stream)
	expanded, foreign, err := expandGatewayCompactionHistory([]byte(`{"input":[{"type":"compaction","encrypted_content":`+mustJSONString(blob)+`}]} `), codec, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if foreign != 0 || !strings.Contains(string(expanded), "This session is being continued") || strings.Contains(string(expanded), `"type":"compaction"`) || !strings.Contains(string(expanded), `"role":"user"`) {
		t.Fatalf("expanded = %s, foreign = %d", expanded, foreign)
	}
	if _, _, err := expandGatewayCompactionHistory([]byte(`{"input":[{"type":"compaction","encrypted_content":`+mustJSONString(blob)+`}]} `), codec, "other-session"); err == nil {
		t.Fatal("session mismatch must reject a gateway-owned blob")
	}
}

func TestCleanGatewayCompactionSummaryMatchesGrokBuildScratchpadRules(t *testing.T) {
	raw := "<summary>**Analysis**\nprivate draft\n</analysis>\n<summary>1. Primary Request: keep this. " + strings.Repeat("x", 520) + "</summary></summary>"
	cleaned := cleanGatewayCompactionSummary(raw)
	if strings.Contains(cleaned, "private draft") || !strings.HasPrefix(cleaned, "Summary:\n1. Primary Request") || strings.Contains(cleaned, "<summary>") {
		t.Fatalf("cleaned = %q", cleaned)
	}

	numbered := cleanGatewayCompactionSummary("<summary>1. Primary: quoted </analysis> token remains harmless.</summary>")
	if !strings.Contains(numbered, "1. Primary") || !strings.Contains(numbered, "<\u200b/analysis>") {
		t.Fatalf("numbered = %q", numbered)
	}
}

func TestPrepareGatewayCompactionSampleMatchesGrokBuild0103(t *testing.T) {
	prepared, err := prepareGatewayCompactionSample([]byte(`{
		"model":"grok-4.5","stream":true,"store":true,"instructions":"client instructions",
		"previous_response_id":"resp_old","max_output_tokens":200,
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"input":[{"type":"message","role":"user","content":"hello"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if json.Unmarshal(prepared, &payload) != nil {
		t.Fatalf("payload = %s", prepared)
	}
	if payload["stream"] != true || payload["store"] != false || payload["instructions"] != nil || payload["temperature"] != float64(1) || payload["tool_choice"] != "none" {
		t.Fatalf("sample controls = %#v", payload)
	}
	if _, exists := payload["previous_response_id"]; exists {
		t.Fatalf("previous_response_id leaked: %s", prepared)
	}
	if _, exists := payload["max_output_tokens"]; exists {
		t.Fatalf("max_output_tokens leaked: %s", prepared)
	}
	if len(payload["tools"].([]any)) != 1 || payload["reasoning"].(map[string]any)["summary"] != "concise" {
		t.Fatalf("sample tools/reasoning = %#v", payload)
	}
	items := payload["input"].([]any)
	last := items[len(items)-1].(map[string]any)
	if last["role"] != "user" || last["content"] != gatewayCompactionPrompt || strings.Contains(string(prepared), "compaction_trigger") {
		t.Fatalf("sample input = %#v", items)
	}
}

func TestPrepareGatewayCompactionSampleOmitsToolChoiceWithoutTools(t *testing.T) {
	prepared, err := prepareGatewayCompactionSample([]byte(`{"model":"grok-4.5","tool_choice":"auto","input":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if json.Unmarshal(prepared, &payload) != nil {
		t.Fatalf("payload = %s", prepared)
	}
	if _, exists := payload["tool_choice"]; exists {
		t.Fatalf("tool_choice without tools = %#v", payload["tool_choice"])
	}
}

func TestForeignCompactionNeverReachesBuildModelInput(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	expanded, foreign, err := expandGatewayCompactionHistory([]byte(`{"input":[{"type":"compaction","encrypted_content":"gAAAAABforeign-codex-replay"},{"role":"user","content":"continue"}]}`), newGatewayCompactionCodec(cipher), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if foreign != 1 || strings.Contains(string(expanded), "gAAAAABforeign-codex-replay") || strings.Contains(string(expanded), `"type":"compaction"`) {
		t.Fatalf("expanded = %s, foreign = %d", expanded, foreign)
	}
}

func TestRemoteCompactionTriggerMustBeTerminal(t *testing.T) {
	_, _, err := normalizeResponsesRequest([]byte(`{"model":"public","input":[{"type":"compaction_trigger"},{"role":"user","content":"late item"}]}`), "grok-4.5")
	var requestErr *responsesRequestError
	if err == nil || !strings.Contains(err.Error(), "最后一项") || !errors.As(err, &requestErr) || requestErr.Param != "input[0]" {
		t.Fatalf("error = %#v", err)
	}
}

func TestForwardResponseEmulatesRemoteCompactionV2(t *testing.T) {
	adapter, encrypted := newCompactionTestAdapter(t)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("upstream Accept = %q", request.Header.Get("Accept"))
		}
		data, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var payload map[string]any
		if json.Unmarshal(data, &payload) != nil {
			t.Fatalf("payload = %s", data)
		}
		if payload["stream"] != true || payload["store"] != false || payload["instructions"] != nil || payload["tool_choice"] != "none" {
			t.Fatalf("sample flags = %#v", payload)
		}
		if _, exists := payload["prompt_cache_key"]; exists {
			t.Fatalf("prompt_cache_key must stay header-only for compaction: %s", data)
		}
		items := payload["input"].([]any)
		if strings.Contains(string(data), "compaction_trigger") || items[len(items)-1].(map[string]any)["content"] != gatewayCompactionPrompt {
			t.Fatalf("compaction sample = %s", data)
		}
		return sseResponse(http.StatusOK, compactionSampleSSE("resp_sample", healthyCompactionSummary()), request), nil
	})
	response, err := adapter.ForwardResponse(t.Context(), compactionProviderRequest(encrypted))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(string(data), `"type":"compaction"`) || !strings.Contains(string(data), "response.completed") {
		t.Fatalf("response = %s, headers = %#v", data, response.Header)
	}
	if !strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "remote_compaction_v2_emulated") {
		t.Fatalf("warnings = %q", response.Header.Get("X-Grok2API-Compatibility-Warnings"))
	}
}

func TestCompactionRetriesDegenerateSamplesOnSameAccount(t *testing.T) {
	adapter, encrypted := newCompactionTestAdapter(t)
	var attempts atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempt := attempts.Add(1)
		summary := "<summary>too short</summary>"
		if attempt == 3 {
			summary = healthyCompactionSummary()
		}
		return sseResponse(http.StatusOK, compactionSampleSSE("resp_retry", summary), request), nil
	})
	request := compactionProviderRequest(encrypted)
	prepared, compatibility, err := normalizeResponsesRequest(request.Body, request.Model)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = prepareGatewayCompactionSample(prepared)
	if err != nil {
		t.Fatal(err)
	}
	response, err := adapter.forwardGatewayCompactionWithPolicy(t.Context(), request, "access-token", prepared, compatibility.warningHeader(), 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if attempts.Load() != 3 || response.StatusCode != http.StatusOK {
		t.Fatalf("attempts = %d, status = %d", attempts.Load(), response.StatusCode)
	}
}

func TestCompactionStreamErrorsUseGrokBuildRetryClassification(t *testing.T) {
	deterministic := `event: response.failed
data: {"type":"response.failed","response":{"error":{"code":"invalid_request_error","message":"bad schema"}}}

`
	_, err := parseGatewayCompactionStream([]byte(deterministic))
	if err == nil || gatewayCompactionErrorIsTransient(err) {
		t.Fatalf("deterministic error = %#v", err)
	}

	transient := `event: error
data: {"type":"error","code":"503","message":"temporarily unavailable"}

`
	_, err = parseGatewayCompactionStream([]byte(transient))
	if err == nil || !gatewayCompactionErrorIsTransient(err) {
		t.Fatalf("transient error = %#v", err)
	}
}

func TestCompactionPostProcessingFailureDoesNotRetryAnotherAccount(t *testing.T) {
	adapter, encrypted := newCompactionTestAdapter(t)
	adapter.compaction = nil
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return sseResponse(http.StatusOK, compactionSampleSSE("resp_sample", healthyCompactionSummary()), request), nil
	})
	response, err := adapter.ForwardResponse(t.Context(), compactionProviderRequest(encrypted))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway || response.Header.Get("X-Should-Retry") != "false" {
		t.Fatalf("status = %d, headers = %#v", response.StatusCode, response.Header)
	}
}

func newCompactionTestAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: "https://build.test/v1", ClientVersion: "0.2.103",
		ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", UserAgent: "grok-shell/0.2.103 (linux; x86_64)",
	}, cipher)
	return adapter, encrypted
}

func compactionProviderRequest(encrypted string) provider.ResponseResourceRequest {
	return provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1",
		Streaming: true, NormalizeBody: true,
		Body: []byte(`{"model":"public","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":[{"role":"user","content":"hello"},{"type":"compaction_trigger"}]}`),
	}
}

func compactionSampleSSE(responseID, summary string) string {
	item, _ := json.Marshal(map[string]any{
		"type": "message", "id": "msg_compact", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": summary}},
	})
	completed, _ := json.Marshal(map[string]any{
		"type": "response.completed", "response": map[string]any{
			"id": responseID, "object": "response", "status": "completed", "model": "grok-4.5",
			"output": []any{json.RawMessage(item)},
			"usage":  map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
		},
	})
	return "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + string(item) + "}\n\n" +
		"event: response.completed\ndata: " + string(completed) + "\n\n"
}

func healthyCompactionSummary() string {
	return "<summary>1. Primary Request and Intent: continue the task. 2. Key Technical Concepts: Responses and compaction. 3. Files and Code Sections: adapter.go. 4. Errors and Fixes: fixed the protocol. 5. Problem Solving: verified the flow. 6. All User Messages: continue. 7. Pending Tasks: tests. 8. Current Work: compaction. 9. Optional Next Step: verify. " + strings.Repeat("x", 600) + "</summary>"
}

func sseResponse(status int, body string, request *http.Request) *http.Response {
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Request: request,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

func compactionBlobFromSSE(t *testing.T, stream []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(stream), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil || event["type"] != "response.output_item.done" {
			continue
		}
		item, _ := event["item"].(map[string]any)
		blob, _ := item["encrypted_content"].(string)
		if blob != "" {
			return blob
		}
	}
	t.Fatal("compaction blob missing")
	return ""
}

func mustJSONString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
