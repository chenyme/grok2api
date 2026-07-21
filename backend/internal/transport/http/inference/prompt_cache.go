package inference

import (
	"encoding/json"
	"net/http"
	"strings"
)

const (
	maxPromptCacheSeedBytes  = 1024
	maxCodexTurnMetadataSize = 16 << 10
)

// extractPromptCacheSeed extracts the client session identifier; the gateway isolates and hashes the key sent upstream.
// It supports Claude Code, Codex, and OpenAI-compatible session_id, conversation_id, and prompt_cache_key signals.
func extractPromptCacheSeed(headers http.Header, body []byte) string {
	// Claude Code sends auxiliary requests such as title generation concurrently and reuses the main session ID.
	// If an auxiliary request uses the explicit session seed, it shares reasoning replay with the main conversation.
	// Falling back to soft identity preserves account affinity without allowing encrypted reasoning to leak across requests.
	if isClaudeCodeTitleRequest(headers, body) {
		return ""
	}
	if headers != nil {
		// Prefer standard session signals from Claude Code, Codex, and OpenAI-compatible clients.
		if seed := normalizePromptCacheSeed(headers.Get("X-Claude-Code-Session-Id")); seed != "" {
			return claudeCodePromptCacheSeed(seed, headers)
		}
		if seed := codexPromptCacheSeedFromHeaders(headers); seed != "" {
			return seed
		}
		for _, name := range []string{
			"X-Session-Id", "Session-Id", "Session_id",
			"X-Conversation-Id", "Conversation-Id", "Conversation_id",
			// Support session signals forwarded by reverse proxies.
			"X-Client-Session-Id", "X-Grok-Conv-Id",
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
		ClientMetadata map[string]json.RawMessage `json:"client_metadata"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	// The handler also writes body.prompt_cache_key to PromptCacheKey. Extract it here as well so middleware
	// and logs that depend only on the seed path can observe it.
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
		return claudeCodePromptCacheSeed(seed, headers)
	}
	if seed := codexPromptCacheSeedFromRawTurnMetadata(payload.ClientMetadata["x-codex-turn-metadata"]); seed != "" {
		return seed
	}
	if seed := normalizeRawPromptCacheSeed(payload.ClientMetadata["x-codex-window-id"]); seed != "" {
		return "codex:window:" + seed
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

func claudeCodePromptCacheSeed(sessionID string, headers http.Header) string {
	sessionID = normalizePromptCacheSeed(sessionID)
	if sessionID == "" {
		return ""
	}
	agentID := "main"
	if headers != nil {
		if value := normalizePromptCacheSeed(headers.Get("X-Claude-Code-Agent-Id")); value != "" {
			agentID = value
		}
	}
	return "claude:" + sessionID + ":agent:" + agentID
}

func codexPromptCacheSeedFromHeaders(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if seed := codexPromptCacheSeedFromTurnMetadata(headers.Get("X-Codex-Turn-Metadata")); seed != "" {
		return seed
	}
	if seed := normalizePromptCacheSeed(headers.Get("X-Codex-Window-Id")); seed != "" {
		return "codex:window:" + seed
	}
	return ""
}

func codexPromptCacheSeedFromTurnMetadata(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxCodexTurnMetadataSize {
		return ""
	}
	var metadata struct {
		PromptCacheKey string `json:"prompt_cache_key"`
		WindowID       string `json:"window_id"`
	}
	if json.Unmarshal([]byte(value), &metadata) != nil {
		return ""
	}
	if seed := normalizePromptCacheSeed(metadata.PromptCacheKey); seed != "" {
		// Keep the same seed as body.prompt_cache_key so a Codex session does not rotate its upstream cache identity
		// when the signal source changes.
		return seed
	}
	if seed := normalizePromptCacheSeed(metadata.WindowID); seed != "" {
		return "codex:window:" + seed
	}
	return ""
}

func codexPromptCacheSeedFromRawTurnMetadata(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return codexPromptCacheSeedFromTurnMetadata(value)
	}
	return codexPromptCacheSeedFromTurnMetadata(string(raw))
}

func normalizeRawPromptCacheSeed(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return normalizePromptCacheSeed(value)
}

func allowBuildClientToolCacheRoute(headers http.Header) bool {
	if headers == nil {
		return false
	}
	if normalizePromptCacheSeed(headers.Get("X-Claude-Code-Session-Id")) != "" {
		return true
	}
	if codexPromptCacheSeedFromHeaders(headers) != "" {
		return true
	}
	return strings.Contains(strings.ToLower(headers.Get("User-Agent")), "codex")
}

func isClaudeCodeTitleRequest(headers http.Header, body []byte) bool {
	if headers == nil || normalizePromptCacheSeed(headers.Get("X-Claude-Code-Session-Id")) == "" || len(body) == 0 {
		return false
	}
	var payload struct {
		System json.RawMessage `json:"system"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.System) == 0 {
		return false
	}
	var texts []string
	var text string
	if json.Unmarshal(payload.System, &text) == nil {
		texts = append(texts, text)
	} else {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(payload.System, &blocks) != nil {
			return false
		}
		for _, block := range blocks {
			if block.Type == "text" {
				texts = append(texts, block.Text)
			}
		}
	}
	for _, value := range texts {
		value = strings.ToLower(strings.TrimSpace(value))
		if strings.Contains(value, "generate a concise") && strings.Contains(value, "title") && strings.Contains(value, "coding session") {
			return true
		}
	}
	return false
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
