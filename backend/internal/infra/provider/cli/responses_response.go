package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	maxCompatibleResponseBytes = 128 << 20
	maxCompatibleSSEEventBytes = 8 << 20
)

// normalizeResponseJSON 将上游普通函数别名恢复为下游 namespace 和 Tool Search 输出项。
func (c *responsesToolCompatibility) normalizeResponseJSON(body []byte) ([]byte, error) {
	if c == nil {
		return body, nil
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("解析 Grok Build Responses 响应: %w", err)
	}
	if err := c.rewriteResponseValue(response); err != nil {
		return nil, err
	}
	c.restoreVisibleTools(response)
	converted, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("编码兼容 Responses 响应: %w", err)
	}
	return converted, nil
}

// normalizeResponseStream 逐事件恢复 SSE 中的 namespace，并隐藏内部 Tool Search 函数参数事件。
func (c *responsesToolCompatibility) normalizeResponseStream(source io.ReadCloser) io.ReadCloser {
	if c == nil {
		return source
	}
	reader, writer := io.Pipe()
	go func() {
		defer source.Close()
		err := consumeCompatibleSSE(source, func(event compatibleSSEEvent) error {
			if !event.HasData() {
				return event.writeTo(writer)
			}
			outputs, rewriteErr := c.rewriteStreamData(event.Event, event.Data())
			if rewriteErr != nil {
				return rewriteErr
			}
			for index, output := range outputs {
				current := event
				if output.Event != "" {
					current.Event = output.Event
				}
				if index > 0 {
					current.ID = ""
					current.Retry = ""
					current.Comments = nil
					current.Other = nil
				}
				current.SetData(output.Data)
				if err := current.writeTo(writer); err != nil {
					return err
				}
			}
			return nil
		})
		_ = writer.CloseWithError(err)
	}()
	return reader
}

type responsesStreamOutput struct {
	Event string
	Data  []byte
}

type responsesStreamCall struct {
	identity     responsesToolIdentity
	arguments    strings.Builder
	lastDelta    map[string]any
	addedPayload map[string]any
}

func (c *responsesToolCompatibility) rewriteStreamData(event string, data []byte) ([]responsesStreamOutput, error) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return []responsesStreamOutput{{Data: data}}, nil
	}
	if !isCompatibleResponsesEvent(event) {
		return nil, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		if event == "" {
			return nil, nil
		}
		return []responsesStreamOutput{{Data: data}}, nil
	}
	kind := stringField(payload, "type")
	if kind == "" {
		kind = event
	}
	if !isCompatibleResponsesEvent(kind) {
		return nil, nil
	}
	if kind == "response.output_item.added" {
		if item, ok := payload["item"].(map[string]any); ok {
			state := c.rememberStreamCall(item)
			if state != nil && state.identity.Kind == responsesApplyPatchTool {
				state.addedPayload = cloneJSONObject(payload)
				return nil, nil
			}
		}
	}
	if kind == "response.function_call_arguments.delta" {
		identity, state, found := c.streamIdentity(payload)
		if found && (identity.Kind == responsesToolSearch || identity.Kind == responsesCustomTool || identity.Kind == responsesApplyPatchTool) {
			state.arguments.WriteString(stringField(payload, "delta"))
			if identity.Kind == responsesCustomTool {
				state.lastDelta = cloneJSONObject(payload)
			}
			return nil, nil
		}
	}
	if kind == "response.function_call_arguments.done" {
		identity, state, found := c.streamIdentity(payload)
		if found && (identity.Kind == responsesToolSearch || identity.Kind == responsesApplyPatchTool) {
			// Tool Search 的 arguments 是结构化对象；等 output_item.done 带齐参数后再对下游可见。
			return nil, nil
		}
		if found && identity.Kind == responsesCustomTool {
			arguments := stringField(payload, "arguments")
			if arguments == "" {
				arguments = state.arguments.String()
			}
			input := decodeCustomToolInput(arguments)
			outputs := make([]responsesStreamOutput, 0, 2)
			if state.lastDelta != nil {
				delta := customToolStreamPayload(state.lastDelta, "response.custom_tool_call_input.delta", "delta", input)
				encoded, err := json.Marshal(delta)
				if err != nil {
					return nil, fmt.Errorf("编码 custom tool delta: %w", err)
				}
				outputs = append(outputs, responsesStreamOutput{Event: "response.custom_tool_call_input.delta", Data: encoded})
			}
			done := customToolStreamPayload(payload, "response.custom_tool_call_input.done", "input", input)
			encoded, err := json.Marshal(done)
			if err != nil {
				return nil, fmt.Errorf("编码 custom tool done: %w", err)
			}
			outputs = append(outputs, responsesStreamOutput{Event: "response.custom_tool_call_input.done", Data: encoded})
			return outputs, nil
		}
	}
	if kind == "response.output_item.done" {
		if item, ok := payload["item"].(map[string]any); ok {
			identity, exists := c.aliases[stringField(item, "name")]
			if exists && identity.Kind == responsesApplyPatchTool {
				return c.rewriteApplyPatchDoneEvent(payload, item)
			}
		}
	}
	if err := c.rewriteResponseValue(payload); err != nil {
		return nil, err
	}
	if response, ok := payload["response"].(map[string]any); ok {
		c.restoreVisibleTools(response)
	}
	converted, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("编码兼容 Responses SSE: %w", err)
	}
	return []responsesStreamOutput{{Data: converted}}, nil
}

