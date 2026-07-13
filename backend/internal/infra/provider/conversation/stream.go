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

// ConvertResponseStream 将 Responses SSE 转换为 Chat Completions 或 Anthropic Messages SSE。
func ConvertResponseStream(source io.ReadCloser, operation string) io.ReadCloser {
	if operation == OperationResponses {
		return source
	}
	reader, writer := io.Pipe()
	go func() {
		defer source.Close()
		converter := newStreamConverter(writer, operation)
		err := consumeSSE(source, converter.handle)
		if err == nil {
			err = converter.finish()
		}
		_ = writer.CloseWithError(err)
	}()
	return reader
}

type streamConverter struct {
	writer      io.Writer
	operation   string
	id          string
	model       string
	created     int64
	started     bool
	textStarted bool
	textIndex   int
	nextIndex   int
	tools       map[string]streamTool
	usage       responseUsage
}

type streamTool struct {
	Index int
	ID    string
	Name  string
}

func newStreamConverter(writer io.Writer, operation string) *streamConverter {
	return &streamConverter{writer: writer, operation: operation, created: time.Now().Unix(), tools: make(map[string]streamTool)}
}

func (c *streamConverter) handle(event string, data []byte) error {
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
		if c.operation != OperationChat {
			return nil
		}
		var delta string
		_ = json.Unmarshal(root["delta"], &delta)
		return c.chatDelta(map[string]any{"reasoning_content": delta})
	case "response.output_item.added":
		var item responseItem
		_ = json.Unmarshal(root["item"], &item)
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
	case "response.output_item.done":
		var item responseItem
		_ = json.Unmarshal(root["item"], &item)
		if item.Type == "function_call" {
			return c.toolDone(item.ID)
		}
	case "response.completed", "response.incomplete":
		var response responseEnvelope
		_ = json.Unmarshal(root["response"], &response)
		c.setResponse(response)
		return c.done(response.Status)
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
			"id": strings.Replace(c.id, "resp_", "msg_", 1), "type": "message", "role": "assistant",
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
	return c.writeEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": c.textIndex, "delta": map[string]any{"type": "text_delta", "text": delta}})
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
	tool := streamTool{Index: outputIndex, ID: item.CallID, Name: item.Name}
	if c.operation == OperationMessages {
		tool.Index = c.nextIndex
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
	if c.operation == OperationChat {
		return c.chatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": tool.Index, "function": map[string]any{"arguments": delta}}}})
	}
	return c.writeEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": tool.Index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": delta},
	})
}

func (c *streamConverter) toolDone(itemID string) error {
	tool, ok := c.tools[itemID]
	if !ok || c.operation != OperationMessages {
		return nil
	}
	return c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": tool.Index})
}

func (c *streamConverter) done(status string) error {
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
		_, err := io.WriteString(c.writer, "data: [DONE]\n\n")
		return err
	}
	if c.textStarted {
		if err := c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.textIndex}); err != nil {
			return err
		}
	}
	stopReason := "end_turn"
	if len(c.tools) > 0 {
		stopReason = "tool_use"
	} else if status == "incomplete" {
		stopReason = "max_tokens"
	}
	if err := c.writeEvent("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": c.usage.OutputTokens},
	}); err != nil {
		return err
	}
	return c.writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

func (c *streamConverter) streamError(data []byte) error {
	if c.operation == OperationMessages {
		return c.writeEvent("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": string(data)}})
	}
	if err := c.writeData(json.RawMessage(data)); err != nil {
		return err
	}
	_, err := io.WriteString(c.writer, "data: [DONE]\n\n")
	return err
}

func (c *streamConverter) finish() error { return nil }

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
