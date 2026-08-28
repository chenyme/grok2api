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
}

type reasoningRecoveryOutcome struct {
	encryptedContentDowngraded bool
	sessionReset               bool
	failed                     bool
	attempts                   []provider.RecoveredAttempt
}

func (o reasoningRecoveryOutcome) merge(other reasoningRecoveryOutcome) reasoningRecoveryOutcome {
	attempts := append([]provider.RecoveredAttempt{}, o.attempts...)
	attempts = append(attempts, other.attempts...)
	return reasoningRecoveryOutcome{
		encryptedContentDowngraded: o.encryptedContentDowngraded || other.encryptedContentDowngraded,
		sessionReset:               o.sessionReset || other.sessionReset,
		failed:                     o.failed || other.failed,
		attempts:                   attempts,
	}
}

func (o *reasoningRecoveryOutcome) recordHidden(stage, result string, resp *http.Response, body []byte, truncated bool) {
	if o == nil {
		return
	}
	diagnostic := provider.DiagnosticResponse{Body: append([]byte(nil), body...), BodyTruncated: truncated}
	if resp != nil {
		diagnostic.StatusCode = resp.StatusCode
		diagnostic.Status = resp.Status
		if resp.Header != nil {
			diagnostic.Header = resp.Header.Clone()
		}
	}
	o.attempts = append(o.attempts, provider.RecoveredAttempt{Stage: stage, Result: result, Diagnostic: diagnostic})
}

func (o *reasoningRecoveryOutcome) setFirstResult(result string) {
	if o == nil || len(o.attempts) == 0 {
		return
	}
	o.attempts[0].Result = result
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
	out := reasoningRecoveryOutcome{}
	out.recordHidden("reasoning_decode_rejected", "pending_recovery", original, errorBody, truncated)
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
			out.setFirstResult("transport_failed")
			out.failed = true
			return original, requestURL, out
		}
		if err := normalizeGzipResponse(retry); err != nil {
			_ = retry.Body.Close()
			a.logReasoningRecovery(request, base, "encrypted_content", "response_decode_failed", retry.StatusCode, err)
			out.setFirstResult("response_decode_failed")
			out.failed = true
			return original, requestURL, out
		}
		if isHTTPSuccess(retry.StatusCode) {
			_ = original.Body.Close()
			a.logReasoningRecovery(request, base, "encrypted_content", "recovered", retry.StatusCode, nil)
			out.setFirstResult("recovered_encrypted_content_stripped")
			out.encryptedContentDowngraded = true
			return retry, retryURL, out
		}
		if retry.StatusCode == http.StatusTooManyRequests {
			_ = original.Body.Close()
			a.logReasoningRecovery(request, base, "encrypted_content", "rate_limited", retry.StatusCode, nil)
			out.setFirstResult("replaced_by_rate_limit")
			out.encryptedContentDowngraded = true
			return retry, retryURL, out
		}
		retryBody, retryTrunc, inspectErr := provider.ReadDiagnosticBody(retry.Body)
		_ = retry.Body.Close()
		if inspectErr != nil || retryTrunc || !isReasoningDecodeFailure(retryBody) {
			a.logReasoningRecovery(request, base, "encrypted_content", "retry_rejected", retry.StatusCode, inspectErr)
			out.recordHidden("reasoning_encrypted_content_retry", "retry_rejected", retry, retryBody, retryTrunc)
			out.setFirstResult("retry_rejected")
			out.failed = true
			return original, requestURL, out
		}
		out.recordHidden("reasoning_encrypted_content_retry", "decode_error_persisted", retry, retryBody, retryTrunc)
		a.logReasoningRecovery(request, base, "encrypted_content", "decode_error_persisted", retry.StatusCode, nil)
	}

	if !canResetReasoningSession(request, portableBody) {
		a.logReasoningRecovery(request, base, "session_reset", "not_safe", 0, nil)
		out.setFirstResult("session_reset_not_safe")
		out.failed = true
		return original, requestURL, out
	}
	statelessBody := removePromptCacheKey(portableBody)
	retry, retryURL, retryErr := a.retryReasoningRecovery(ctx, request, accessToken, statelessBody, base, true)
	if retryErr != nil {
		a.logReasoningRecovery(request, base, "session_reset", "transport_failed", 0, retryErr)
		out.setFirstResult("session_reset_transport_failed")
		out.failed = true
		return original, requestURL, out
	}
	if err := normalizeGzipResponse(retry); err != nil {
		_ = retry.Body.Close()
		a.logReasoningRecovery(request, base, "session_reset", "response_decode_failed", retry.StatusCode, err)
		out.setFirstResult("session_reset_response_decode_failed")
		out.failed = true
		return original, requestURL, out
	}
	if retry.StatusCode == http.StatusTooManyRequests {
		_ = original.Body.Close()
		a.logReasoningRecovery(request, base, "session_reset", "rate_limited", retry.StatusCode, nil)
		out.setFirstResult("replaced_by_rate_limit")
		out.encryptedContentDowngraded = encryptedChanged
		out.sessionReset = true
		return retry, retryURL, out
	}
	if !isHTTPSuccess(retry.StatusCode) {
		status := retry.StatusCode
		retryBody, retryTrunc, _ := provider.ReadDiagnosticBody(retry.Body)
		_ = retry.Body.Close()
		a.logReasoningRecovery(request, base, "session_reset", "retry_rejected", status, nil)
		out.recordHidden("reasoning_session_reset", "retry_rejected", retry, retryBody, retryTrunc)
		out.setFirstResult("session_reset_rejected")
		out.failed = true
		return original, requestURL, out
	}

	_ = original.Body.Close()
	a.logReasoningRecovery(request, base, "session_reset", "recovered", retry.StatusCode, nil)
	out.setFirstResult("recovered_session_reset")
	out.encryptedContentDowngraded = encryptedChanged
	out.sessionReset = true
	return retry, retryURL, out
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
