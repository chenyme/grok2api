package conversation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const maxDeferredSearchTextBytes = 8 << 20

// ConvertResponseStream 将 Responses SSE 转换为 Chat Completions 或 Anthropic Messages SSE。
func ConvertResponseStream(source io.ReadCloser, operation string) io.ReadCloser {
	return ConvertResponseStreamWithOptions(source, operation, ResponseOptions{})
}

// ConvertResponseStreamWithOptions 按下游协议选项生成 Chat 或 Anthropic SSE。
func ConvertResponseStreamWithOptions(source io.ReadCloser, operation string, options ResponseOptions) io.ReadCloser {
	if operation == OperationResponses {
		return source
	}
	reader, writer := io.Pipe()
	go func() {
		defer source.Close()
		converter := newStreamConverter(writer, operation, options)
		err := consumeSSE(source, converter.handle)
		if err == nil {
			err = converter.finish()
		}
		_ = writer.CloseWithError(err)
	}()
	return reader
}

type streamConverter struct {
	writer            io.Writer
	operation         string
	id                string
	model             string
	created           int64
	started           bool
	finished          bool
	textStarted       bool
	textIndex         int
	thinkingStarted   bool
	thinkingClosed    bool
	thinkingIndex     int
	thinkingItemID    string
	nextIndex         int
	tools             map[string]streamTool
	webSearch         []webSearchCall
	webSearchEmitted  map[string]bool
	deferSearchText   bool
	pendingSearchText strings.Builder
	usage             responseUsage
	options           ResponseOptions
	stopFilter        *anthropicStreamStopFilter
	stopSequence      string
	refused           bool
}

type streamTool struct {
	Index     int
	ID        string
	Name      string
	Arguments string
	SentArgs  bool
	Closed    bool
}

func newStreamConverter(writer io.Writer, operation string, options ResponseOptions) *streamConverter {
	return &streamConverter{
		writer: writer, operation: operation, created: time.Now().Unix(), tools: make(map[string]streamTool),
		webSearchEmitted: make(map[string]bool),
		deferSearchText:  operation == OperationMessages && options.AnthropicWebSearch,
		options:          options, stopFilter: newAnthropicStreamStopFilter(options.StopSequences),
	}
}

// noteWebSearch records a Build web_search_call. Emission is deferred to doneMessages
// so we always use the completed action.sources payload from the final envelope when available.
// For progressive UI we still emit server_tool_use as soon as we see the call.
func (c *streamConverter) noteWebSearch(call webSearchCall, final bool) error {
	filtered := dedupeWebSearchCalls([]webSearchCall{call})
	if len(filtered) == 0 {
		return nil
	}
	call = filtered[0]
	replaced := false
	for i, existing := range c.webSearch {
		if existing.ID == call.ID {
			// Prefer richer final payload.
			if final || len(call.Hits) >= len(existing.Hits) {
				c.webSearch[i] = call
			}
			replaced = true
			break
		}
	}
	if !replaced {
		if len(c.webSearch) >= maxWebSearchCalls {
			return nil
		}
		c.webSearch = append(c.webSearch, call)
	}
	if !c.textStarted {
		c.deferSearchText = true
	}
	if c.textStarted || (c.thinkingStarted && !c.thinkingClosed) {
		return nil
	}
	// Emit server_tool_use promptly so Claude Code can show "Searching: …".
	return c.emitWebSearchUse(call)
}

func (c *streamConverter) emitWebSearchUse(call webSearchCall) error {
	if err := c.start(); err != nil {
		return err
	}
	if c.webSearchEmitted[call.ID+"#use"] {
		return nil
	}
	index := c.nextIndex
	c.nextIndex++
	c.webSearchEmitted[call.ID+"#use"] = true
	if err := c.writeEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "server_tool_use", "id": call.ID, "name": "web_search", "input": map[string]any{}},
	}); err != nil {
		return err
	}
	if call.Query != "" {
		if err := c.writeEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": queryJSONPartial(call.Query)},
		}); err != nil {
			return err
		}
	}
	if err := c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
		return err
	}
	return nil
}

func (c *streamConverter) emitPendingWebSearchResults() error {
	c.webSearch = dedupeWebSearchCalls(c.webSearch)
	for _, call := range c.webSearch {
		if c.webSearchEmitted[call.ID+"#result"] {
			continue
		}
		if !c.webSearchEmitted[call.ID+"#use"] {
			if err := c.emitWebSearchUse(call); err != nil {
				return err
			}
		}
		if err := c.start(); err != nil {
			return err
		}
		index := c.nextIndex
		c.nextIndex++
		c.webSearchEmitted[call.ID+"#result"] = true
		var content any
		if call.Failed {
			code := call.Code
			if code == "" {
				code = "unavailable"
			}
			content = map[string]any{"type": "web_search_tool_result_error", "error_code": code}
		} else {
			hits := make([]any, 0, len(call.Hits))
			for _, hit := range call.Hits {
				hits = append(hits, map[string]any{"type": "web_search_result", "title": hit.Title, "url": hit.URL})
			}
			content = hits
		}
		if err := c.writeEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{
				"type": "web_search_tool_result", "tool_use_id": call.ID, "content": content,
			},
		}); err != nil {
			return err
		}
		if err := c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
			return err
		}
	}
	return nil
}

