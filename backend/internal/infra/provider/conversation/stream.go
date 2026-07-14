package conversation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ConvertResponseStream 将 Responses SSE 转换为 Chat Completions 或 Anthropic Messages SSE。
func ConvertResponseStream(source io.ReadCloser, operation string) io.ReadCloser {
	return ConvertResponseStreamWithOptions(source, operation, ResponseOptions{})
}

// ConvertResponseStreamWithOptions 按原始 Messages 请求选项生成有序 Anthropic SSE。
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
	writer          io.Writer
	operation       string
	id              string
	model           string
	created         int64
	started         bool
	finished        bool
	textStarted     bool
	textIndex       int
	thinkingStarted bool
	thinkingClosed  bool
	thinkingIndex   int
	thinkingItemID  string
	nextIndex       int
	tools           map[string]streamTool
	usage           responseUsage
	options         ResponseOptions
	stopFilter      *anthropicStreamStopFilter
	stopSequence    string
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
		options: options, stopFilter: newAnthropicStreamStopFilter(options.StopSequences),
	}
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
		return c.textDelta(delta)
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
	case "response.completed", "response.incomplete":
		var response responseEnvelope
		_ = json.Unmarshal(root["response"], &response)
		c.setResponse(response)
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
	if value.Usage.InputTokens != 0 || value.Usage.OutputTokens != 0 {
		c.usage = value.Usage
	}
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
		return c.writeData(map[string]any{
			"id": strings.Replace(c.id, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk",
			"created": c.created, "model": c.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
		})
	}
	return c.writeEvent("message_start", map[string]any{
		"type": "message_start", "message": map[string]any{
			"id": anthropicMessageID(c.id), "type": "message", "role": "assistant",
			"model": c.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": anthropicUsage(c.usage),
		},
	})
}

func (c *streamConverter) textDelta(delta string) error {
	if c.operation == OperationChat {
		return c.chatDelta(map[string]any{"content": delta})
	}
	if !c.textStarted {
		c.textStarted = true
		c.textIndex = c.nextIndex
		c.nextIndex++
		if err := c.writeEvent("content_block_start", map[string]any{"type": "content_block_start", "index": c.textIndex, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
			return err
		}
	}
	emit, matched := c.stopFilter.Push(delta)
	if matched != "" {
		c.stopSequence = matched
	}
	if emit == "" {
		return nil
	}
	return c.writeEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": c.textIndex, "delta": map[string]any{"type": "text_delta", "text": emit}})
}

func (c *streamConverter) thinkingStart(itemID string) error {
	if !c.options.AnthropicThinking || c.thinkingStarted {
		return nil
	}
	if err := c.start(); err != nil {
		return err
	}
	c.thinkingStarted = true
	c.thinkingIndex = c.nextIndex
	c.nextIndex++
	c.thinkingItemID = itemID
	return c.writeEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": c.thinkingIndex,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
}

func (c *streamConverter) thinkingDelta(delta string) error {
	if c.operation != OperationMessages || !c.options.AnthropicThinking {
		return nil
	}
	if err := c.thinkingStart(""); err != nil {
		return err
	}
	return c.writeEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": c.thinkingIndex,
		"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
	})
}

func (c *streamConverter) thinkingDone(item responseItem) error {
	if c.operation != OperationMessages || !c.options.AnthropicThinking || !c.thinkingStarted || c.thinkingClosed {
		return nil
	}
	if item.Encrypted != "" {
		if err := c.writeEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": c.thinkingIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": item.Encrypted},
		}); err != nil {
			return err
		}
	}
	c.thinkingClosed = true
	return c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.thinkingIndex})
}

func (c *streamConverter) chatDelta(delta map[string]any) error {
	if err := c.start(); err != nil {
		return err
	}
	return c.writeData(map[string]any{
		"id": strings.Replace(c.id, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	})
}

func (c *streamConverter) toolStart(item responseItem, outputIndex int) error {
	if err := c.start(); err != nil {
		return err
	}
	tool := streamTool{Index: outputIndex, ID: item.CallID, Name: item.Name, Arguments: item.Arguments}
	if c.operation == OperationMessages {
		tool.Index = c.nextIndex
		tool.ID = anthropicToolUseID(tool.ID)
		c.nextIndex++
	}
	c.tools[item.ID] = tool
	if c.operation == OperationChat {
		return c.chatDelta(map[string]any{"tool_calls": []any{map[string]any{
			"index": tool.Index, "id": tool.ID, "type": "function", "function": map[string]any{"name": tool.Name, "arguments": ""},
		}}})
	}
	return c.writeEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": tool.Index,
		"content_block": map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]any{}},
	})
}

func (c *streamConverter) toolDelta(itemID, delta string) error {
	tool, ok := c.tools[itemID]
	if !ok {
		return nil
	}
	tool.SentArgs = true
	c.tools[itemID] = tool
	if c.operation == OperationChat {
		return c.chatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": tool.Index, "function": map[string]any{"arguments": delta}}}})
	}
	return c.writeEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": tool.Index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": delta},
	})
}

