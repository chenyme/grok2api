package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

const qualityWebSearchResponseLimit = 4 << 20

var degradedWebSearchQueries = map[string]struct{}{
	"current information and recent sources":      {},
	"best expert recommendations and evidence":    {},
	"latest updates and authoritative references": {},
}

// inspectDegradedResponsesWebSearch buffers only bounded, non-streaming
// Responses API output. The original bytes are replayed for callers that do
// not match the narrow known-degraded signature.
func inspectDegradedResponsesWebSearch(request []byte, body io.ReadCloser) (io.ReadCloser, bool) {
	if body == nil || !qualityRequestUsesHostedWebSearch(request) {
		return body, false
	}
	raw, err := io.ReadAll(io.LimitReader(body, qualityWebSearchResponseLimit+1))
	replay := &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(raw), body), source: body}
	if err != nil || len(raw) > qualityWebSearchResponseLimit {
		return replay, false
	}
	return replay, responsesContainDegradedWebSearch(raw)
}

func qualityRequestUsesHostedWebSearch(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return false
	}
	if qualityToolListUsesWebSearch(payload["tools"]) {
		return true
	}
	items, _ := payload["input"].([]any)
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || jsonNodeString(item["type"]) != "additional_tools" {
			continue
		}
		if qualityToolListUsesWebSearch(item["tools"]) {
			return true
		}
	}
	return false
}

func qualityToolListUsesWebSearch(value any) bool {
	tools, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(jsonNodeString(tool["type"])))
		if kind == "web_search" || strings.HasPrefix(kind, "web_search_") {
			return true
		}
		if kind == "namespace" && qualityToolListUsesWebSearch(tool["tools"]) {
			return true
		}
	}
	return false
}

func responsesContainDegradedWebSearch(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return false
	}
	if responsesOutputContainsDegradedWebSearch(payload["output"]) {
		return true
	}
	response, _ := payload["response"].(map[string]any)
	return response != nil && responsesOutputContainsDegradedWebSearch(response["output"])
}

func responsesOutputContainsDegradedWebSearch(value any) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || jsonNodeString(item["type"]) != "web_search_call" {
			continue
		}
		action, _ := item["action"].(map[string]any)
		if action == nil || jsonNodeString(action["type"]) != "search" {
			continue
		}
		query := strings.ToLower(strings.Join(strings.Fields(jsonNodeString(action["query"])), " "))
		if _, found := degradedWebSearchQueries[query]; found {
			return true
		}
	}
	return false
}
