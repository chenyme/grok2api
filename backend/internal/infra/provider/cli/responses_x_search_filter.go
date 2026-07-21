package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// buildXSearchResponseFilter hides internal custom_tool_call items emitted while xAI executes native x_search.
// The upstream has already completed them, so Codex and Claude Code must not execute them again.
type buildXSearchResponseFilter struct {
	clientTools          map[string]struct{}
	injectedToolTypes    map[string]struct{}
	droppedOutputIndexes map[int]struct{}
	droppedItemIDs       map[string]struct{}
}

func newBuildXSearchResponseFilter(route buildPromptCacheRoute) *buildXSearchResponseFilter {
	return &buildXSearchResponseFilter{
		clientTools:          route.clientDeclaredTools,
		injectedToolTypes:    route.injectedToolTypes,
		droppedOutputIndexes: make(map[int]struct{}),
		droppedItemIDs:       make(map[string]struct{}),
	}
}

func filterBuildPromptCacheResponse(response *http.Response, streaming bool, route buildPromptCacheRoute) error {
	if response == nil || response.Body == nil || (!route.filterXSearch && len(route.injectedToolTypes) == 0) {
		return nil
	}
	filter := newBuildXSearchResponseFilter(route)
	if streaming {
		response.Body = filter.stream(response.Body)
		response.Header.Del("Content-Length")
		response.ContentLength = -1
		return nil
	}
	source := response.Body
	data, err := io.ReadAll(io.LimitReader(source, maxCompatibleResponseBytes+1))
	_ = source.Close()
	if err != nil {
		return err
	}
	if len(data) > maxCompatibleResponseBytes {
		return fmt.Errorf("Grok Build Responses 响应超过 %d MiB", maxCompatibleResponseBytes>>20)
	}
	filtered, err := filter.filterJSON(data)
	if err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(filtered))
	response.Header.Set("Content-Length", strconv.Itoa(len(filtered)))
	response.ContentLength = int64(len(filtered))
	return nil
}

func (f *buildXSearchResponseFilter) stream(source io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer source.Close()
		err := consumeCompatibleSSE(source, func(event compatibleSSEEvent) error {
			if !event.HasData() {
				return event.writeTo(writer)
			}
			data := event.Data()
			if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
				return event.writeTo(writer)
			}
			filtered, keep, filterErr := f.filterEvent(data)
			if filterErr != nil {
				return filterErr
			}
			if !keep {
				return nil
			}
			event.SetData(filtered)
			return event.writeTo(writer)
		})
		_ = writer.CloseWithError(err)
	}()
	return reader
}

func (f *buildXSearchResponseFilter) filterJSON(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 Grok Build Responses 响应: %w", err)
	}
	if err := f.filterEnvelope(payload); err != nil {
		return nil, err
	}
	filtered, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("编码 Grok Build Responses 响应: %w", err)
	}
	return filtered, nil
}

func (f *buildXSearchResponseFilter) filterEvent(data []byte) ([]byte, bool, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return data, true, nil
	}
	if item := payload["item"]; !isEmptyJSON(item) && f.isInternalCall(item) {
		f.recordDroppedItem(payload, item)
		return nil, false, nil
	}
	if err := f.filterEnvelope(payload); err != nil {
		return nil, false, err
	}
	if f.referencesDroppedItem(payload) {
		return nil, false, nil
	}
	f.compactOutputIndex(payload)
	filtered, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	return filtered, true, nil
}

func (f *buildXSearchResponseFilter) filterEnvelope(payload map[string]json.RawMessage) error {
	if err := f.filterOutput(payload); err != nil {
		return err
	}
	if err := f.filterTools(payload); err != nil {
		return err
	}
	if raw := payload["response"]; !isEmptyJSON(raw) {
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) == nil && response != nil {
			if err := f.filterOutput(response); err != nil {
				return err
			}
			if err := f.filterTools(response); err != nil {
				return err
			}
			payload["response"] = mustJSON(response)
		}
	}
	return nil
}