func (c *streamConverter) toolArgumentsDone(itemID, arguments string) error {
	tool, ok := c.tools[itemID]
	if !ok || tool.Closed {
		return nil
	}
	if !tool.SentArgs {
		if arguments == "" {
			arguments = tool.Arguments
		}
		if arguments != "" {
			if c.operation == OperationChat {
				if err := c.chatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": tool.Index, "function": map[string]any{"arguments": arguments}}}}); err != nil {
					return err
				}
			} else if err := c.writeEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": tool.Index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
			}); err != nil {
				return err
			}
			tool.SentArgs = true
		}
	}
	if c.operation != OperationMessages {
		c.tools[itemID] = tool
		return nil
	}
	tool.Closed = true
	c.tools[itemID] = tool
	return c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": tool.Index})
}

func (c *streamConverter) done(status string) error {
	if c.finished {
		return nil
	}
	if err := c.start(); err != nil {
		return err
	}
	if c.operation == OperationChat {
		finishReason := "stop"
		if len(c.tools) > 0 {
			finishReason = "tool_calls"
		} else if status == "incomplete" {
			finishReason = "length"
		}
		if err := c.writeData(map[string]any{
			"id": strings.Replace(c.id, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": c.created, "model": c.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}}, "usage": chatUsage(c.usage),
		}); err != nil {
			return err
		}
		c.finished = true
		_, err := io.WriteString(c.writer, "data: [DONE]\n\n")
		return err
	}
	if c.operation == OperationMessages && c.stopSequence == "" {
		if pending := c.stopFilter.Flush(); pending != "" {
			if err := c.textDeltaWithoutFilter(pending); err != nil {
				return err
			}
		}
	}
	if c.thinkingStarted && !c.thinkingClosed {
		c.thinkingClosed = true
		if err := c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.thinkingIndex}); err != nil {
			return err
		}
	}
	if c.textStarted {
		if err := c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.textIndex}); err != nil {
			return err
		}
	}
	openTools := make([]streamTool, 0)
	for itemID, tool := range c.tools {
		if c.operation == OperationMessages && !tool.Closed {
			tool.Closed = true
			c.tools[itemID] = tool
			openTools = append(openTools, tool)
		}
	}
	sort.Slice(openTools, func(i, j int) bool { return openTools[i].Index < openTools[j].Index })
	for _, tool := range openTools {
		if err := c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": tool.Index}); err != nil {
			return err
		}
	}
	stopReason := "end_turn"
	if len(c.tools) > 0 {
		stopReason = "tool_use"
	} else if c.stopSequence != "" {
		stopReason = "stop_sequence"
	} else if status == "incomplete" {
		stopReason = "max_tokens"
	}
	if err := c.writeEvent("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nullableAnthropicString(c.stopSequence)},
		"usage": map[string]any{"input_tokens": c.usage.InputTokens, "output_tokens": c.usage.OutputTokens},
	}); err != nil {
		return err
	}
	c.finished = true
	return c.writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

func (c *streamConverter) textDeltaWithoutFilter(delta string) error {
	if delta == "" {
		return nil
	}
	if !c.textStarted {
		c.textStarted = true
		c.textIndex = c.nextIndex
		c.nextIndex++
		if err := c.writeEvent("content_block_start", map[string]any{"type": "content_block_start", "index": c.textIndex, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
			return err
		}
	}
	return c.writeEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": c.textIndex, "delta": map[string]any{"type": "text_delta", "text": delta}})
}

func (c *streamConverter) streamError(data []byte) error {
	c.finished = true
	if c.operation == OperationMessages {
		return c.writeEvent("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": string(data)}})
	}
	if err := c.writeData(json.RawMessage(data)); err != nil {
		return err
	}
	_, err := io.WriteString(c.writer, "data: [DONE]\n\n")
	return err
}

func (c *streamConverter) finish() error {
	if c.finished {
		return nil
	}
	return c.done("")
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

type anthropicStreamStopFilter struct {
	sequences []string
	pending   string
	matched   string
}

func newAnthropicStreamStopFilter(sequences []string) *anthropicStreamStopFilter {
	filtered := make([]string, 0, len(sequences))
	for _, sequence := range sequences {
		if sequence != "" {
			filtered = append(filtered, sequence)
		}
	}
	return &anthropicStreamStopFilter{sequences: filtered}
}

func (f *anthropicStreamStopFilter) Push(delta string) (string, string) {
	if f == nil || len(f.sequences) == 0 {
		return delta, ""
	}
	if f.matched != "" {
		return "", f.matched
	}
	f.pending += delta
	matchAt := -1
	matched := ""
	for _, sequence := range f.sequences {
		if index := strings.Index(f.pending, sequence); index >= 0 && (matchAt < 0 || index < matchAt) {
			matchAt = index
			matched = sequence
		}
	}
	if matchAt >= 0 {
		emit := f.pending[:matchAt]
		f.pending = ""
		f.matched = matched
		return emit, matched
	}
	hold := 0
	for _, sequence := range f.sequences {
		maxPrefix := min(len(sequence)-1, len(f.pending))
		for size := maxPrefix; size > hold; size-- {
			if strings.HasSuffix(f.pending, sequence[:size]) {
				hold = size
				break
			}
		}
	}
	emitAt := len(f.pending) - hold
	emit := f.pending[:emitAt]
	f.pending = f.pending[emitAt:]
	return emit, ""
}

func (f *anthropicStreamStopFilter) Flush() string {
	if f == nil || f.matched != "" {
		return ""
	}
	value := f.pending
	f.pending = ""
	return value
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
