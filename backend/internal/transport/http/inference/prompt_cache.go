package inference

import (
	"encoding/json"
	"net/http"
	"strings"
)

const maxPromptCacheSeedBytes = 1024

// extractPromptCacheSeed 提取客户端会话标识；真正发往上游的 key 会在 Gateway 中隔离并哈希。
// 兼容 Claude Code / Codex / Sub2API（session_id、conversation_id、prompt_cache_key）。
func extractPromptCacheSeed(headers http.Header, body []byte) string {
	if headers != nil {
		// 优先级对齐 CPA + Sub2API 常用信号
		for _, name := range []string{
			"X-Claude-Code-Session-Id",
			"X-Session-ID", "X-Session-Id", "Session-Id", "Session_id", "session_id",
			"X-Conversation-Id", "Conversation-Id", "Conversation_id", "conversation_id",
			// Sub2API / 反代偶发透传
			"X-Client-Session-Id", "X-Grok-Conv-Id", "x-grok-conv-id",
		} {
			if seed := normalizePromptCacheSeed(headers.Get(name)); seed != "" {
				return seed
			}
		}
	}
	var payload struct {
		PromptCacheKey      string `json:"prompt_cache_key"`
		ConversationID      string `json:"conversation_id"`
		ConversationIDCamel string `json:"conversationId"`
		SessionID           string `json:"session_id"`
		SessionIDCamel      string `json:"sessionId"`
		Metadata            struct {
			SessionID      string `json:"session_id"`
			SessionIDCamel string `json:"sessionId"`
			UserID         string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	// body.prompt_cache_key 在 handler 里也会进 PromptCacheKey；这里再提取一次，
	// 保证仅依赖 seed 路径的中间件/日志也能看到。
	if seed := normalizePromptCacheSeed(payload.PromptCacheKey); seed != "" {
		return seed
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
	if seed := normalizePromptCacheSeed(payload.SessionID); seed != "" {
		return seed
	}
	if seed := normalizePromptCacheSeed(payload.SessionIDCamel); seed != "" {
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
