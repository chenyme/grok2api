package gateway

import (
	"encoding/json"
	"strings"
)

// Distinctive line from grok-build full_replace_summary_prompt.txt and the
// Grok TUI compaction request. Codex remote-v2 uses compaction_trigger
// instead; the TUI appends this prompt as a normal last user item.
const clientCompactionPromptMarker = "it is a system-generated compaction prompt, not a real user message"

type responsesCompactionKind uint8

const (
	responsesCompactionNone responsesCompactionKind = iota
	responsesCompactionTrigger
	responsesCompactionTUI
)

// isResponsesCompactionRequest detects a context-compaction turn without
// retaining the request body. Codex remote compaction v2 sends
// compaction_trigger; Grok TUI sends the canonical summary prompt as the
// last input/message item. The Provider adapter still requires the trigger
// before it rewrites the body into an encrypted compaction blob.
func isResponsesCompactionRequest(body []byte) bool {
	return classifyResponsesCompactionRequest(body) != responsesCompactionNone
}

func classifyResponsesCompactionRequest(body []byte) responsesCompactionKind {
	var payload struct {
		Input    []json.RawMessage `json:"input"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return responsesCompactionNone
	}
	if hasCompactionTrigger(payload.Input) {
		return responsesCompactionTrigger
	}
	if lastItemLooksLikeCompactionPrompt(payload.Input) || lastItemLooksLikeCompactionPrompt(payload.Messages) {
		return responsesCompactionTUI
	}
	return responsesCompactionNone
}

func hasCompactionTrigger(items []json.RawMessage) bool {
	for _, raw := range items {
		var item struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(item.Type), "compaction_trigger") {
			return true
		}
	}
	return false
}

func lastItemLooksLikeCompactionPrompt(items []json.RawMessage) bool {
	if len(items) == 0 {
		return false
	}
	var item struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(items[len(items)-1], &item) != nil || !strings.EqualFold(strings.TrimSpace(item.Role), "user") {
		return false
	}
	return looksLikeCompactionPrompt(extractContentText(item.Content))
}

func looksLikeCompactionPrompt(text string) bool {
	return strings.Contains(text, clientCompactionPromptMarker)
}

func extractContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var builder strings.Builder
		for _, part := range parts {
			builder.WriteString(part.Text)
		}
		return builder.String()
	}
	return ""
}
