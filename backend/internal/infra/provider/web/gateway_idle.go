package web

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
)

type gatewayActivity struct {
	observed bool
	textSeen bool
	seen     map[[sha256.Size]byte]struct{}
}

func newGatewayActivity() *gatewayActivity {
	return &gatewayActivity{seen: make(map[[sha256.Size]byte]struct{})}
}

func (activity *gatewayActivity) observeFrame(data []byte) bool {
	var payload struct {
		Event map[string]any `json:"event"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return false
	}
	return activity.observe(payload.Event)
}

func (activity *gatewayActivity) observe(event map[string]any) bool {
	typeName, _ := event["type"].(string)
	generated := false
	search := false
	switch typeName {
	case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		delta, _ := event["delta"].(string)
		generated = strings.TrimSpace(delta) != ""
	case "response.output_text.done":
		text, _ := event["text"].(string)
		generated = !activity.textSeen && strings.TrimSpace(text) != ""
	case "response.chunk":
		chunk, _ := event["chunk"].(map[string]any)
		text, _ := chunk["text"].(map[string]any)
		delta, _ := text["text"].(string)
		generated = strings.TrimSpace(delta) != ""
		for _, field := range []string{"tool_usage_card", "tool_result", "render_citation"} {
			value, _ := chunk[field].(map[string]any)
			if activity.observeTool(field, value) {
				search = true
			}
		}
	case "response.search.result":
		result, _ := event["result"].(map[string]any)
		search = activity.observeTool("search_result", result)
	}
	if generated {
		activity.textSeen = true
	}
	if generated || search {
		activity.observed = true
		return true
	}
	return false
}

func (activity *gatewayActivity) observeTool(kind string, value map[string]any) bool {
	if value == nil || len(activity.seen) >= maxTrackedServerTools {
		return false
	}
	content := make(map[string]any)
	for _, field := range []string{
		"tool_usage_card_id", "tool_call_id", "id", "web_search", "x_search", "x_post", "url", "citation_id",
	} {
		if item, exists := value[field]; exists {
			content[field] = item
		}
	}
	if len(content) == 0 {
		return false
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(append([]byte(kind+":"), encoded...))
	if _, exists := activity.seen[digest]; exists {
		return false
	}
	activity.seen[digest] = struct{}{}
	return true
}
