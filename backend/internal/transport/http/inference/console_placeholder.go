package inference

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const consoleThinkingPlaceholder = "深度思考中"

// The placeholder is added only at the downstream transport boundary. This
// keeps quality checks and account-health decisions tied to real upstream
// reasoning, while giving clients a visible thinking section.
func addConsolePlaceholderJSON(data []byte, protocol streamProtocol) []byte {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return data
	}
	if root["error"] != nil || root["type"] == "error" || root["status"] == "failed" {
		return data
	}
	switch protocol {
	case streamProtocolChat:
		choices, _ := root["choices"].([]any)
		if len(choices) == 0 {
			return data
		}
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if message == nil || strings.TrimSpace(stringValue(message["reasoning_content"])) != "" {
			return data
		}
		message["reasoning_content"] = consoleThinkingPlaceholder
	case streamProtocolAnthropic:
		content, _ := root["content"].([]any)
		for _, value := range content {
			part, _ := value.(map[string]any)
			if part["type"] == "thinking" || part["type"] == "redacted_thinking" {
				return data
			}
		}
		root["content"] = append([]any{map[string]any{"type": "thinking", "thinking": consoleThinkingPlaceholder}}, content...)
	case streamProtocolResponses:
		output, _ := root["output"].([]any)
		for _, value := range output {
			item, _ := value.(map[string]any)
			if item["type"] == "reasoning" {
				if !consoleReasoningItemHasText(item) {
					item["summary"] = consolePlaceholderItem()["summary"]
				}
				encoded, err := json.Marshal(root)
				if err != nil {
					return data
				}
				return encoded
			}
		}
		root["output"] = append([]any{consolePlaceholderItem()}, output...)
	default:
		return data
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return data
	}
	return encoded
}

func consolePlaceholderItem() map[string]any {
	return map[string]any{"id": "rs_console_placeholder", "type": "reasoning", "status": "completed",
		"summary": []any{map[string]any{"type": "summary_text", "text": consoleThinkingPlaceholder}}}
}

func consoleReasoningItemHasText(item map[string]any) bool {
	for _, name := range []string{"summary", "content"} {
		parts, _ := item[name].([]any)
		for _, value := range parts {
			part, _ := value.(map[string]any)
			if strings.TrimSpace(stringValue(part["text"])) != "" {
				return true
			}
		}
	}
	return false
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

type consolePlaceholderStream struct {
	*io.PipeReader
}

func newConsolePlaceholderStream(source io.Reader, protocol streamProtocol) *consolePlaceholderStream {
	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(copyConsolePlaceholderStream(writer, source, protocol))
	}()
	return &consolePlaceholderStream{PipeReader: reader}
}

