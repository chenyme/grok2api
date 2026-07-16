package conversation

import (
	"sort"
	"strings"
)

func (c *streamConverter) startMessages() error {
	return c.writeEvent("message_start", map[string]any{
		"type": "message_start", "message": map[string]any{
			"id": anthropicMessageID(c.id), "type": "message", "role": "assistant",
			"model": c.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": anthropicUsage(c.usage, 0),
		},
	})
}

func (c *streamConverter) textDeltaMessages(delta string) error {
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

func (c *streamConverter) toolStartMessages(item responseItem) error {
	if err := c.start(); err != nil {
		return err
	}
	tool := streamTool{Index: c.nextIndex, ID: anthropicToolUseID(item.CallID), Name: item.Name, Arguments: item.Arguments}
	c.nextIndex++
	c.tools[item.ID] = tool
	return c.writeEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": tool.Index,
		"content_block": map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]any{}},
	})
}

func (c *streamConverter) toolDeltaMessages(itemID, delta string) error {
	tool, ok := c.tools[itemID]
	if !ok {
		return nil
	}
	tool.SentArgs = true
	c.tools[itemID] = tool
	return c.writeEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": tool.Index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": delta},
	})
}

func (c *streamConverter) toolArgumentsDoneMessages(itemID, arguments string) error {
	tool, ok := c.tools[itemID]
	if !ok || tool.Closed {
		return nil
	}
	if !tool.SentArgs {
		if arguments == "" {
			arguments = tool.Arguments
		}
		if arguments != "" {
			if err := c.writeEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": tool.Index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
			}); err != nil {
				return err
			}
			tool.SentArgs = true
		}
	}
	tool.Closed = true
	c.tools[itemID] = tool
	return c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": tool.Index})
}

func (c *streamConverter) doneMessages(status string) error {
	if c.thinkingStarted && !c.thinkingClosed {
		c.thinkingClosed = true
		if err := c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.thinkingIndex}); err != nil {
			return err
		}
	}
	if c.options.AnthropicWebSearchRequired && len(c.webSearch) == 0 {
		c.webSearch = []webSearchCall{unavailableWebSearchCall(c.options.AnthropicWebSearchQuery)}
	}
	if c.deferSearchText {
		// Hosted search was observed before text, so preserve Anthropic's block order:
		// server_tool_use → web_search_tool_result → text.
		if err := c.emitPendingWebSearchResults(); err != nil {
			return err
		}
		if pending := c.pendingSearchText.String(); pending != "" {
			if err := c.textDeltaMessages(pending); err != nil {
				return err
			}
		}
	}
	if c.stopSequence == "" {
		if pending := c.stopFilter.Flush(); pending != "" {
			if err := c.textDeltaWithoutFilter(pending); err != nil {
				return err
			}
		}
	}
	if c.textStarted {
		if err := c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.textIndex}); err != nil {
			return err
		}
	}
	if !c.deferSearchText {
		// If text arrived before the search item, close it before starting any server
		// tool blocks so content blocks never overlap.
		if err := c.emitPendingWebSearchResults(); err != nil {
			return err
		}
	}
	openTools := make([]streamTool, 0)
	for itemID, tool := range c.tools {
		if !tool.Closed {
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
	// Only client function tools force tool_use stop. Hosted web_search is end_turn.
	if len(c.tools) > 0 {
		stopReason = "tool_use"
	} else if c.stopSequence != "" {
		stopReason = "stop_sequence"
	} else if c.refused {
		stopReason = "refusal"
	} else if status == "incomplete" {
		stopReason = "max_tokens"
	}
	usage := anthropicUsage(c.usage, webSearchRequestCount(c.webSearch))
	if err := c.writeEvent("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nullableAnthropicString(c.stopSequence)},
		"usage": usage,
	}); err != nil {
		return err
	}
	c.finished = true
	return c.writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

func (c *streamConverter) streamErrorMessages(data []byte) error {
	return c.writeEvent("error", map[string]any{"type": "error", "error": normalizeAnthropicError(streamErrorValue(data))})
}
