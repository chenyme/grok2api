package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// buildPromptCacheRoute records internal tools added to route this request through the cache-capable path.
// injectedToolTypes restores the client's original visible tool list during response processing.
type buildPromptCacheRoute struct {
	filterXSearch       bool
	injectedToolTypes   map[string]struct{}
	clientDeclaredTools map[string]struct{}
}

func prepareBuildPromptCacheRoute(body []byte, operation, model, promptCacheKey string, allowClientTools bool) ([]byte, buildPromptCacheRoute, error) {
	route := buildPromptCacheRoute{
		injectedToolTypes:   make(map[string]struct{}),
		clientDeclaredTools: make(map[string]struct{}),
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, route, fmt.Errorf("解析 Build prompt cache 请求: %w", err)
	}
	if payload == nil {
		payload = make(map[string]json.RawMessage)
	}
	tools, err := buildCacheRouteTools(payload)
	if err != nil {
		return nil, route, err
	}
	for _, rawTool := range tools {
		kind, name := buildCacheToolIdentity(rawTool)
		if kind == "function" || kind == "custom" {
			if name != "" {
				route.clientDeclaredTools[name] = struct{}{}
			}
		}
		if kind == "x_search" {
			route.filterXSearch = true
		}
	}

	// Hide upstream internal subcalls even when the client explicitly declares x_search.
	// Cache routing itself applies only to plain-text conversations with a stable cache session identity.
	if strings.TrimSpace(promptCacheKey) == "" || !isBuildCacheConversationOperation(operation) || isBuildCacheMediaModel(model) || hasBuildCacheToolType(tools, "image_generation") {
		return body, route, nil
	}

	if len(tools) == 0 {
		// A tool-free request uses none to select the cache-capable route without granting search capability.
		tools = append(tools, json.RawMessage(`{"type":"web_search"}`), json.RawMessage(`{"type":"x_search"}`))
		payload["tool_choice"] = mustJSON("none")
		route.injectedToolTypes["web_search"] = struct{}{}
		route.injectedToolTypes["x_search"] = struct{}{}
		route.filterXSearch = true
	} else if !hasBuildCacheToolType(tools, "x_search") && (allowClientTools || hasBuildCacheToolType(tools, "web_search")) {
		// With client tools, add only the x_search route required by the official Build behavior and preserve
		// the original tool_choice. Do not also expose web_search.
		tools = append(tools, json.RawMessage(`{"type":"x_search"}`))
		route.injectedToolTypes["x_search"] = struct{}{}
		route.filterXSearch = true
	}
	payload["tools"] = mustJSON(tools)
	if _, injected := route.injectedToolTypes["x_search"]; injected {
		updatedChoice, choiceErr := appendBuildCacheXSearchToAllowedTools(payload["tool_choice"])
		if choiceErr != nil {
			return nil, route, choiceErr
		}
		if len(updatedChoice) > 0 {
			payload["tool_choice"] = updatedChoice
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, route, fmt.Errorf("编码 Build prompt cache 请求: %w", err)
	}
	return encoded, route, nil
}

func appendBuildCacheXSearchToAllowedTools(raw json.RawMessage) (json.RawMessage, error) {
	if isEmptyJSON(raw) {
		return nil, nil
	}
	var choice map[string]json.RawMessage
	if json.Unmarshal(raw, &choice) != nil || choice == nil {
		return nil, nil
	}
	var choiceType string
	_ = json.Unmarshal(choice["type"], &choiceType)
	if strings.TrimSpace(choiceType) != "allowed_tools" {
		return nil, nil
	}
	var allowed []json.RawMessage
	if json.Unmarshal(choice["tools"], &allowed) != nil {
		return nil, nil
	}
	for _, item := range allowed {
		kind, _ := buildCacheToolIdentity(item)
		if kind == "x_search" {
			return nil, nil
		}
	}
	allowed = append(allowed, json.RawMessage(`{"type":"x_search"}`))
	choice["tools"] = mustJSON(allowed)
	return json.Marshal(choice)
}

func buildCacheRouteTools(payload map[string]json.RawMessage) ([]json.RawMessage, error) {
	raw, exists := payload["tools"]
	if !exists || isEmptyJSON(raw) {
		return nil, nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil, &responsesRequestError{Message: "tools 必须是数组", Param: "tools", Code: "invalid_parameter"}
	}
	return tools, nil
}

func buildCacheToolIdentity(raw json.RawMessage) (kind, name string) {
	var tool struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tool) != nil {
		return "", ""
	}
	return strings.TrimSpace(tool.Type), strings.TrimSpace(tool.Name)
}

func hasBuildCacheToolType(tools []json.RawMessage, kind string) bool {
	for _, rawTool := range tools {
		toolType, _ := buildCacheToolIdentity(rawTool)
		if toolType == kind {
			return true
		}
	}
	return false
}

func isBuildCacheConversationOperation(operation string) bool {
	switch strings.TrimSpace(operation) {
	case "", "responses", "chat", "messages":
		return true
	default:
		return false
	}
}

func isBuildCacheMediaModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(model, "image") || strings.Contains(model, "imagine") || strings.Contains(model, "video")
}
