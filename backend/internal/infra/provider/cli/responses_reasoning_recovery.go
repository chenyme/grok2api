package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

var reasoningDecodeFailureMarkers = [][]byte{
	[]byte("could not decode the compaction blob"),
	[]byte("could not decrypt the provided encrypted_content"),
}

// recoverReasoningDecodeFailure handles only the upstream's explicit
// pre-generation opaque-reasoning decode rejection. The retry uses the same
// credential, base URL and idempotency key. If the downgraded request is not
// successful, the original 400 is returned so the Gateway does not rotate to
// another account or obscure the first failure.
func (a *Adapter) recoverReasoningDecodeFailure(
	ctx context.Context,
	request provider.ResponseResourceRequest,
	accessToken string,
	body []byte,
	base string,
	response *http.Response,
	requestURL string,
) (*http.Response, string, bool) {
	if response == nil || response.StatusCode != http.StatusBadRequest {
		return response, requestURL, false
	}
	downgraded, changed := stripReasoningEncryptedContent(body)
	if !changed {
		return response, requestURL, false
	}
	errorBody, truncated, err := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return cloneBufferedResponse(response, errorBody, truncated), requestURL, false
	}
	original := cloneBufferedResponse(response, errorBody, truncated)
	if truncated || !isReasoningDecodeFailure(errorBody) {
		return original, requestURL, false
	}

	// The fallback body differs from the rejected request, so it must not reuse
	// the original Idempotency-Key. The first request failed during input
	// validation before generation, making a fresh key safe here.
	retryRequest := request
	retryRequest.IdempotencyID, _ = security.NewOpaqueToken(18)
	retry, retryURL, retryErr := a.doResponseRequest(ctx, retryRequest, accessToken, downgraded, base)
	if retryErr != nil {
		return original, requestURL, false
	}
	if err := normalizeGzipResponse(retry); err != nil {
		_ = retry.Body.Close()
		return original, requestURL, false
	}
	if !isHTTPSuccess(retry.StatusCode) {
		_ = retry.Body.Close()
		return original, requestURL, false
	}
	_ = original.Body.Close()
	return retry, retryURL, true
}

func isReasoningDecodeFailure(body []byte) bool {
	lower := bytes.ToLower(body)
	for _, marker := range reasoningDecodeFailureMarkers {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// stripReasoningEncryptedContent removes opaque reasoning state while
// preserving any readable summary/content. An encrypted-only reasoning item
// becomes empty after stripping and is removed entirely.
func stripReasoningEncryptedContent(body []byte) ([]byte, bool) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body, false
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) == 0 {
		return body, false
	}
	changed := false
	rebuilt := make([]any, 0, len(input))
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || stringField(item, "type") != "reasoning" {
			rebuilt = append(rebuilt, raw)
			continue
		}
		encrypted, ok := item["encrypted_content"].(string)
		if !ok || strings.TrimSpace(encrypted) == "" {
			rebuilt = append(rebuilt, raw)
			continue
		}
		cleaned := cloneJSONObject(item)
		delete(cleaned, "encrypted_content")
		delete(cleaned, "id")
		delete(cleaned, "status")
		changed = true
		if hasReadableReasoningContent(cleaned) {
			rebuilt = append(rebuilt, cleaned)
		}
	}
	if !changed {
		return body, false
	}
	payload["input"] = rebuilt
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return encoded, true
}

func hasReadableReasoningContent(item map[string]any) bool {
	for _, field := range []string{"summary", "content"} {
		parts, _ := item[field].([]any)
		for _, raw := range parts {
			part, _ := raw.(map[string]any)
			if strings.TrimSpace(stringField(part, "text")) != "" {
				return true
			}
		}
	}
	return false
}

func appendCompatibilityWarning(header http.Header, warning string) {
	if header == nil || strings.TrimSpace(warning) == "" {
		return
	}
	existing := strings.TrimSpace(header.Get("X-Grok2API-Compatibility-Warnings"))
	if existing == "" {
		header.Set("X-Grok2API-Compatibility-Warnings", warning)
		return
	}
	for _, value := range strings.Split(existing, ",") {
		if strings.TrimSpace(value) == warning {
			return
		}
	}
	header.Set("X-Grok2API-Compatibility-Warnings", existing+","+warning)
}