func (c *responsesToolCompatibility) rememberStreamCall(item map[string]any) *responsesStreamCall {
	if stringField(item, "type") != "function_call" {
		return nil
	}
	identity, exists := c.aliases[stringField(item, "name")]
	if !exists {
		return nil
	}
	state := &responsesStreamCall{identity: identity}
	for _, key := range []string{stringField(item, "id"), stringField(item, "call_id")} {
		if key != "" {
			c.streamCalls[key] = state
		}
	}
	return state
}

func (c *responsesToolCompatibility) streamIdentity(payload map[string]any) (responsesToolIdentity, *responsesStreamCall, bool) {
	for _, key := range []string{stringField(payload, "item_id"), stringField(payload, "call_id")} {
		if state, exists := c.streamCalls[key]; exists {
			return state.identity, state, true
		}
	}
	identity, exists := c.aliases[stringField(payload, "name")]
	if !exists {
		return responsesToolIdentity{}, nil, false
	}
	state := &responsesStreamCall{identity: identity}
	for _, key := range []string{stringField(payload, "item_id"), stringField(payload, "call_id")} {
		if key != "" {
			c.streamCalls[key] = state
		}
	}
	return identity, state, true
}

func (c *responsesToolCompatibility) rewriteResponseValue(value any) error {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if err := c.rewriteResponseValue(item); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range typed {
			if err := c.rewriteResponseValue(item); err != nil {
				return err
			}
		}
		switch stringField(typed, "type") {
		case "function_call":
			if err := c.rewriteFunctionCall(typed); err != nil {
				return err
			}
		case "shell_call":
			if c.legacyLocalShell {
				rewriteLegacyLocalShellCall(typed)
			}
		}
	}
	return nil
}

func (c *responsesToolCompatibility) rewriteFunctionCall(call map[string]any) error {
	identity, exists := c.aliases[stringField(call, "name")]
	if !exists {
		return nil
	}
	switch identity.Kind {
	case responsesFunctionTool:
		call["name"] = identity.Name
		if identity.Namespace != "" {
			call["namespace"] = identity.Namespace
		} else {
			delete(call, "namespace")
		}
	case responsesCustomTool:
		call["type"] = "custom_tool_call"
		call["name"] = identity.Name
		if identity.Namespace != "" {
			call["namespace"] = identity.Namespace
		} else {
			delete(call, "namespace")
		}
		call["input"] = decodeCustomToolInput(call["arguments"])
		delete(call, "arguments")
	case responsesToolSearch:
		call["type"] = "tool_search_call"
		call["execution"] = "client"
		call["arguments"] = decodeToolSearchArguments(call["arguments"])
		delete(call, "name")
		delete(call, "namespace")
	case responsesApplyPatchTool:
		operation, err := decodeApplyPatchArguments(call["arguments"], "response.output[].arguments")
		if err != nil {
			return fmt.Errorf("恢复 apply_patch_call: %w", err)
		}
		call["type"] = "apply_patch_call"
		call["operation"] = operation
		delete(call, "name")
		delete(call, "namespace")
		delete(call, "arguments")
	}
	return nil
}

func rewriteLegacyLocalShellCall(call map[string]any) {
	call["type"] = "local_shell_call"
	call["action"] = rewriteLegacyShellAction(call["action"])
	delete(call, "max_output_length")
}

