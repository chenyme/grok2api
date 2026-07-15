package conversation

import (
	"encoding/json"
	"io"
	"strings"
)

func (c *streamConverter) startChat() error {
	return c.writeData(map[string]any{
		"id": strings.Replace(c.id, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk",
		"created": c.created, "model": c.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	})
}

func (c *streamConverter) textDeltaChat(delta string) error {
	return c.chatDelta(map[string]any{"content": delta})
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

func (c *streamConverter) toolStartChat(item responseItem, outputIndex int) error {
	if err := c.start(); err != nil {
		return err
	}
	tool := streamTool{Index: outputIndex, ID: item.CallID, Name: item.Name, Arguments: item.Arguments}
	c.tools[item.ID] = tool
	return c.chatDelta(map[string]any{"tool_calls": []any{map[string]any{
		"index": tool.Index, "id": tool.ID, "type": "function", "function": map[string]any{"name": tool.Name, "arguments": ""},
	}}})
}

func (c *streamConverter) toolDeltaChat(itemID, delta string) error {
	tool, ok := c.tools[itemID]
	if !ok {
		return nil
	}
	tool.SentArgs = true
	c.tools[itemID] = tool
	return c.chatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": tool.Index, "function": map[string]any{"arguments": delta}}}})
}

func (c *streamConverter) toolArgumentsDoneChat(itemID, arguments string) error {
	tool, ok := c.tools[itemID]
	if !ok || tool.Closed {
		return nil
	}
	if !tool.SentArgs {
		if arguments == "" {
			arguments = tool.Arguments
		}
		if arguments != "" {
			if err := c.chatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": tool.Index, "function": map[string]any{"arguments": arguments}}}}); err != nil {
				return err
			}
			tool.SentArgs = true
		}
	}
	c.tools[itemID] = tool
	return nil
}

func (c *streamConverter) doneChat(status string) error {
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

func (c *streamConverter) streamErrorChat(data []byte) error {
	if err := c.writeData(json.RawMessage(data)); err != nil {
		return err
	}
	_, err := io.WriteString(c.writer, "data: [DONE]\n\n")
	return err
}
