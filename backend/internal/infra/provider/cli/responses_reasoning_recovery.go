package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

var reasoningDecodeFailureMarkers = [][]byte{
	[]byte("could not decode the compaction blob"),
	[]byte("could not decrypt the provided encrypted_content"),
	[]byte("invalid_encrypted_content"),
}

type reasoningRecoveryOutcome struct {
	encryptedContentDowngraded bool
	sessionReset               bool
	failed                     bool
}

func (o reasoningRecoveryOutcome) merge(other reasoningRecoveryOutcome) reasoningRecoveryOutcome {
	return reasoningRecoveryOutcome{
		encryptedContentDowngraded: o.encryptedContentDowngraded || other.encryptedContentDowngraded,
		sessionReset:               o.sessionReset || other.sessionReset,
		failed:                     o.failed || other.failed,
	}
}

func (o reasoningRecoveryOutcome) appendWarnings(header http.Header) {
	if o.encryptedContentDowngraded {
		appendCompatibilityWarning(header, "reasoning_encrypted_content_downgraded")
	}
	if o.sessionReset {
		appendCompatibilityWarning(header, "reasoning_session_reset")
	}
	if o.failed {
		appendCompatibilityWarning(header, "reasoning_recovery_failed")
	}
}

// recoverReasoningDecodeFailure handles only the upstream's explicit
// pre-generation opaque-reasoning decode rejection. Recovery never changes
// credential or Build/XAI plane:
//  1. remove replayed encrypted_content, keep any readable summary as a
//     portable assistant message, and retry in the same session;
//  2. when a 400 remains (or no opaque item exists), clear the server-side
//     session identity and retry once with the full portable input.
//
// If recovery is unsuccessful, the original 400 is returned with
// reasoning_recovery_failed so the Gateway can rotate accounts. The Provider
// itself never changes credential.
func (a *Adapter) recoverReasoningDecodeFailure(
	ctx context.Context,
	request provider.ResponseResourceRequest,
	accessToken string,
	body []byte,
	base string,
	replayKey string,
	response *http.Response,
	requestURL string,
) (*http.Response, string, reasoningRecoveryOutcome, error) {
	if response == nil || response.StatusCode != http.StatusBadRequest {
		return response, requestURL, reasoningRecoveryOutcome{}, nil
	}
	errorBody, truncated, err := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return cloneBufferedResponse(response, errorBody, truncated), requestURL, reasoningRecoveryOutcome{}, nil
	}
	original := cloneBufferedResponse(response, errorBody, truncated)
	if truncated || !isReasoningDecodeFailure(errorBody) {
		return original, requestURL, reasoningRecoveryOutcome{}, nil
	}
	// 一旦上游明确拒绝 opaque reasoning，立即清理该账号/平面的服务端回放，
	// 防止下次请求再次注入同一份已失效密文。成功响应会按正常 Capture 流程写回新状态。
	if a.replay != nil && replayKey != "" {
		a.replay.Clear(ctx, request.Model, replayKey)
	}

	portableBody, encryptedChanged := stripReasoningEncryptedContent(body)
	if encryptedChanged {
		retry, retryURL, retryErr := a.retryReasoningRecovery(ctx, request, accessToken, portableBody, base, false)
		if retryErr != nil {
			a.logReasoningRecovery(request, base, "encrypted_content", "transport_failed", 0, retryErr)
			_ = original.Body.Close()
			return nil, requestURL, reasoningRecoveryOutcome{}, retryErr
		}
		if err := normalizeGzipResponse(retry); err != nil {
			_ = retry.Body.Close()
			a.logReasoningRecovery(request, base, "encrypted_content", "response_decode_failed", retry.StatusCode, err)
			_ = original.Body.Close()
			return nil, retryURL, reasoningRecoveryOutcome{}, err
		}
		if retry.StatusCode != http.StatusBadRequest {
			_ = original.Body.Close()
			result := "retry_response"
			if isHTTPSuccess(retry.StatusCode) {
				result = "recovered"
			}
			a.logReasoningRecovery(request, base, "encrypted_content", result, retry.StatusCode, nil)
			return retry, retryURL, reasoningRecoveryOutcome{encryptedContentDowngraded: true}, nil
		}
		retryStatus := retry.StatusCode
		sameDecodeFailure, inspectErr := responseHasReasoningDecodeFailure(retry)
		if inspectErr != nil {
			a.logReasoningRecovery(request, base, "encrypted_content", "retry_rejected", retryStatus, inspectErr)
			_ = original.Body.Close()
			return nil, retryURL, reasoningRecoveryOutcome{}, inspectErr
		}
		if sameDecodeFailure {
			a.logReasoningRecovery(request, base, "encrypted_content", "decode_error_persisted", retryStatus, nil)
		} else {
			// Stripping ciphertext can reword the 400 (for example a bare
			// reasoning item). Keep going to session reset instead of aborting.
			a.logReasoningRecovery(request, base, "encrypted_content", "retry_still_400", retryStatus, nil)
		}
	}

	if !canResetReasoningSession(request, portableBody) {
		a.logReasoningRecovery(request, base, "session_reset", "not_safe", 0, nil)
		return original, requestURL, reasoningRecoveryOutcome{failed: true}, nil
	}
	statelessBody := removePromptCacheKey(portableBody)
	retry, retryURL, retryErr := a.retryReasoningRecovery(ctx, request, accessToken, statelessBody, base, true)
	if retryErr != nil {
		a.logReasoningRecovery(request, base, "session_reset", "transport_failed", 0, retryErr)
		_ = original.Body.Close()
		return nil, requestURL, reasoningRecoveryOutcome{}, retryErr
	}
	if err := normalizeGzipResponse(retry); err != nil {
		_ = retry.Body.Close()
		a.logReasoningRecovery(request, base, "session_reset", "response_decode_failed", retry.StatusCode, err)
		_ = original.Body.Close()
		return nil, retryURL, reasoningRecoveryOutcome{}, err
	}
	if retry.StatusCode != http.StatusBadRequest {
		_ = original.Body.Close()
		result := "retry_response"
		if isHTTPSuccess(retry.StatusCode) {
			result = "recovered"
		}
		a.logReasoningRecovery(request, base, "session_reset", result, retry.StatusCode, nil)
		return retry, retryURL, reasoningRecoveryOutcome{
			encryptedContentDowngraded: encryptedChanged,
			sessionReset:               true,
		}, nil
	}

	_ = retry.Body.Close()
	a.logReasoningRecovery(request, base, "session_reset", "retry_rejected", retry.StatusCode, nil)
	return original, requestURL, reasoningRecoveryOutcome{failed: true}, nil
}