func (c *responsesToolCompatibility) rewriteApplyPatchDoneEvent(payload, item map[string]any) ([]responsesStreamOutput, error) {
	done := cloneJSONObject(payload)
	if err := c.rewriteResponseValue(done); err != nil {
		return nil, err
	}
	doneItem, _ := done["item"].(map[string]any)
	var state *responsesStreamCall
	for _, key := range []string{stringField(item, "id"), stringField(item, "call_id")} {
		if candidate, exists := c.streamCalls[key]; exists {
			state = candidate
			break
		}
	}
	added := map[string]any{"type": "response.output_item.added"}
	if state != nil && state.addedPayload != nil {
		added = cloneJSONObject(state.addedPayload)
		added["type"] = "response.output_item.added"
	}
	for _, key := range []string{"output_index", "sequence_number"} {
		if value, exists := done[key]; exists && added[key] == nil {
			added[key] = cloneJSONValue(value)
		}
	}
	addedItem := cloneJSONObject(doneItem)
	addedItem["status"] = "in_progress"
	added["item"] = addedItem
	addedData, err := json.Marshal(added)
	if err != nil {
		return nil, fmt.Errorf("编码 apply_patch added event: %w", err)
	}
	doneData, err := json.Marshal(done)
	if err != nil {
		return nil, fmt.Errorf("编码 apply_patch done event: %w", err)
	}
	return []responsesStreamOutput{
		{Event: "response.output_item.added", Data: addedData},
		{Event: "response.output_item.done", Data: doneData},
	}, nil
}

func customToolStreamPayload(source map[string]any, kind, valueKey, value string) map[string]any {
	result := map[string]any{"type": kind, valueKey: value}
	for _, key := range []string{"item_id", "output_index", "sequence_number"} {
		if item, exists := source[key]; exists {
			result[key] = item
		}
	}
	return result
}

func isCompatibleResponsesEvent(kind string) bool {
	return kind == "" || kind == "error" || strings.HasPrefix(kind, "response.")
}

func decodeToolSearchArguments(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	if strings.TrimSpace(text) == "" {
		return map[string]any{}
	}
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) == nil {
		return decoded
	}
	return map[string]any{"input": text}
}

func (c *responsesToolCompatibility) restoreVisibleTools(response map[string]any) {
	if _, exists := response["tools"]; !exists {
		return
	}
	response["tools"] = cloneJSONArray(c.visibleTools)
}

type compatibleSSEEvent struct {
	Event    string
	ID       string
	Retry    string
	Comments []string
	Other    []string
	data     []string
}

func (e compatibleSSEEvent) Data() []byte {
	return []byte(strings.Join(e.data, "\n"))
}

func (e compatibleSSEEvent) HasData() bool { return len(e.data) > 0 }

func (e *compatibleSSEEvent) SetData(data []byte) {
	e.data = strings.Split(string(data), "\n")
}

func (e compatibleSSEEvent) writeTo(writer io.Writer) error {
	for _, comment := range e.Comments {
		if _, err := fmt.Fprintln(writer, comment); err != nil {
			return err
		}
	}
	if e.Event != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", e.Event); err != nil {
			return err
		}
	}
	if e.ID != "" {
		if _, err := fmt.Fprintf(writer, "id: %s\n", e.ID); err != nil {
			return err
		}
	}
	if e.Retry != "" {
		if _, err := fmt.Fprintf(writer, "retry: %s\n", e.Retry); err != nil {
			return err
		}
	}
	for _, field := range e.Other {
		if _, err := fmt.Fprintln(writer, field); err != nil {
			return err
		}
	}
	for _, line := range e.data {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(writer)
	return err
}

func consumeCompatibleSSE(source io.Reader, handle func(compatibleSSEEvent) error) error {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64<<10), maxCompatibleSSEEventBytes)
	event := compatibleSSEEvent{}
	eventBytes := 0
	flush := func() error {
		if len(event.data) == 0 && len(event.Comments) == 0 && len(event.Other) == 0 && event.Event == "" && event.ID == "" && event.Retry == "" {
			return nil
		}
		current := event
		event = compatibleSSEEvent{}
		eventBytes = 0
		return handle(current)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		eventBytes += len(line)
		if eventBytes > maxCompatibleSSEEventBytes {
			return fmt.Errorf("Grok Build Responses SSE 单事件超过 %d MiB", maxCompatibleSSEEventBytes>>20)
		}
		field, value, found := strings.Cut(line, ":")
		if found && strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch {
		case strings.HasPrefix(line, ":"):
			event.Comments = append(event.Comments, line)
		case !found:
			event.Other = append(event.Other, line)
		case field == "event":
			event.Event = value
		case field == "data":
			event.data = append(event.data, value)
		case field == "id":
			event.ID = value
		case field == "retry":
			event.Retry = value
		default:
			event.Other = append(event.Other, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