func (c *streamConverter) handle(event string, data []byte) error {
	if c.finished {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	typeName := event
	if raw := root["type"]; typeName == "" {
		_ = json.Unmarshal(raw, &typeName)
	}
	if c.stopSequence != "" && typeName != "response.completed" && typeName != "response.incomplete" && typeName != "response.failed" && typeName != "error" {
		return nil
	}
	switch typeName {
	case "response.created", "response.in_progress":
		var response responseEnvelope
		_ = json.Unmarshal(root["response"], &response)
		c.setResponse(response)
		return c.start()
	case "response.output_text.delta":
		var delta string
		_ = json.Unmarshal(root["delta"], &delta)
		if err := c.start(); err != nil {
			return err
		}
		if c.operation == OperationMessages && c.deferSearchText {
			return c.bufferSearchText(delta)
		}
		return c.textDelta(delta)
	case "response.refusal.delta":
		var delta string
		_ = json.Unmarshal(root["delta"], &delta)
		c.refused = true
		if c.operation == OperationChat {
			return c.chatDelta(map[string]any{"refusal": delta})
		}
		return c.textDeltaMessages(delta)
	case "response.output_text.annotation.added":
		if c.operation != OperationChat {
			return nil
		}
		var annotation any
		if json.Unmarshal(root["annotation"], &annotation) != nil || annotation == nil {
			return nil
		}
		return c.chatDelta(map[string]any{"annotations": []any{annotation}})
	case "response.reasoning_summary_text.delta":
		var delta string
		_ = json.Unmarshal(root["delta"], &delta)
		if c.operation == OperationChat {
			return c.chatDelta(map[string]any{"reasoning_content": delta})
		}
		return c.thinkingDelta(delta)
	case "response.reasoning_text.delta":
		var delta string
		_ = json.Unmarshal(root["delta"], &delta)
		if c.operation == OperationChat {
			return c.chatDelta(map[string]any{"reasoning_content": delta})
		}
		if c.operation == OperationMessages {
			return c.thinkingDelta(delta)
		}
		return nil
	case "response.output_item.added":
		var item responseItem
		_ = json.Unmarshal(root["item"], &item)
		if item.Type == "reasoning" && c.operation == OperationMessages && c.options.AnthropicThinking {
			return c.thinkingStart(item.ID)
		}
		if item.Type == "web_search_call" && c.operation == OperationMessages && c.options.AnthropicWebSearch {
			if call, ok := parseWebSearchCallItem(item); ok {
				return c.noteWebSearch(call, false)
			}
			return nil
		}
		if item.Type != "function_call" {
			return nil
		}
		var outputIndex int
		_ = json.Unmarshal(root["output_index"], &outputIndex)
		return c.toolStart(item, outputIndex)
	case "response.function_call_arguments.delta":
		var itemID, delta string
		_ = json.Unmarshal(root["item_id"], &itemID)
		_ = json.Unmarshal(root["delta"], &delta)
		return c.toolDelta(itemID, delta)
	case "response.function_call_arguments.done":
		var itemID, arguments string
		_ = json.Unmarshal(root["item_id"], &itemID)
		_ = json.Unmarshal(root["arguments"], &arguments)
		return c.toolArgumentsDone(itemID, arguments)
	case "response.output_item.done":
		var item responseItem
		_ = json.Unmarshal(root["item"], &item)
		if item.Type == "function_call" {
			return c.toolArgumentsDone(item.ID, item.Arguments)
		}
		if item.Type == "reasoning" {
			return c.thinkingDone(item)
		}
		if item.Type == "web_search_call" && c.operation == OperationMessages && c.options.AnthropicWebSearch {
			if call, ok := parseWebSearchCallItem(item); ok {
				return c.noteWebSearch(call, true)
			}
		}
	case "response.completed", "response.incomplete":
		var response responseEnvelope
		_ = json.Unmarshal(root["response"], &response)
		c.setResponse(response)
		if c.operation == OperationMessages && c.options.AnthropicWebSearch {
			parsed := parseResponse(response)
			for _, call := range parsed.WebSearch {
				if err := c.noteWebSearch(call, true); err != nil {
					return err
				}
			}
		}
		status := response.Status
		if status == "" && typeName == "response.incomplete" {
			status = "incomplete"
		}
		return c.done(status)
	case "error", "response.failed":
		return c.streamError(data)
	}
	return nil
}

func (c *streamConverter) bufferSearchText(delta string) error {
	pending := c.pendingSearchText.Len()
	if pending >= maxDeferredSearchTextBytes || len(delta) > maxDeferredSearchTextBytes-pending {
		return fmt.Errorf("WebSearch 延迟文本缓冲超过 %d MiB", maxDeferredSearchTextBytes>>20)
	}
	c.pendingSearchText.WriteString(delta)
	return nil
}

func (c *streamConverter) setResponse(value responseEnvelope) {
	if value.ID != "" {
		c.id = value.ID
	}
	if value.Model != "" {
		c.model = value.Model
	}
	if value.CreatedAt != 0 {
		c.created = value.CreatedAt
	}
	c.usage = mergeResponseUsage(c.usage, value.Usage)
}

func mergeResponseUsage(current, update responseUsage) responseUsage {
	if update.InputTokens != 0 {
		current.InputTokens = update.InputTokens
	}
	if update.OutputTokens != 0 {
		current.OutputTokens = update.OutputTokens
	}
	if update.TotalTokens != 0 {
		current.TotalTokens = update.TotalTokens
	}
	if update.CostInUSDTicks != 0 {
		current.CostInUSDTicks = update.CostInUSDTicks
	}
	if update.NumSourcesUsed != 0 {
		current.NumSourcesUsed = update.NumSourcesUsed
	}
	if update.NumServerSideToolsUsed != 0 {
		current.NumServerSideToolsUsed = update.NumServerSideToolsUsed
	}
	if update.InputTokensDetails.CachedTokens != 0 {
		current.InputTokensDetails.CachedTokens = update.InputTokensDetails.CachedTokens
	}
	if update.OutputTokensDetails.ReasoningTokens != 0 {
		current.OutputTokensDetails.ReasoningTokens = update.OutputTokensDetails.ReasoningTokens
	}
	if update.ContextDetails.InputTokens != 0 {
		current.ContextDetails.InputTokens = update.ContextDetails.InputTokens
	}
	if update.ContextDetails.OutputTokens != 0 {
		current.ContextDetails.OutputTokens = update.ContextDetails.OutputTokens
	}
	return current
}

func (c *streamConverter) start() error {
	if c.started {
		return nil
	}
	c.started = true
	if c.id == "" {
		c.id = "resp_" + fmt.Sprint(time.Now().UnixNano())
	}
	if c.operation == OperationChat {
		return c.startChat()
	}
	return c.startMessages()
}

func (c *streamConverter) textDelta(delta string) error {
	if c.operation == OperationChat {
		return c.textDeltaChat(delta)
	}
	return c.textDeltaMessages(delta)
}

func (c *streamConverter) toolStart(item responseItem, outputIndex int) error {
	if c.operation == OperationMessages {
		return c.toolStartMessages(item)
	}
	return c.toolStartChat(item, outputIndex)
}

func (c *streamConverter) toolDelta(itemID, delta string) error {
	if c.operation == OperationChat {
		return c.toolDeltaChat(itemID, delta)
	}
	return c.toolDeltaMessages(itemID, delta)
}

func (c *streamConverter) toolArgumentsDone(itemID, arguments string) error {
	if c.operation == OperationChat {
		return c.toolArgumentsDoneChat(itemID, arguments)
	}
	return c.toolArgumentsDoneMessages(itemID, arguments)
}

func (c *streamConverter) done(status string) error {
	if c.finished {
		return nil
	}
	if err := c.start(); err != nil {
		return err
	}
	if c.operation == OperationChat {
		return c.doneChat(status)
	}
	return c.doneMessages(status)
}

func (c *streamConverter) streamError(data []byte) error {
	c.finished = true
	if c.operation == OperationMessages {
		return c.streamErrorMessages(data)
	}
	return c.streamErrorChat(data)
}

func (c *streamConverter) finish() error {
	if c.finished {
		return nil
	}
	return c.done("")
}

func streamErrorValue(data []byte) any {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return strings.TrimSpace(string(data))
	}
	if response, ok := root["response"].(map[string]any); ok {
		if value, exists := response["error"]; exists && value != nil {
			return value
		}
	}
	if value, exists := root["error"]; exists && value != nil {
		return value
	}
	if message, ok := root["message"].(string); ok {
		return message
	}
	return strings.TrimSpace(string(data))
}

func (c *streamConverter) writeData(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.writer, "data: %s\n\n", data)
	return err
}

func (c *streamConverter) writeEvent(event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.writer, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func consumeSSE(source io.Reader, handle func(string, []byte) error) error {
	reader := bufio.NewReaderSize(source, 64<<10)
	var event string
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			case line == "":
				if data.Len() > 0 {
					if handleErr := handle(event, []byte(data.String())); handleErr != nil {
						return handleErr
					}
				}
				event = ""
				data.Reset()
			}
		}
		if err != nil {
			if err == io.EOF {
				if data.Len() > 0 {
					return handle(event, []byte(data.String()))
				}
				return nil
			}
			return err
		}
	}
}
