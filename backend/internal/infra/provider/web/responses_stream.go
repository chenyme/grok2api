package web

import (
	"fmt"
	"io"
	"strings"
)

type responsesReasoningStreamItem struct {
	id    string
	index int
	text  strings.Builder
}

type responsesMessageStreamItem struct {
	id          string
	index       int
	text        strings.Builder
	annotations []map[string]any
}

// webResponsesStream owns the Responses output sequence. Every output item is
// assigned once, completed once, and then reused verbatim in response.completed.
type webResponsesStream struct {
	writer            io.Writer
	responseID        string
	nextOutputIndex   int
	outputs           []any
	reasoning         *responsesReasoningStreamItem
	message           *responsesMessageStreamItem
	messageCount      int
	streamedReasoning strings.Builder
}

func newWebResponsesStream(writer io.Writer, responseID string) *webResponsesStream {
	return &webResponsesStream{writer: writer, responseID: responseID}
}

func (s *webResponsesStream) allocateOutputIndex() int {
	index := s.nextOutputIndex
	s.nextOutputIndex++
	s.outputs = append(s.outputs, nil)
	return index
}

func (s *webResponsesStream) completeOutput(index int, item map[string]any) error {
	if index < 0 || index >= len(s.outputs) {
		return fmt.Errorf("Responses output_index %d 超出已分配范围", index)
	}
	if s.outputs[index] != nil {
		return fmt.Errorf("Responses output_index %d 被重复完成", index)
	}
	s.outputs[index] = item
	return nil
}

func (s *webResponsesStream) Delta(kind, delta string) error {
	if delta == "" {
		return nil
	}
	if kind == "reasoning" {
		return s.reasoningDelta(delta)
	}
	if err := s.closeReasoning(); err != nil {
		return err
	}
	if err := s.ensureMessage(); err != nil {
		return err
	}
	s.message.text.WriteString(delta)
	return writeSSE(s.writer, "response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "response_id": s.responseID,
		"item_id": s.message.id, "output_index": s.message.index, "content_index": 0,
		"delta": delta, "logprobs": []any{},
	})
}

func (s *webResponsesStream) reasoningDelta(delta string) error {
	if s.reasoning == nil {
		if err := s.closeMessage(nil); err != nil {
			return err
		}
		item := &responsesReasoningStreamItem{id: newWebID("rs"), index: s.allocateOutputIndex()}
		s.reasoning = item
		if err := writeSSE(s.writer, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "response_id": s.responseID, "output_index": item.index,
			"item": map[string]any{"id": item.id, "type": "reasoning", "status": "in_progress", "summary": []any{}},
		}); err != nil {
			return err
		}
		if err := writeSSE(s.writer, "response.reasoning_summary_part.added", map[string]any{
			"type": "response.reasoning_summary_part.added", "response_id": s.responseID,
			"item_id": item.id, "output_index": item.index, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		}); err != nil {
			return err
		}
	}
	s.reasoning.text.WriteString(delta)
	s.streamedReasoning.WriteString(delta)
	return writeSSE(s.writer, "response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "response_id": s.responseID,
		"item_id": s.reasoning.id, "output_index": s.reasoning.index, "summary_index": 0, "delta": delta,
	})
}

func (s *webResponsesStream) closeReasoning() error {
	if s == nil || s.reasoning == nil {
		return nil
	}
	item := s.reasoning
	text := item.text.String()
	if err := writeSSE(s.writer, "response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "response_id": s.responseID,
		"item_id": item.id, "output_index": item.index, "summary_index": 0, "text": text,
	}); err != nil {
		return err
	}
	part := map[string]any{"type": "summary_text", "text": text}
	if err := writeSSE(s.writer, "response.reasoning_summary_part.done", map[string]any{
		"type": "response.reasoning_summary_part.done", "response_id": s.responseID,
		"item_id": item.id, "output_index": item.index, "summary_index": 0, "part": part,
	}); err != nil {
		return err
	}
	completed := map[string]any{"id": item.id, "type": "reasoning", "status": "completed", "summary": []any{part}}
	if err := writeSSE(s.writer, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "response_id": s.responseID, "output_index": item.index, "item": completed,
	}); err != nil {
		return err
	}
	if err := s.completeOutput(item.index, completed); err != nil {
		return err
	}
	s.reasoning = nil
	return nil
}

