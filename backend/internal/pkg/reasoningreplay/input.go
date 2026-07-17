package reasoningreplay

import (
	"encoding/json"
	"strings"
)

func filterReplayItemsForInput(body []byte, items [][]byte) [][]byte {
	var payload struct {
		Input []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Input) == 0 {
		return nil
	}
	inputItems := make([]map[string]json.RawMessage, 0, len(payload.Input))
	for _, raw := range payload.Input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) == nil {
			inputItems = append(inputItems, item)
		}
	}
	lastAssistant, hasLastAssistant := lastAssistantMessage(inputItems)
	cachedAssistant, hasCachedAssistant := replayAssistantMessage(items)
	assistantMatches := hasLastAssistant && hasCachedAssistant && assistantContentEqual(lastAssistant, cachedAssistant)
	if hasLastAssistant && hasCachedAssistant && !assistantMatches {
		return nil
	}
	existingCalls := map[string]bool{}
	existingOutputs := map[string]string{}
	existingEncrypted := map[string]bool{}
	for _, item := range inputItems {
		var typeName string
		_ = json.Unmarshal(item["type"], &typeName)
		typeName = strings.TrimSpace(typeName)
		switch typeName {
		case "reasoning":
			var enc string
			_ = json.Unmarshal(item["encrypted_content"], &enc)
			if enc != "" {
				existingEncrypted[enc] = true
			}
		case "function_call_output", "custom_tool_call_output":
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			for _, candidate := range comparableReplayCallIDs(callID) {
				existingOutputs[candidate] = callID
			}
		case "function_call", "custom_tool_call":
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			for _, key := range replayToolCallKeys(typeName, callID) {
				existingCalls[key] = true
			}
		}
	}
	filtered := make([][]byte, 0, len(items))
	for _, item := range items {
		var typed struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
			CallID           string `json:"call_id"`
		}
		if json.Unmarshal(item, &typed) != nil {
			continue
		}
		switch strings.TrimSpace(typed.Type) {
		case "reasoning":
			if existingEncrypted[typed.EncryptedContent] {
				continue
			}
		case "message":
			if assistantMatches {
				continue
			}
		case "function_call", "custom_tool_call":
			keys := replayToolCallKeys(strings.TrimSpace(typed.Type), typed.CallID)
			if len(keys) == 0 || anyReplayCallKeyExists(existingCalls, keys) {
				continue
			}
			outputCallID := ""
			for _, candidate := range comparableReplayCallIDs(typed.CallID) {
				if value := existingOutputs[candidate]; value != "" {
					outputCallID = value
					break
				}
			}
			if outputCallID == "" {
				continue
			}
			for _, key := range keys {
				existingCalls[key] = true
			}
			if outputCallID != typed.CallID {
				item = rewriteReplayCallID(item, outputCallID)
			}
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func comparableReplayCallIDs(callID string) []string {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return nil
	}
	const anthropicPrefix = "toolu_"
	if strings.HasPrefix(callID, anthropicPrefix) {
		upstreamID := strings.TrimPrefix(callID, anthropicPrefix)
		if upstreamID != "" {
			return []string{callID, upstreamID}
		}
		return []string{callID}
	}
	return []string{callID, anthropicPrefix + callID}
}

func replayToolCallKeys(itemType, callID string) []string {
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return nil
	}
	ids := comparableReplayCallIDs(callID)
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, itemType+"\x00"+id)
	}
	return keys
}

func anyReplayCallKeyExists(existing map[string]bool, keys []string) bool {
	for _, key := range keys {
		if existing[key] {
			return true
		}
	}
	return false
}

func rewriteReplayCallID(item []byte, callID string) []byte {
	var raw map[string]json.RawMessage
	if json.Unmarshal(item, &raw) != nil {
		return item
	}
	encoded, err := json.Marshal(callID)
	if err != nil {
		return item
	}
	raw["call_id"] = encoded
	updated, err := json.Marshal(raw)
	if err != nil {
		return item
	}
	return updated
}

func lastAssistantMessage(items []map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		var typeName, role string
		_ = json.Unmarshal(item["type"], &typeName)
		_ = json.Unmarshal(item["role"], &role)
		typeName = strings.TrimSpace(typeName)
		if (typeName != "" && typeName != "message") || !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}
		return item, true
	}
	return nil, false
}