func (a *Adapter) retryReasoningRecovery(ctx context.Context, request provider.ResponseResourceRequest, accessToken string, body []byte, base string, resetSession bool) (*http.Response, string, error) {
	retryRequest := request
	retryRequest.IdempotencyID, _ = security.NewOpaqueToken(18)
	stage := "reasoning_replay"
	if resetSession {
		retryRequest.PromptCacheKey = ""
		retryRequest.GrokTurnIndex = ""
		stage = "reasoning_session_reset"
	}
	return a.doResponseRequest(infraegress.WithPhysicalCallStage(ctx, stage), retryRequest, accessToken, body, base)
}

func responseHasReasoningDecodeFailure(response *http.Response) (bool, error) {
	if response == nil || response.StatusCode != http.StatusBadRequest {
		if response != nil {
			_ = response.Body.Close()
		}
		return false, nil
	}
	body, truncated, err := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return false, err
	}
	return !truncated && isReasoningDecodeFailure(body), nil
}

func canResetReasoningSession(request provider.ResponseResourceRequest, body []byte) bool {
	if request.Method != http.MethodPost || strings.TrimSpace(request.PromptCacheKey) == "" {
		return false
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	previousResponseID, _ := payload["previous_response_id"].(string)
	return strings.TrimSpace(previousResponseID) == ""
}

func removePromptCacheKey(body []byte) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	delete(payload, "prompt_cache_key")
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

func (a *Adapter) logReasoningRecovery(request provider.ResponseResourceRequest, base, stage, result string, status int, err error) {
	plane := "build"
	if fallback := a.fallbackBaseURL(); fallback != "" && strings.EqualFold(strings.TrimRight(base, "/"), fallback) {
		plane = "xai"
	}
	attributes := []any{
		"account_id", request.Credential.ID,
		"model", request.Model,
		"operation", request.Operation,
		"plane", plane,
		"stage", stage,
		"result", result,
	}
	if status != 0 {
		attributes = append(attributes, "status", status)
	}
	if err != nil {
		attributes = append(attributes, "error", err)
	}
	logger := a.logger
	if logger != nil {
		logger.Warn("reasoning_decode_recovery", attributes...)
	}
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

// stripReasoningEncryptedContent removes undecodable opaque reasoning and
// compaction ciphertext so Grok Build does not fail on server-side decryption.
// Readable reasoning summaries are kept as portable assistant messages; empty
// encrypted-only reasoning items are dropped. Foreign compaction blobs become
// a boundary note because this gateway cannot decrypt them.
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
		if !ok {
			rebuilt = append(rebuilt, raw)
			continue
		}
		switch stringField(item, "type") {
		case "reasoning":
			encrypted, hasEncrypted := item["encrypted_content"].(string)
			if !hasEncrypted || strings.TrimSpace(encrypted) == "" {
				if portable, ok := portableReasoningSummaryMessage(item); ok {
					changed = true
					rebuilt = append(rebuilt, portable)
					continue
				}
				rebuilt = append(rebuilt, raw)
				continue
			}
			changed = true
			if portable, ok := portableReasoningSummaryMessage(item); ok {
				rebuilt = append(rebuilt, portable)
			}
		case "compaction":
			changed = true
			if portable, ok := portableReasoningSummaryMessage(item); ok {
				rebuilt = append(rebuilt, portable)
				continue
			}
			rebuilt = append(rebuilt, compatibilityBoundaryMessage("A prior compacted context could not be decoded by upstream. Continue from the retained conversation messages."))
		default:
			rebuilt = append(rebuilt, raw)
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

func portableReasoningSummaryMessage(item map[string]any) (map[string]any, bool) {
	text := reasoningPortableText(item)
	if text == "" {
		return nil, false
	}
	return map[string]any{
		"type": "message", "role": "assistant",
		"content": "Prior model reasoning summary:\n" + text,
	}, true
}

func reasoningPortableText(item map[string]any) string {
	var parts []string
	for _, field := range []string{"summary", "content"} {
		values, _ := item[field].([]any)
		for _, raw := range values {
			part, _ := raw.(map[string]any)
			if text := strings.TrimSpace(stringField(part, "text")); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
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