func (s *webResponsesStream) ensureMessage() error {
	if s.message != nil {
		return nil
	}
	item := &responsesMessageStreamItem{id: newWebID("msg"), index: s.allocateOutputIndex()}
	s.message = item
	s.messageCount++
	if err := writeSSE(s.writer, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "response_id": s.responseID, "output_index": item.index,
		"item": map[string]any{"id": item.id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
	}); err != nil {
		return err
	}
	return writeSSE(s.writer, "response.content_part.added", map[string]any{
		"type": "response.content_part.added", "response_id": s.responseID,
		"item_id": item.id, "output_index": item.index, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}},
	})
}

func (s *webResponsesStream) Annotations(annotations []map[string]any, startIndex int) error {
	if len(annotations) == 0 {
		return nil
	}
	if err := s.closeReasoning(); err != nil {
		return err
	}
	if err := s.ensureMessage(); err != nil {
		return err
	}
	for i, annotation := range annotations {
		if err := writeSSE(s.writer, "response.output_text.annotation.added", map[string]any{
			"type": "response.output_text.annotation.added", "response_id": s.responseID,
			"item_id": s.message.id, "output_index": s.message.index, "content_index": 0,
			"annotation_index": startIndex + i, "annotation": responsesURLCitation(annotation),
		}); err != nil {
			return err
		}
	}
	s.message.annotations = append(s.message.annotations, annotations...)
	return nil
}

func (s *webResponsesStream) closeMessage(finalAnnotations []map[string]any) error {
	if s == nil || s.message == nil {
		return nil
	}
	item := s.message
	annotations := item.annotations
	if len(finalAnnotations) > 0 && s.messageCount == 1 {
		annotations = finalAnnotations
	}
	wireAnnotations := responsesAnnotations(annotations)
	if wireAnnotations == nil {
		wireAnnotations = []any{}
	}
	text := item.text.String()
	if err := writeSSE(s.writer, "response.output_text.done", map[string]any{
		"type": "response.output_text.done", "response_id": s.responseID,
		"item_id": item.id, "output_index": item.index, "content_index": 0, "text": text, "logprobs": []any{},
	}); err != nil {
		return err
	}
	part := map[string]any{"type": "output_text", "text": text, "annotations": wireAnnotations, "logprobs": []any{}}
	if err := writeSSE(s.writer, "response.content_part.done", map[string]any{
		"type": "response.content_part.done", "response_id": s.responseID,
		"item_id": item.id, "output_index": item.index, "content_index": 0, "part": part,
	}); err != nil {
		return err
	}
	completed := map[string]any{
		"id": item.id, "type": "message", "role": "assistant", "status": "completed", "content": []any{part},
	}
	if err := writeSSE(s.writer, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "response_id": s.responseID, "output_index": item.index, "item": completed,
	}); err != nil {
		return err
	}
	if err := s.completeOutput(item.index, completed); err != nil {
		return err
	}
	s.message = nil
	return nil
}

func (s *webResponsesStream) HostedSearch(call hostedSearchCall) error {
	if err := s.closeReasoning(); err != nil {
		return err
	}
	items := xaiHostedSearchOutputItems(parsedChat{HostedSearchCalls: []hostedSearchCall{call}})
	if len(items) == 0 {
		return nil
	}
	completed := items[0].(map[string]any)
	index := s.allocateOutputIndex()
	added := cloneAnyMap(completed)
	added["status"] = "in_progress"
	if err := writeSSE(s.writer, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "response_id": s.responseID, "output_index": index, "item": added,
	}); err != nil {
		return err
	}
	eventType := "response.web_search_call.completed"
	if call.Kind == "x_search" {
		eventType = "response.x_search_call.completed"
	}
	if err := writeSSE(s.writer, eventType, map[string]any{
		"type": eventType, "response_id": s.responseID, "output_index": index, "item_id": call.ID,
	}); err != nil {
		return err
	}
	if err := writeSSE(s.writer, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "response_id": s.responseID, "output_index": index, "item": completed,
	}); err != nil {
		return err
	}
	return s.completeOutput(index, completed)
}