func replayAssistantMessage(items [][]byte) (map[string]json.RawMessage, bool) {
	for _, item := range items {
		var raw map[string]json.RawMessage
		if json.Unmarshal(item, &raw) != nil {
			continue
		}
		var typeName, role string
		_ = json.Unmarshal(raw["type"], &typeName)
		_ = json.Unmarshal(raw["role"], &role)
		if strings.TrimSpace(typeName) == "message" && strings.EqualFold(strings.TrimSpace(role), "assistant") {
			return raw, true
		}
	}
	return nil, false
}

func assistantContentEqual(left, right map[string]json.RawMessage) bool {
	leftParts, leftOK := assistantParts(left["content"])
	rightParts, rightOK := assistantParts(right["content"])
	if !leftOK || !rightOK || len(leftParts) != len(rightParts) {
		return false
	}
	for i := range leftParts {
		if leftParts[i] != rightParts[i] {
			return false
		}
	}
	return true
}

type assistantPart struct {
	partType string
	value    string
}

func assistantParts(raw json.RawMessage) ([]assistantPart, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return []assistantPart{{partType: "output_text", value: asString}}, true
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, false
	}
	result := make([]assistantPart, 0, len(parts))
	for _, part := range parts {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		switch strings.TrimSpace(partType) {
		case "output_text":
			var text string
			if json.Unmarshal(part["text"], &text) != nil {
				return nil, false
			}
			result = append(result, assistantPart{partType: partType, value: text})
		case "refusal":
			var refusal string
			if json.Unmarshal(part["refusal"], &refusal) != nil {
				return nil, false
			}
			result = append(result, assistantPart{partType: partType, value: refusal})
		default:
			return nil, false
		}
	}
	return result, len(result) > 0
}

func insertReplayItems(body []byte, replayItems [][]byte) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body, false
	}
	inputRaw, ok := payload["input"]
	if !ok {
		return body, false
	}
	var input []json.RawMessage
	if json.Unmarshal(inputRaw, &input) != nil {
		return body, false
	}
	insertAt := replayInsertIndex(input, replayItems)
	// 不基于不可信请求长度计算预分配容量，交由 append 安全扩容。
	next := make([]json.RawMessage, 0)
	for i, item := range input {
		if i == insertAt {
			for _, replay := range replayItems {
				next = append(next, json.RawMessage(replay))
			}
		}
		next = append(next, item)
	}
	if insertAt == len(input) {
		for _, replay := range replayItems {
			next = append(next, json.RawMessage(replay))
		}
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return body, false
	}
	payload["input"] = encoded
	updated, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return updated, true
}

func replayInsertIndex(input []json.RawMessage, replayItems [][]byte) int {
	replayCallIDs := map[string]bool{}
	for _, item := range replayItems {
		var typed struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(item, &typed) != nil {
			continue
		}
		if typed.Type == "function_call" || typed.Type == "custom_tool_call" {
			for _, id := range comparableReplayCallIDs(typed.CallID) {
				replayCallIDs[id] = true
			}
		}
	}
	if len(replayCallIDs) > 0 {
		for index, raw := range input {
			var typed struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(raw, &typed) != nil {
				continue
			}
			if typed.Type != "function_call_output" && typed.Type != "custom_tool_call_output" {
				continue
			}
			callIDs := comparableReplayCallIDs(typed.CallID)
			if len(callIDs) == 0 {
				return index
			}
			for _, callID := range callIDs {
				if replayCallIDs[callID] {
					return index
				}
			}
		}
	}
	for index := len(input) - 1; index >= 0; index-- {
		var typed struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(input[index], &typed) != nil {
			continue
		}
		typeName := strings.TrimSpace(typed.Type)
		if (typeName == "" || typeName == "message") && strings.EqualFold(strings.TrimSpace(typed.Role), "assistant") {
			return index
		}
	}
	for index, raw := range input {
		if shouldInsertReplayBefore(raw) {
			return index
		}
	}
	return len(input)
}

func shouldInsertReplayBefore(raw json.RawMessage) bool {
	var typed struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if json.Unmarshal(raw, &typed) != nil {
		return true
	}
	typeName := strings.TrimSpace(typed.Type)
	role := strings.ToLower(strings.TrimSpace(typed.Role))
	if role == "" || (typeName != "" && typeName != "message") {
		return true
	}
	return role != "system" && role != "developer"
}

// CaptureBody 包装上游响应，在流结束时抽取并写入回放缓存。