func (f *buildXSearchResponseFilter) filterOutput(envelope map[string]json.RawMessage) error {
	raw := envelope["output"]
	if isEmptyJSON(raw) {
		return nil
	}
	var output []json.RawMessage
	if json.Unmarshal(raw, &output) != nil {
		return fmt.Errorf("解析 Grok Build Responses output 失败")
	}
	filtered := make([]json.RawMessage, 0, len(output))
	for _, rawItem := range output {
		if f.isInternalCall(rawItem) {
			continue
		}
		filtered = append(filtered, rawItem)
	}
	envelope["output"] = mustJSON(filtered)
	return nil
}

func (f *buildXSearchResponseFilter) filterTools(envelope map[string]json.RawMessage) error {
	if len(f.injectedToolTypes) == 0 {
		return nil
	}
	raw := envelope["tools"]
	if isEmptyJSON(raw) {
		return nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return fmt.Errorf("解析 Grok Build Responses tools 失败")
	}
	filtered := make([]json.RawMessage, 0, len(tools))
	for _, rawTool := range tools {
		kind, _ := buildCacheToolIdentity(rawTool)
		if _, injected := f.injectedToolTypes[kind]; injected {
			continue
		}
		filtered = append(filtered, rawTool)
	}
	if len(filtered) == 0 {
		delete(envelope, "tools")
		return nil
	}
	envelope["tools"] = mustJSON(filtered)
	return nil
}

type buildXSearchResponseItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func (f *buildXSearchResponseFilter) isInternalCall(raw json.RawMessage) bool {
	var item buildXSearchResponseItem
	if json.Unmarshal(raw, &item) != nil {
		return false
	}
	kind := strings.TrimSpace(item.Type)
	if kind == "web_search_call" {
		_, injected := f.injectedToolTypes["web_search"]
		return injected
	}
	if kind != "custom_tool_call" && kind != "function_call" {
		return false
	}
	callID := strings.TrimSpace(item.CallID)
	if strings.HasPrefix(callID, "xs_call") {
		return true
	}
	name := strings.TrimSpace(item.Name)
	if !isBuildInternalXSearchToolName(name) || strings.TrimSpace(item.Namespace) != "" {
		return false
	}
	if _, declared := f.clientTools[name]; declared {
		return false
	}
	return true
}

func isBuildInternalXSearchToolName(name string) bool {
	switch strings.TrimSpace(name) {
	case "x_user_search", "x_semantic_search", "x_keyword_search", "x_thread_fetch":
		return true
	default:
		return false
	}
}

func (f *buildXSearchResponseFilter) recordDroppedItem(payload map[string]json.RawMessage, rawItem json.RawMessage) {
	if index, ok := rawJSONInt(payload["output_index"]); ok {
		f.droppedOutputIndexes[index] = struct{}{}
	}
	var item buildXSearchResponseItem
	if json.Unmarshal(rawItem, &item) != nil {
		return
	}
	for _, value := range []string{item.ID, item.CallID} {
		if value = strings.TrimSpace(value); value != "" {
			f.droppedItemIDs[value] = struct{}{}
		}
	}
}

func (f *buildXSearchResponseFilter) referencesDroppedItem(payload map[string]json.RawMessage) bool {
	if index, ok := rawJSONInt(payload["output_index"]); ok {
		if _, dropped := f.droppedOutputIndexes[index]; dropped {
			return true
		}
	}
	for _, name := range []string{"item_id", "call_id"} {
		if value := strings.TrimSpace(rawJSONString(payload[name])); value != "" {
			if _, dropped := f.droppedItemIDs[value]; dropped {
				return true
			}
		}
	}
	return false
}

func (f *buildXSearchResponseFilter) compactOutputIndex(payload map[string]json.RawMessage) {
	index, ok := rawJSONInt(payload["output_index"])
	if !ok {
		return
	}
	removedBefore := 0
	for dropped := range f.droppedOutputIndexes {
		if dropped < index {
			removedBefore++
		}
	}
	if removedBefore > 0 {
		payload["output_index"] = mustJSON(index - removedBefore)
	}
}

func rawJSONInt(raw json.RawMessage) (int, bool) {
	if isEmptyJSON(raw) {
		return 0, false
	}
	parsed, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
	return parsed, err == nil
}

func rawJSONString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
