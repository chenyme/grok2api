package reasoningreplay

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
)

func previousResponseIDPresent(body []byte) bool {
	var payload struct {
		PreviousResponseID string `json:"previous_response_id"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	return strings.TrimSpace(payload.PreviousResponseID) != ""
}

func extractReplayItemsFromPayload(payload []byte) ([][]byte, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(payload, &root) != nil {
		return nil, false
	}
	outputRaw, outputExists := root["output"]
	if !outputExists {
		if respRaw, ok := root["response"]; ok {
			var nested map[string]json.RawMessage
			if json.Unmarshal(respRaw, &nested) != nil {
				return nil, false
			}
			outputRaw, outputExists = nested["output"]
		}
	}
	if !outputExists || len(outputRaw) == 0 {
		return nil, false
	}
	var output []json.RawMessage
	if json.Unmarshal(outputRaw, &output) != nil {
		return nil, false
	}
	items := make([][]byte, 0, len(output))
	for _, item := range output {
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &typed) != nil {
			continue
		}
		switch strings.TrimSpace(typed.Type) {
		case "reasoning", "message", "function_call", "custom_tool_call":
			items = append(items, append([]byte(nil), item...))
		}
	}
	return items, true
}

func normalizeReplayItems(items [][]byte) ([][]byte, bool) {
	normalized := make([][]byte, 0, len(items))
	hasAnchor := false
	for _, item := range items {
		next, ok := normalizeReplayItem(item)
		if !ok {
			continue
		}
		normalized = append(normalized, next)
		var typed struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(next, &typed)
		switch strings.TrimSpace(typed.Type) {
		case "reasoning", "function_call", "custom_tool_call":
			hasAnchor = true
		}
	}
	return normalized, hasAnchor && len(normalized) > 0
}

func normalizeReplayItem(item []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(item, &raw) != nil {
		return nil, false
	}
	var typeName string
	_ = json.Unmarshal(raw["type"], &typeName)
	switch strings.TrimSpace(typeName) {
	case "reasoning":
		return normalizeReasoningItem(raw)
	case "message":
		return normalizeAssistantMessageItem(raw)
	case "function_call":
		return normalizeFunctionCallItem(raw)
	case "custom_tool_call":
		return normalizeCustomToolCallItem(raw)
	default:
		return nil, false
	}
}

func normalizeReasoningItem(raw map[string]json.RawMessage) ([]byte, bool) {
	var encrypted string
	if json.Unmarshal(raw["encrypted_content"], &encrypted) != nil {
		return nil, false
	}
	if !validGrokReplayEncryptedContent(encrypted) {
		return nil, false
	}
	out := map[string]any{
		"type":              "reasoning",
		"summary":           []any{},
		"content":           nil,
		"encrypted_content": encrypted,
	}
	data, err := json.Marshal(out)
	return data, err == nil
}

func validGrokReplayEncryptedContent(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxReplayEncryptedLen {
		return false
	}
	if strings.HasPrefix(value, "gAAAA") || strings.Contains(value, "=") {
		return false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(decoded) < minReplayEncryptedDecodedLen {
		return false
	}
	return replayByteEntropyRatio(decoded) >= minReplayEncryptedEntropy
}

func replayByteEntropyRatio(value []byte) float64 {
	if len(value) == 0 {
		return 0
	}
	var counts [256]int
	for _, item := range value {
		counts[item]++
	}
	size := float64(len(value))
	entropy := 0.0
	for _, count := range counts {
		if count == 0 {
			continue
		}
		probability := float64(count) / size
		entropy -= probability * math.Log2(probability)
	}
	maxSymbols := min(len(value), 256)
	if maxSymbols <= 1 {
		return 0
	}
	return entropy / math.Log2(float64(maxSymbols))
}

func normalizeAssistantMessageItem(raw map[string]json.RawMessage) ([]byte, bool) {
	var role string
	_ = json.Unmarshal(raw["role"], &role)
	if !strings.EqualFold(strings.TrimSpace(role), "assistant") {
		return nil, false
	}
	var content []map[string]json.RawMessage
	if json.Unmarshal(raw["content"], &content) != nil || len(content) == 0 {
		return nil, false
	}
	parts := make([]map[string]any, 0, len(content))
	for _, part := range content {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		switch strings.TrimSpace(partType) {
		case "output_text":
			var text string
			if json.Unmarshal(part["text"], &text) != nil {
				continue
			}
			parts = append(parts, map[string]any{"type": "output_text", "text": text})
		case "refusal":
			var refusal string
			if json.Unmarshal(part["refusal"], &refusal) != nil {
				continue
			}
			parts = append(parts, map[string]any{"type": "refusal", "refusal": refusal})
		}
	}
	if len(parts) == 0 {
		return nil, false
	}
	data, err := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": parts})
	return data, err == nil
}

func normalizeFunctionCallItem(raw map[string]json.RawMessage) ([]byte, bool) {
	var callID, name, arguments string
	_ = json.Unmarshal(raw["call_id"], &callID)
	_ = json.Unmarshal(raw["name"], &name)
	if json.Unmarshal(raw["arguments"], &arguments) != nil {
		return nil, false
	}
	callID, name = strings.TrimSpace(callID), strings.TrimSpace(name)
	if callID == "" || name == "" {
		return nil, false
	}
	data, err := json.Marshal(map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": arguments})
	return data, err == nil
}

func normalizeCustomToolCallItem(raw map[string]json.RawMessage) ([]byte, bool) {
	var callID, name string
	_ = json.Unmarshal(raw["call_id"], &callID)
	_ = json.Unmarshal(raw["name"], &name)
	callID, name = strings.TrimSpace(callID), strings.TrimSpace(name)
	if callID == "" || name == "" || len(raw["input"]) == 0 {
		return nil, false
	}
	out := map[string]any{"type": "custom_tool_call", "status": "completed", "call_id": callID, "name": name}
	var status string
	if json.Unmarshal(raw["status"], &status) == nil && strings.TrimSpace(status) != "" {
		out["status"] = status
	}
	var input any
	if json.Unmarshal(raw["input"], &input) != nil {
		return nil, false
	}
	out["input"] = input
	data, err := json.Marshal(out)
	return data, err == nil
}
