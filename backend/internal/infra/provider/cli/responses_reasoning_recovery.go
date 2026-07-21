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
//  1. remove replayed encrypted_content and retry in the same session;
//  2. when the same decode error remains (or no opaque item exists), clear the
//     server-side session identity and retry once with the full portable input.
//
// If recovery is unsuccessful, the original 400 is returned so the Gateway
// does not rotate accounts or obscure the first failure.
func (a *Adapter) recoverReasoningDecodeFailure(
	ctx context.Context,
	request provider.ResponseResourceRequest,
	accessToken string,
	body []byte,
	base string,
	replayKey string,
	response *http.Response,
	requestURL string,
) (*http.Response, string, reasoningRecoveryOutcome) {
	if response == nil || response.StatusCode != http.StatusBadRequest {
		return response, requestURL, reasoningRecoveryOutcome{}
	}
	errorBody, truncated, err := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return cloneBufferedResponse(response, errorBody, truncated), requestURL, reasoningRecoveryOutcome{}
	}
	original := cloneBufferedResponse(response, errorBody, truncated)
	if truncated || !isReasoningDecodeFailure(errorBody) {
		return original, requestURL, reasoningRecoveryOutcome{}
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
			return original, requestURL, reasoningRecoveryOutcome{failed: true}
		}
		if err := normalizeGzipResponse(retry); err != nil {
			_ = retry.Body.Close()
			a.logReasoningRecovery(request, base, "encrypted_content", "response_decode_failed", retry.StatusCode, err)
			return original, requestURL, reasoningRecoveryOutcome{failed: true}
		}
		if isHTTPSuccess(retry.StatusCode) {
			_ = original.Body.Close()
			a.logReasoningRecovery(request, base, "encrypted_content", "recovered", retry.StatusCode, nil)
			return retry, retryURL, reasoningRecoveryOutcome{encryptedContentDowngraded: true}
		}
		if retry.StatusCode == http.StatusTooManyRequests {
			// 去除失效密文后得到的 429 是当前账号的真实上游状态。保留它，
			// 让网关进行账号冷却和切换，不能回退成已无效的初始解码 400。
			_ = original.Body.Close()
			a.logReasoningRecovery(request, base, "encrypted_content", "rate_limited", retry.StatusCode, nil)
			return retry, retryURL, reasoningRecoveryOutcome{encryptedContentDowngraded: true}
		}
		sameDecodeFailure, inspectErr := responseHasReasoningDecodeFailure(retry)
		if inspectErr != nil || !sameDecodeFailure {
			a.logReasoningRecovery(request, base, "encrypted_content", "retry_rejected", retry.StatusCode, inspectErr)
			return original, requestURL, reasoningRecoveryOutcome{failed: true}
		}
		a.logReasoningRecovery(request, base, "encrypted_content", "decode_error_persisted", retry.StatusCode, nil)
	}

	if !canResetReasoningSession(request, portableBody) {
		a.logReasoningRecovery(request, base, "session_reset", "not_safe", 0, nil)
		return original, requestURL, reasoningRecoveryOutcome{failed: true}
	}
	statelessBody := removePromptCacheKey(portableBody)
	retry, retryURL, retryErr := a.retryReasoningRecovery(ctx, request, accessToken, statelessBody, base, true)
	if retryErr != nil {
		a.logReasoningRecovery(request, base, "session_reset", "transport_failed", 0, retryErr)
		return original, requestURL, reasoningRecoveryOutcome{failed: true}
	}
	if err := normalizeGzipResponse(retry); err != nil {
		_ = retry.Body.Close()
		a.logReasoningRecovery(request, base, "session_reset", "response_decode_failed", retry.StatusCode, err)
		return original, requestURL, reasoningRecoveryOutcome{failed: true}
	}
	if retry.StatusCode == http.StatusTooManyRequests {
		// 无状态恢复也可能命中当前账号的真实限流。与去密文恢复保持一致，
		// 必须把 429 交回网关，才能执行账号冷却和候选账号切换。
		_ = original.Body.Close()
		a.logReasoningRecovery(request, base, "session_reset", "rate_limited", retry.StatusCode, nil)
		return retry, retryURL, reasoningRecoveryOutcome{
			encryptedContentDowngraded: encryptedChanged,
			sessionReset:               true,
		}
	}
	if !isHTTPSuccess(retry.StatusCode) {
		status := retry.StatusCode
		_ = retry.Body.Close()
		a.logReasoningRecovery(request, base, "session_reset", "retry_rejected", status, nil)
		return original, requestURL, reasoningRecoveryOutcome{failed: true}
	}

	_ = original.Body.Close()
	a.logReasoningRecovery(request, base, "session_reset", "recovered", retry.StatusCode, nil)
	return retry, retryURL, reasoningRecoveryOutcome{
		encryptedContentDowngraded: encryptedChanged,
		sessionReset:               true,
	}
}

func (a *Adapter) retryReasoningRecovery(ctx context.Context, request provider.ResponseResourceRequest, accessToken string, body []byte, base string, resetSession bool) (*http.Response, string, error) {
	retryRequest := request
	retryRequest.IdempotencyID, _ = security.NewOpaqueToken(18)
	if resetSession {
		retryRequest.PromptCacheKey = ""
		retryRequest.GrokTurnIndex = ""
	}
	return a.doResponseRequest(ctx, retryRequest, accessToken, body, base)
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
