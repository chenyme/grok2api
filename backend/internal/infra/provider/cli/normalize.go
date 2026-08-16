package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

// normalizeResponsesRequest 改写路由字段和兼容别名，并为上游不支持的新工具协议建立请求级映射。
func normalizeResponsesRequest(body []byte, model string) ([]byte, *responsesToolCompatibility, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("解析 Responses 请求: %w", err)
	}
	payload["model"] = mustJSON(model)
	if _, err := normalizeBuildRequestPayload(payload, model, conversation.OperationResponses); err != nil {
		return nil, nil, err
	}
	if responseFormat, exists := payload["response_format"]; exists {
		var text map[string]json.RawMessage
		if raw := payload["text"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, nil, fmt.Errorf("解析 text: %w", err)
			}
		}
		if text == nil {
			text = make(map[string]json.RawMessage)
		}
		if isEmptyJSON(text["format"]) {
			formatted, err := normalizeResponseFormat(responseFormat)
			if err != nil {
				return nil, nil, err
			}
			text["format"] = formatted
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, nil, err
		}
		payload["text"] = encoded
		delete(payload, "response_format")
	}
	patchReasoningTextTypes(payload)
	compatibility, err := normalizeResponsesTools(payload)
	if err != nil {
		return nil, nil, err
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	return normalized, compatibility, nil
}

// normalizeBuildRequest applies the stable compatibility boundary shared by Responses,
// Chat Completions, and Anthropic Messages before the request reaches Grok Build.
func normalizeBuildRequest(body []byte, model, operation string) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 Build 请求: %w", err)
	}
	changed, err := normalizeBuildRequestPayload(payload, model, operation)
	if err != nil {
		return nil, err
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(payload)
}

func normalizeBuildRequestPayload(payload map[string]json.RawMessage, model, operation string) (bool, error) {
	changed := false
	// client_metadata is a Codex transport envelope and may contain local paths,
	// repository remotes, and installation/session identifiers. It is consumed by
	// the HTTP boundary for cache affinity and must never cross into Grok Build.
	if _, exists := payload["client_metadata"]; exists {
		delete(payload, "client_metadata")
		changed = true
	}
	if normalizeBuildReasoningEffortPayload(payload, model) {
		changed = true
	}
	// grok-build 1.0.4 always requests a concise reasoning summary from its
	// Responses backend, even when it uses the model's default effort. Chat
	// Completions has no separate summary parameter, so make that Build-specific
	// compatibility contract explicit without changing native Responses,
	// Console, or Web semantics.
	if operation == conversation.OperationChat {
		var reasoning map[string]json.RawMessage
		if raw := payload["reasoning"]; !isEmptyJSON(raw) {
			if err := json.Unmarshal(raw, &reasoning); err != nil {
				return false, fmt.Errorf("解析 Build reasoning: %w", err)
			}
		}
		if reasoning == nil {
			reasoning = make(map[string]json.RawMessage)
		}
		if isEmptyJSON(reasoning["summary"]) {
			reasoning["summary"] = mustJSON("concise")
			payload["reasoning"] = mustJSON(reasoning)
			changed = true
		}
	}
	defaultsChanged, err := applyBuildResponseDefaults(payload)
	if err != nil {
		return false, err
	}
	return changed || defaultsChanged, nil
}

// applyBuildResponseDefaults mirrors the official Grok Build client boundary.
// Explicit store=true remains a caller choice; only an absent/null value is made ZDR-safe.
func applyBuildResponseDefaults(payload map[string]json.RawMessage) (bool, error) {
	changed := false
	if raw, exists := payload["store"]; !exists || isEmptyJSON(raw) {
		payload["store"] = mustJSON(false)
		changed = true
	}

	var includes []string
	if raw, exists := payload["include"]; exists && !isEmptyJSON(raw) {
		if err := json.Unmarshal(raw, &includes); err != nil {
			return false, fmt.Errorf("解析 Build include: %w", err)
		}
	}
	for _, value := range includes {
		if value == "reasoning.encrypted_content" {
			return changed, nil
		}
	}
	includes = append(includes, "reasoning.encrypted_content")
	payload["include"] = mustJSON(includes)
	return true, nil
}

