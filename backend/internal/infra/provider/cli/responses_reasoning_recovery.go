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
	[]byte("could not decrypt the provided encrypted_content"),
	[]byte("invalid_encrypted_content"),
}

var compactionBlobDecodeFailureMarkers = [][]byte{
	[]byte("could not decode the compaction blob"),
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

// recordHidden 追加一次确实发出的、但未作为最终响应返回的上游调用。
func (o *reasoningRecoveryOutcome) recordHidden(stage, result string, call responseCall, body []byte, truncated bool, failure error) {
	if o == nil {
		return
	}
	diagnostic := provider.DiagnosticResponse{Body: append([]byte(nil), body...), BodyTruncated: truncated}
	if call.response != nil {
		diagnostic.StatusCode = call.response.StatusCode
		diagnostic.Status = call.response.Status
		if call.response.Header != nil {
			diagnostic.Header = call.response.Header.Clone()
		}
	}
	o.attempts = append(o.attempts, provider.RecoveredAttempt{
		Stage: stage, Result: result, UpstreamURL: call.upstreamURL,
		StartedAt: call.startedAt, DurationMS: call.durationMS,
		Diagnostic: diagnostic, Failure: failure,
	})
}

// prependOriginal 在最终响应已被替换时，把最初的 decode 400 放回真实调用序列首位。
func (o *reasoningRecoveryOutcome) prependOriginal(result string, call responseCall, body []byte, truncated bool) {
	if o == nil {
		return
	}
	previous := o.attempts
	o.attempts = nil
	o.recordHidden("reasoning_decode_rejected", result, call, body, truncated, nil)
	o.attempts = append(o.attempts, previous...)
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

// recoverReasoningDecodeFailure 只处理上游在生成前明确返回的 opaque reasoning 解码失败。
// 恢复始终留在同一账号和同一 Build/XAI 平面：先移除 encrypted_content 并保留可读摘要，
// 若仍为 400 再清空服务端会话身份重试。最终仍失败时返回原始 400 和内部失败标记，由网关换号。
func (a *Adapter) recoverReasoningDecodeFailure(
	ctx context.Context,
	request provider.ResponseResourceRequest,
	accessToken string,
	body []byte,
	base string,
	replayKey string,
	initialCall responseCall,
) (responseCall, reasoningRecoveryOutcome, error) {
	response := initialCall.response
	if response == nil || response.StatusCode != http.StatusBadRequest {
		return initialCall, reasoningRecoveryOutcome{}, nil
	}
	errorBody, truncated, err := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	if err != nil {
		initialCall.response = cloneBufferedResponse(response, errorBody, truncated)
		return initialCall, reasoningRecoveryOutcome{}, nil
	}
	original := cloneBufferedResponse(response, errorBody, truncated)
	originalCall := initialCall
	originalCall.response = original
	if truncated || !isReasoningDecodeFailure(errorBody) {
		return originalCall, reasoningRecoveryOutcome{}, nil
	}
	// Build historically reused the compaction-decode wording for opaque
	// reasoning and server-side session failures. Only treat that wording as a
	// real compaction rejection when the rejected request actually carried a
	// compaction input item. This preserves reasoning recovery without ever
	// rewriting a client-held compact state.
	if isCompactionBlobDecodeFailure(errorBody) && hasCompactionInputItem(body) {
		return originalCall, reasoningRecoveryOutcome{}, nil
	}
	out := reasoningRecoveryOutcome{}
	// 一旦上游明确拒绝 opaque reasoning，立即清理该账号/平面的服务端回放，
	// 防止下次请求再次注入同一份已失效密文。成功响应会按正常 Capture 流程写回新状态。
	if a.replay != nil && replayKey != "" {
		a.replay.Clear(ctx, request.Model, replayKey)
	}

	portableBody, encryptedChanged := stripReasoningEncryptedContent(body)
	if encryptedChanged {
		retryCall := a.retryReasoningRecovery(ctx, request, accessToken, portableBody, base, false)
		if retryCall.err != nil {
			a.logReasoningRecovery(request, base, "encrypted_content", "transport_failed", 0, retryCall.err)
			_ = original.Body.Close()
			return responseCall{}, out, retryCall.err
		}
		if err := normalizeGzipResponse(retryCall.response); err != nil {
			_ = retryCall.response.Body.Close()
			a.logReasoningRecovery(request, base, "encrypted_content", "response_decode_failed", retryCall.response.StatusCode, err)
			_ = original.Body.Close()
			return responseCall{}, out, err
		}
		if retryCall.response.StatusCode != http.StatusBadRequest {
			_ = original.Body.Close()
			logResult := "retry_response"
			auditResult := "replaced_by_retry_response"
			if isHTTPSuccess(retryCall.response.StatusCode) {
				logResult = "recovered"
				auditResult = "recovered_encrypted_content_stripped"
			} else if retryCall.response.StatusCode == http.StatusTooManyRequests {
				auditResult = "replaced_by_rate_limit"
			}
			a.logReasoningRecovery(request, base, "encrypted_content", logResult, retryCall.response.StatusCode, nil)
			out.prependOriginal(auditResult, originalCall, errorBody, truncated)
			out.encryptedContentDowngraded = true
			return retryCall, out, nil
		}
		retryBody, retryTrunc, inspectErr := provider.ReadDiagnosticBody(retryCall.response.Body)
		_ = retryCall.response.Body.Close()
		if inspectErr != nil {
			a.logReasoningRecovery(request, base, "encrypted_content", "retry_rejected", retryCall.response.StatusCode, inspectErr)
			_ = original.Body.Close()
			return responseCall{}, out, inspectErr
		}
		retryResult := "retry_still_400"
		if !retryTrunc && isReasoningDecodeFailure(retryBody) {
			retryResult = "decode_error_persisted"
		}
		out.recordHidden("reasoning_encrypted_content_retry", retryResult, retryCall, retryBody, retryTrunc, nil)
		a.logReasoningRecovery(request, base, "encrypted_content", retryResult, retryCall.response.StatusCode, nil)
	}

	if !canResetReasoningSession(request, portableBody) {
		a.logReasoningRecovery(request, base, "session_reset", "not_safe", 0, nil)
		out.failed = true
		return originalCall, out, nil
	}
	statelessBody := removePromptCacheKey(portableBody)
	retryCall := a.retryReasoningRecovery(ctx, request, accessToken, statelessBody, base, true)
	if retryCall.err != nil {
		a.logReasoningRecovery(request, base, "session_reset", "transport_failed", 0, retryCall.err)
		_ = original.Body.Close()
		return responseCall{}, out, retryCall.err
	}
	if err := normalizeGzipResponse(retryCall.response); err != nil {
		_ = retryCall.response.Body.Close()
		a.logReasoningRecovery(request, base, "session_reset", "response_decode_failed", retryCall.response.StatusCode, err)
		_ = original.Body.Close()
		return responseCall{}, out, err
	}
	if retryCall.response.StatusCode != http.StatusBadRequest {
		_ = original.Body.Close()
		logResult := "retry_response"
		auditResult := "replaced_by_retry_response"
		if isHTTPSuccess(retryCall.response.StatusCode) {
			logResult = "recovered"
			auditResult = "recovered_session_reset"
		} else if retryCall.response.StatusCode == http.StatusTooManyRequests {
			auditResult = "replaced_by_rate_limit"
		}
		a.logReasoningRecovery(request, base, "session_reset", logResult, retryCall.response.StatusCode, nil)
		out.prependOriginal(auditResult, originalCall, errorBody, truncated)
		out.encryptedContentDowngraded = encryptedChanged
		out.sessionReset = true
		return retryCall, out, nil
	}

	retryBody, retryTrunc, inspectErr := provider.ReadDiagnosticBody(retryCall.response.Body)
	_ = retryCall.response.Body.Close()
	a.logReasoningRecovery(request, base, "session_reset", "retry_rejected", retryCall.response.StatusCode, inspectErr)
	out.recordHidden("reasoning_session_reset", "retry_rejected", retryCall, retryBody, retryTrunc, inspectErr)
	out.failed = true
	return originalCall, out, nil
}

// retryReasoningRecovery 使用新的幂等键执行同账号、同平面的恢复调用。
func (a *Adapter) retryReasoningRecovery(ctx context.Context, request provider.ResponseResourceRequest, accessToken string, body []byte, base string, resetSession bool) responseCall {
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
	return isCompactionBlobDecodeFailure(body) || containsDecodeFailureMarker(body, reasoningDecodeFailureMarkers)
}

func isCompactionBlobDecodeFailure(body []byte) bool {
	return containsDecodeFailureMarker(body, compactionBlobDecodeFailureMarkers)
}

func containsDecodeFailureMarker(body []byte, markers [][]byte) bool {
	lower := bytes.ToLower(body)
	for _, marker := range markers {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func hasCompactionInputItem(body []byte) bool {
	var payload struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	for _, item := range payload.Input {
		if strings.TrimSpace(item.Type) == "compaction" {
			return true
		}
	}
	return false
}

// stripReasoningEncryptedContent removes only undecodable opaque reasoning.
// Readable reasoning summaries are kept as portable assistant messages; empty
// encrypted-only reasoning items are dropped. Compaction items are client-held
// upstream state and must never be rewritten by reasoning recovery.
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
		if stringField(item, "type") == "reasoning" {
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
			continue
		}
		rebuilt = append(rebuilt, raw)
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