func copyConsolePlaceholderStream(writer io.Writer, source io.Reader, protocol streamProtocol) error {
	reader := bufio.NewReaderSize(source, 64<<10)
	inserted := false
	seenReasoning := false
	shiftIndices := false
	sequenceShift := 0
	insertionIndex := 0
	existingReasoningID := ""
	for {
		frame, err := readConsoleSSEFrame(reader)
		if len(frame) > 0 {
			event, root, valid := parseConsoleSSEFrame(frame)
			if valid {
				switch protocol {
				case streamProtocolChat:
					choice := firstConsoleChoice(root)
					delta, _ := choice["delta"].(map[string]any)
					if strings.TrimSpace(stringValue(delta["reasoning_content"])) != "" {
						seenReasoning = true
					}
					if !inserted && !seenReasoning && delta != nil && (strings.TrimSpace(stringValue(delta["content"])) != "" || delta["tool_calls"] != nil) {
						placeholder := cloneConsoleMap(root)
						placeholderChoice := firstConsoleChoice(placeholder)
						placeholderChoice["delta"] = map[string]any{"reasoning_content": consoleThinkingPlaceholder}
						if writeErr := writeConsoleSSEFrame(writer, "", placeholder); writeErr != nil {
							return writeErr
						}
						inserted = true
					}
				case streamProtocolAnthropic:
					if event == "content_block_start" {
						part, _ := root["content_block"].(map[string]any)
						if part["type"] == "thinking" || part["type"] == "redacted_thinking" {
							seenReasoning = true
						}
						if !inserted && !seenReasoning && part["type"] != nil {
							index := intValue(root["index"])
							if err := writeConsoleSSEFrame(writer, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "thinking", "thinking": ""}}); err != nil {
								return err
							}
							if err := writeConsoleSSEFrame(writer, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": consoleThinkingPlaceholder}}); err != nil {
								return err
							}
							if err := writeConsoleSSEFrame(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
								return err
							}
							inserted, shiftIndices = true, true
						}
					}
					if shiftIndices {
						if _, ok := root["index"]; ok {
							root["index"] = intValue(root["index"]) + 1
							frame = marshalConsoleSSEFrame(event, root)
						}
					}
				case streamProtocolResponses:
					if event == "response.output_item.added" || event == "response.output_item.done" {
						item, _ := root["item"].(map[string]any)
						if item["type"] == "reasoning" && consoleReasoningItemHasText(item) {
							seenReasoning = true
						}
						if event == "response.output_item.done" && item["type"] == "reasoning" && !inserted && !seenReasoning {
							insertionIndex = intValue(root["output_index"])
							existingReasoningID = stringValue(item["id"])
							delta := map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": existingReasoningID,
								"output_index": insertionIndex, "summary_index": 0, "delta": consoleThinkingPlaceholder}
							if _, exists := root["sequence_number"]; exists {
								delta["sequence_number"] = intValue(root["sequence_number"])
								sequenceShift = 1
							}
							if err := writeConsoleSSEFrame(writer, "response.reasoning_summary_text.delta", delta); err != nil {
								return err
							}
							item["summary"] = consolePlaceholderItem()["summary"]
							inserted = true
							frame = marshalConsoleSSEFrame(event, root)
						}
					}
					if event == "response.reasoning_text.delta" || event == "response.reasoning_summary_text.delta" {
						if strings.TrimSpace(stringValue(root["delta"])) != "" {
							seenReasoning = true
						}
					}
					item, _ := root["item"].(map[string]any)
					if !inserted && !seenReasoning && (event == "response.output_text.delta" || (event == "response.output_item.added" && item["type"] != "reasoning")) {
						insertionIndex = intValue(root["output_index"])
						sequence := -1
						if _, exists := root["sequence_number"]; exists {
							sequence = intValue(root["sequence_number"])
						}
						if err := writeConsoleResponsesPlaceholder(writer, sequence, insertionIndex); err != nil {
							return err
						}
						inserted, shiftIndices = true, true
						if sequence >= 0 {
							sequenceShift = 3
						}
					}
					if sequenceShift > 0 {
						if _, ok := root["sequence_number"]; ok {
							root["sequence_number"] = intValue(root["sequence_number"]) + sequenceShift
							frame = marshalConsoleSSEFrame(event, root)
						}
					}
					if shiftIndices {
						if _, ok := root["output_index"]; ok && intValue(root["output_index"]) >= insertionIndex {
							root["output_index"] = intValue(root["output_index"]) + 1
						}
						if event == "response.completed" || event == "response.incomplete" {
							response, _ := root["response"].(map[string]any)
							if response != nil {
								output, _ := response["output"].([]any)
								at := max(0, min(insertionIndex, len(output)))
								response["output"] = append(output[:at], append([]any{consolePlaceholderItem()}, output[at:]...)...)
							}
						}
						frame = marshalConsoleSSEFrame(event, root)
					} else if inserted && existingReasoningID != "" && (event == "response.completed" || event == "response.incomplete") {
						response, _ := root["response"].(map[string]any)
						output, _ := response["output"].([]any)
						for _, value := range output {
							item, _ := value.(map[string]any)
							if stringValue(item["id"]) == existingReasoningID {
								item["summary"] = consolePlaceholderItem()["summary"]
							}
						}
						frame = marshalConsoleSSEFrame(event, root)
					}
				}
			}
			if _, writeErr := writer.Write(frame); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func readConsoleSSEFrame(reader *bufio.Reader) ([]byte, error) {
	var frame bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		frame.Write(line)
		if frame.Len() > maxStreamEventInspectionBytes {
			return nil, fmt.Errorf("Console SSE 事件超过安全上限")
		}
		if len(bytes.TrimSpace(line)) == 0 || err != nil {
			return frame.Bytes(), err
		}
	}
}

func parseConsoleSSEFrame(frame []byte) (string, map[string]any, bool) {
	var event string
	var data []byte
	for _, line := range bytes.Split(frame, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("event:")) {
			event = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			data = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return event, nil, false
	}
	if event == "" {
		event = stringValue(root["type"])
	}
	return event, root, true
}

func firstConsoleChoice(root map[string]any) map[string]any {
	choices, _ := root["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	return choice
}

func cloneConsoleMap(root map[string]any) map[string]any {
	encoded, _ := json.Marshal(root)
	var cloned map[string]any
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func intValue(value any) int {
	number, _ := value.(float64)
	return int(number)
}

func marshalConsoleSSEFrame(event string, root map[string]any) []byte {
	data, _ := json.Marshal(root)
	if event == "" {
		return []byte(fmt.Sprintf("data: %s\n\n", data))
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
}

func writeConsoleSSEFrame(writer io.Writer, event string, root map[string]any) error {
	_, err := writer.Write(marshalConsoleSSEFrame(event, root))
	return err
}

func writeConsoleResponsesPlaceholder(writer io.Writer, sequence, outputIndex int) error {
	item := consolePlaceholderItem()
	added := map[string]any{"type": "response.output_item.added", "output_index": outputIndex,
		"item": map[string]any{"id": item["id"], "type": "reasoning", "status": "in_progress", "summary": []any{}}}
	delta := map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": item["id"], "output_index": outputIndex, "summary_index": 0, "delta": consoleThinkingPlaceholder}
	done := map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item}
	if sequence >= 0 {
		added["sequence_number"] = sequence
		delta["sequence_number"] = sequence + 1
		done["sequence_number"] = sequence + 2
	}
	if err := writeConsoleSSEFrame(writer, "response.output_item.added", added); err != nil {
		return err
	}
	if err := writeConsoleSSEFrame(writer, "response.reasoning_summary_text.delta", delta); err != nil {
		return err
	}
	return writeConsoleSSEFrame(writer, "response.output_item.done", done)
}
