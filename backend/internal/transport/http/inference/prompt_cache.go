package inference

import (
	"encoding/json"
	"net/http"
	"strings"
)

const maxPromptCacheSeedBytes = 1024

// extractPromptCacheSeed 提取客户端会话标识；真正发往上游的 key 会在 Gateway 中隔离并哈希。
func extractPromptCacheSeed(headers http.Header, body []byte) string {
	if seed := normalizePromptCacheSeed(headers.Get("X-Claude-Code-Session-Id")); seed != "" {
		return seed
	}
	for _, name := range []string{"X-Session-ID", "Session-Id", "Session_id"} {
		if seed := normalizePromptCacheSeed(headers.Get(name)); seed != "" {
			return seed
		}
	}
	var payload struct {
		Metadata struct {
			SessionID      string `json:"session_id"`
			SessionIDCamel string `json:"sessionId"`
			UserID         string `json:"user_id"`
		} `json:"metadata"`
		ConversationID      string `json:"conversation_id"`
		ConversationIDCamel string `json:"conversationId"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if seed := normalizePromptCacheSeed(payload.Metadata.SessionID); seed != "" {
		return seed
	}
	if seed := normalizePromptCacheSeed(payload.Metadata.SessionIDCamel); seed != "" {
		return seed
	}
	if seed := promptCacheSeedFromUserID(payload.Metadata.UserID); seed != "" {
		return seed
	}
	if seed := normalizePromptCacheSeed(payload.ConversationID); seed != "" {
		return seed
	}
	return normalizePromptCacheSeed(payload.ConversationIDCamel)
}

func promptCacheSeedFromUserID(userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	var embedded struct {
		SessionID      string `json:"session_id"`
		SessionIDCamel string `json:"sessionId"`
	}
	if json.Unmarshal([]byte(userID), &embedded) == nil {
		if seed := normalizePromptCacheSeed(embedded.SessionID); seed != "" {
			return seed
		}
		if seed := normalizePromptCacheSeed(embedded.SessionIDCamel); seed != "" {
			return seed
		}
	}
	const marker = "_session_"
	if index := strings.LastIndex(userID, marker); index >= 0 {
		return normalizePromptCacheSeed(userID[index+len(marker):])
	}
	return ""
}

func normalizePromptCacheSeed(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxPromptCacheSeedBytes {
		return ""
	}
	return value
}