func (s *webResponsesStream) ToolCalls(calls []parsedToolCall) error {
	if err := s.closeReasoning(); err != nil {
		return err
	}
	for _, call := range calls {
		index := s.allocateOutputIndex()
		itemID := newWebID("fc")
		added := map[string]any{"id": itemID, "type": "function_call", "status": "in_progress", "call_id": call.ID, "name": call.Name, "arguments": ""}
		if err := writeSSE(s.writer, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "response_id": s.responseID, "output_index": index, "item": added,
		}); err != nil {
			return err
		}
		if err := writeSSE(s.writer, "response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "response_id": s.responseID,
			"item_id": itemID, "output_index": index, "delta": call.Arguments,
		}); err != nil {
			return err
		}
		if err := writeSSE(s.writer, "response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "response_id": s.responseID,
			"item_id": itemID, "output_index": index, "arguments": call.Arguments,
		}); err != nil {
			return err
		}
		completed := map[string]any{"id": itemID, "type": "function_call", "status": "completed", "call_id": call.ID, "name": call.Name, "arguments": call.Arguments}
		if err := writeSSE(s.writer, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "response_id": s.responseID, "output_index": index, "item": completed,
		}); err != nil {
			return err
		}
		if err := s.completeOutput(index, completed); err != nil {
			return err
		}
	}
	return nil
}

func (s *webResponsesStream) Finish(parsed *parsedChat) error {
	if s == nil || parsed == nil {
		return nil
	}
	if err := s.closeReasoning(); err != nil {
		return err
	}
	if err := s.closeMessage(parsed.Annotations); err != nil {
		return err
	}
	allReasoning := parsed.Reasoning.String()
	streamedReasoning := s.streamedReasoning.String()
	lateReasoning := ""
	if strings.HasPrefix(allReasoning, streamedReasoning) {
		lateReasoning = allReasoning[len(streamedReasoning):]
	} else if allReasoning != streamedReasoning {
		lateReasoning = allReasoning
	}
	if lateReasoning != "" {
		if err := s.appendCompletedReasoning(lateReasoning); err != nil {
			return err
		}
	}
	if s.messageCount == 0 && len(parsed.ToolCalls) == 0 {
		if err := s.ensureMessage(); err != nil {
			return err
		}
		if err := s.closeMessage(parsed.Annotations); err != nil {
			return err
		}
	}
	for index, item := range s.outputs {
		if item == nil {
			return fmt.Errorf("Responses output_index %d 未完成", index)
		}
	}
	parsed.ResponseOutput = append([]any(nil), s.outputs...)
	return nil
}

func (s *webResponsesStream) appendCompletedReasoning(text string) error {
	index := s.allocateOutputIndex()
	id := newWebID("rs")
	added := map[string]any{"id": id, "type": "reasoning", "status": "in_progress", "summary": []any{}}
	if err := writeSSE(s.writer, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "response_id": s.responseID, "output_index": index, "item": added,
	}); err != nil {
		return err
	}
	part := map[string]any{"type": "summary_text", "text": text}
	if err := writeSSE(s.writer, "response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "response_id": s.responseID,
		"item_id": id, "output_index": index, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	}); err != nil {
		return err
	}
	if err := writeSSE(s.writer, "response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "response_id": s.responseID,
		"item_id": id, "output_index": index, "summary_index": 0, "text": text,
	}); err != nil {
		return err
	}
	if err := writeSSE(s.writer, "response.reasoning_summary_part.done", map[string]any{
		"type": "response.reasoning_summary_part.done", "response_id": s.responseID,
		"item_id": id, "output_index": index, "summary_index": 0, "part": part,
	}); err != nil {
		return err
	}
	completed := map[string]any{"id": id, "type": "reasoning", "status": "completed", "summary": []any{part}}
	if err := writeSSE(s.writer, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "response_id": s.responseID, "output_index": index, "item": completed,
	}); err != nil {
		return err
	}
	return s.completeOutput(index, completed)
}

func cloneAnyMap(value map[string]any) map[string]any {
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}