// normalizeBuildReasoningEffortPayload maps client aliases to levels accepted by
// the selected Grok model. Grok 4.5 and unknown models retain the proven defensive
// xhigh/max -> high behavior for models without an xhigh wire contract. Models
// that explicitly support xhigh keep xhigh and map the client-only max alias to
// that highest verified upstream level.
func normalizeBuildReasoningEffortPayload(payload map[string]json.RawMessage, model string) bool {
	raw, exists := payload["reasoning"]
	if !exists || isEmptyJSON(raw) {
		return false
	}
	var reasoning map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reasoning); err != nil || reasoning == nil {
		return false
	}
	// CPA and the current xAI model registry both treat Composer as a model
	// without configurable thinking levels. Preserve other reasoning controls
	// such as summary, but never forward reasoning.effort to Composer.
	if modeldomain.IsGrokComposerModel(model) {
		if _, exists := reasoning["effort"]; !exists {
			return false
		}
		delete(reasoning, "effort")
		if len(reasoning) == 0 {
			delete(payload, "reasoning")
		} else {
			payload["reasoning"] = mustJSON(reasoning)
		}
		return true
	}
	var effort string
	if err := json.Unmarshal(reasoning["effort"], &effort); err != nil {
		return false
	}
	var normalized string
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "xhigh":
		if modeldomain.SupportsReasoningEffort(model, modeldomain.ReasoningEffortXHigh) {
			normalized = modeldomain.ReasoningEffortXHigh
		} else {
			normalized = modeldomain.ReasoningEffortHigh
		}
	case "max":
		if modeldomain.SupportsReasoningEffort(model, modeldomain.ReasoningEffortXHigh) {
			normalized = modeldomain.ReasoningEffortXHigh
		} else {
			normalized = modeldomain.ReasoningEffortHigh
		}
	default:
		return false
	}
	if effort == normalized {
		return false
	}
	reasoning["effort"] = mustJSON(normalized)
	payload["reasoning"] = mustJSON(reasoning)
	return true
}

// patchReasoningTextTypes 对齐官方 CLI 的序列化后修补：Responses 上游要求
// reasoning.content[*] 必须携带 type=reasoning_text，即使部分客户端只发送 text。
func patchReasoningTextTypes(payload map[string]json.RawMessage) {
	raw := payload["input"]
	if isEmptyJSON(raw) {
		return
	}
	var items []any
	if json.Unmarshal(raw, &items) != nil {
		return // 字符串输入或其他合法简写不需要处理。
	}
	changed := false
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || item["type"] != "reasoning" {
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, rawContent := range content {
			value, ok := rawContent.(map[string]any)
			if !ok {
				continue
			}
			if _, exists := value["type"]; !exists {
				value["type"] = "reasoning_text"
				changed = true
			}
		}
	}
	if changed {
		payload["input"] = mustJSON(items)
	}
}

func normalizeResponseFormat(raw json.RawMessage) (json.RawMessage, error) {
	var format map[string]json.RawMessage
	if err := json.Unmarshal(raw, &format); err != nil {
		return nil, fmt.Errorf("解析 response_format: %w", err)
	}
	var formatType string
	_ = json.Unmarshal(format["type"], &formatType)
	if formatType != "json_schema" || isEmptyJSON(format["json_schema"]) {
		return raw, nil
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(format["json_schema"], &schema); err != nil {
		return nil, fmt.Errorf("解析 response_format.json_schema: %w", err)
	}
	result := make(map[string]json.RawMessage, len(schema))
	result["type"] = mustJSON("json_schema")
	for key, value := range schema {
		if key != "type" {
			result[key] = value
		}
	}
	return json.Marshal(result)
}

func isEmptyJSON(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	return len(value) == 0 || bytes.Equal(value, []byte("null")) || bytes.Equal(value, []byte(`""`))
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
