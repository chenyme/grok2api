package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func convertMessagesRequest(body []byte, model string) ([]byte, ResponseOptions, error) {
	var request anthropicRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, ResponseOptions{}, fmt.Errorf("解析 Messages 请求: %w", err)
	}
	if len(request.Messages) == 0 {
		return nil, ResponseOptions{}, errors.New("messages 必须是非空数组")
	}
	if request.MaxTokens <= 0 {
		return nil, ResponseOptions{}, errors.New("max_tokens 必须是正整数")
	}
	for name, value := range map[string]*float64{"temperature": request.Temperature, "top_p": request.TopP} {
		if value != nil && (*value < 0 || *value > 1) {
			return nil, ResponseOptions{}, fmt.Errorf("%s 必须在 0 到 1 之间", name)
		}
	}
	for index, sequence := range request.StopSequences {
		if sequence == "" {
			return nil, ResponseOptions{}, fmt.Errorf("stop_sequences[%d] 不能为空", index)
		}
	}
	thinkingEnabled := false
	if request.Thinking != nil {
		switch request.Thinking.Type {
		case "", "disabled":
		case "enabled", "adaptive":
			thinkingEnabled = true
		default:
			return nil, ResponseOptions{}, fmt.Errorf("不支持 thinking.type=%q", request.Thinking.Type)
		}
	}
	input, inlineInstructions, err := convertAnthropicMessages(request.Messages, anthropicDeclaredToolNames(request.Tools))
	if err != nil {
		return nil, ResponseOptions{}, err
	}
	if len(input) == 0 {
		return nil, ResponseOptions{}, errors.New("messages 中没有可发送的 user 或 assistant 内容")
	}
	target := map[string]any{
		"model": model, "input": input, "stream": request.Stream,
		"max_output_tokens": request.MaxTokens, "store": false,
	}
	instructions := make([]string, 0, len(inlineInstructions))
	if system, err := anthropicSystemText(request.System); err != nil {
		return nil, ResponseOptions{}, err
	} else if system != "" {
		instructions = append(instructions, system)
	}
	instructions = append(instructions, inlineInstructions...)
	if len(instructions) > 0 {
		target["instructions"] = strings.Join(instructions, "\n\n")
	}
	copyOptionalNumber(target, "temperature", request.Temperature)
	copyOptionalNumber(target, "top_p", request.TopP)
	if request.Metadata != nil {
		if userID, _ := request.Metadata["user_id"].(string); strings.TrimSpace(userID) != "" {
			target["safety_identifier"] = strings.TrimSpace(userID)
		}
	}
	if request.OutputConfig != nil && request.OutputConfig.Format != nil {
		if request.OutputConfig.Format.Type != "json_schema" || request.OutputConfig.Format.Schema == nil {
			return nil, ResponseOptions{}, errors.New("output_config.format 必须是带 schema 的 json_schema")
		}
		target["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "anthropic_output", "schema": request.OutputConfig.Format.Schema}}
	}
	hasWebSearchTool := hasAnthropicWebSearchTool(request.Tools)
	webSearchEnabled := hasWebSearchTool && (request.ToolChoice == nil || !strings.EqualFold(strings.TrimSpace(request.ToolChoice.Type), "none"))
	webSearchRequired := webSearchEnabled && anthropicWebSearchRequired(request.Tools, request.ToolChoice)
	webSearchQuery := ""
	if webSearchEnabled {
		webSearchQuery = anthropicWebSearchQuery(request.Messages)
	}
	if thinkingEnabled {
		effort := anthropicThinkingEffort(request.Thinking.BudgetTokens)
		if request.OutputConfig != nil && request.OutputConfig.Effort != "" {
			effort = request.OutputConfig.Effort
		}
		switch effort {
		case "minimal":
			effort = "low"
		case "max", "xhigh":
			effort = "high"
		case "low", "medium", "high":
		default:
			return nil, ResponseOptions{}, fmt.Errorf("不支持 output_config.effort=%q", effort)
		}
		target["reasoning"] = map[string]any{"effort": effort, "summary": "detailed"}
		target["include"] = []any{"reasoning.encrypted_content"}
	}
	if len(request.Tools) > 0 {
		tools, err := convertAnthropicTools(request.Tools)
		if err != nil {
			return nil, ResponseOptions{}, err
		}
		target["tools"] = tools
	}
	if len(request.MCPServers) > 0 {
		servers, err := convertAnthropicMCPServers(request.MCPServers)
		if err != nil {
			return nil, ResponseOptions{}, err
		}
		existing, _ := target["tools"].([]any)
		target["tools"] = append(existing, servers...)
	}
	if request.ToolChoice != nil {
		choice, parallel, err := convertAnthropicToolChoice(*request.ToolChoice, hasWebSearchTool)
		if err != nil {
			return nil, ResponseOptions{}, err
		}
		target["tool_choice"] = choice
		target["parallel_tool_calls"] = parallel
	}
	converted, err := json.Marshal(target)
	return converted, ResponseOptions{
		AnthropicThinking:          thinkingEnabled,
		AnthropicWebSearch:         webSearchEnabled,
		AnthropicWebSearchRequired: webSearchRequired,
		AnthropicWebSearchQuery:    webSearchQuery,
		StopSequences:              append([]string(nil), request.StopSequences...),
	}, err
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
	Metadata      map[string]any     `json:"metadata"`
	Thinking      *struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	} `json:"thinking"`
	TopK         json.RawMessage      `json:"top_k"`
	MCPServers   []anthropicMCPServer `json:"mcp_servers"`
	OutputConfig *struct {
		Effort string `json:"effort"`
		Format *struct {
			Type   string         `json:"type"`
			Schema map[string]any `json:"schema"`
		} `json:"format"`
	} `json:"output_config"`
	Tools      []map[string]json.RawMessage `json:"tools"`
	ToolChoice *anthropicToolChoice         `json:"tool_choice"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
}

func convertAnthropicMessages(messages []anthropicMessage, declaredTools map[string]struct{}) ([]any, []string, error) {
	input := make([]any, 0, len(messages))
	instructions := make([]string, 0)
	pendingCalls := make(map[string]struct{})
	usedCalls := make(map[string]struct{})
	serverSearches := make(map[string]map[string]any)
	for messageIndex, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "system" || role == "developer" {
			text, err := anthropicSystemText(message.Content)
			if err != nil {
				return nil, nil, fmt.Errorf("messages[%d] %s 内容无效: %w", messageIndex, role, err)
			}
			if text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, nil, fmt.Errorf("Messages API 不支持 role=%q", message.Role)
		}
		if len(pendingCalls) > 0 && role != "user" {
			return nil, nil, fmt.Errorf("messages[%d] 必须是包含 tool_result 的 user 消息", messageIndex)
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			if len(pendingCalls) > 0 {
				return nil, nil, fmt.Errorf("messages[%d] 必须返回全部待处理 tool_use", messageIndex)
			}
			input = append(input, map[string]any{"type": "message", "role": role, "content": text})
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(message.Content, &blocks) != nil {
			return nil, nil, fmt.Errorf("messages[%d].content 必须是字符串或内容块数组", messageIndex)
		}
		hadPending := len(pendingCalls) > 0
		regularBeforeResult := false
		messageParts := make([]any, 0, len(blocks))
		flushMessage := func() {
			if len(messageParts) > 0 {
				input = append(input, map[string]any{"type": "message", "role": role, "content": messageParts})
				messageParts = nil
			}
		}
		for blockIndex, block := range blocks {
			path := fmt.Sprintf("messages[%d].content[%d]", messageIndex, blockIndex)
			var typeName string
			_ = json.Unmarshal(block["type"], &typeName)
			switch typeName {
			case "text":
				regularBeforeResult = regularBeforeResult || len(pendingCalls) > 0
				var value string
				if json.Unmarshal(block["text"], &value) != nil {
					return nil, nil, fmt.Errorf("%s.text 无效", path)
				}
				messageParts = append(messageParts, map[string]any{"type": "input_text", "text": value})
			case "image":
				regularBeforeResult = regularBeforeResult || len(pendingCalls) > 0
				imageURL, err := anthropicImageURL(block["source"])
				if err != nil {
					return nil, nil, fmt.Errorf("%s: %w", path, err)
				}
				messageParts = append(messageParts, map[string]any{"type": "input_image", "image_url": imageURL})
			case "document":
				regularBeforeResult = regularBeforeResult || len(pendingCalls) > 0
				document, err := anthropicDocument(block)
				if err != nil {
					return nil, nil, fmt.Errorf("%s: %w", path, err)
				}
				messageParts = append(messageParts, document)
			case "tool_use":
				if role != "assistant" {
					return nil, nil, fmt.Errorf("%s tool_use 只允许出现在 assistant 消息", path)
				}
				flushMessage()
				var value struct {
					ID    string         `json:"id"`
					Name  string         `json:"name"`
					Input map[string]any `json:"input"`
				}
				if encoded, _ := json.Marshal(block); json.Unmarshal(encoded, &value) != nil || strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.Name) == "" || value.Input == nil {
					return nil, nil, fmt.Errorf("%s 缺少有效 id、name 或 object input", path)
				}
				if _, exists := usedCalls[value.ID]; exists {
					return nil, nil, fmt.Errorf("%s 包含重复 tool_use id %q", path, value.ID)
				}
				arguments, _ := json.Marshal(value.Input)
				input = append(input, map[string]any{"type": "function_call", "call_id": value.ID, "name": value.Name, "arguments": string(arguments)})
				pendingCalls[value.ID] = struct{}{}
				usedCalls[value.ID] = struct{}{}
			case "tool_result":
				if role != "user" {
					return nil, nil, fmt.Errorf("%s tool_result 只允许出现在 user 消息", path)
				}
				flushMessage()
				var toolUseID string
				_ = json.Unmarshal(block["tool_use_id"], &toolUseID)
				if _, exists := pendingCalls[toolUseID]; strings.TrimSpace(toolUseID) == "" || !exists {
					return nil, nil, fmt.Errorf("%s.tool_use_id %q 未匹配待处理 tool_use", path, toolUseID)
				}
				if regularBeforeResult {
					return nil, nil, fmt.Errorf("%s tool_result 必须位于文本、图片或文档块之前", path)
				}
				output, err := anthropicToolResult(block["content"], declaredTools)
				if err != nil {
					return nil, nil, fmt.Errorf("%s.content: %w", path, err)
				}
				if raw := block["is_error"]; !isEmptyJSON(raw) {
					var isError bool
					if json.Unmarshal(raw, &isError) != nil {
						return nil, nil, fmt.Errorf("%s.is_error 必须是布尔值", path)
					}
					if isError {
						output = markAnthropicToolError(output)
					}
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": toolUseID, "output": output})
				delete(pendingCalls, toolUseID)
			case "thinking":
				if role != "assistant" {
					return nil, nil, fmt.Errorf("%s thinking 只允许出现在 assistant 消息", path)
				}
				flushMessage()
				var thinking, signature string
				_ = json.Unmarshal(block["thinking"], &thinking)
				_ = json.Unmarshal(block["signature"], &signature)
				item := map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": thinking}}}
				if signature != "" {
					item["encrypted_content"] = signature
				}
				input = append(input, item)
			case "redacted_thinking":
				if role != "assistant" {
					return nil, nil, fmt.Errorf("%s redacted_thinking 只允许出现在 assistant 消息", path)
				}
				flushMessage()
				var data string
				if json.Unmarshal(block["data"], &data) != nil || data == "" {
					return nil, nil, fmt.Errorf("%s.data 无效", path)
				}
				// Grok Build / XAI requires the reasoning summary field to exist
				// whenever encrypted_content is replayed. An empty summary is the
				// canonical carrier for Anthropic redacted_thinking.
				input = append(input, map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": data})
			case "server_tool_use":
				if role != "assistant" {
					continue
				}
				var value struct {
					ID    string         `json:"id"`
					Name  string         `json:"name"`
					Input map[string]any `json:"input"`
				}
				encoded, _ := json.Marshal(block)
				if json.Unmarshal(encoded, &value) != nil || value.Name != "web_search" || strings.TrimSpace(value.ID) == "" {
					continue
				}
				flushMessage()
				query, _ := value.Input["query"].(string)
				call := map[string]any{
					"type": "web_search_call", "id": value.ID, "status": "completed",
					"action": map[string]any{"type": "search", "query": query},
				}
				serverSearches[value.ID] = call
				input = append(input, call)
			case "web_search_tool_result":
				if role != "assistant" {
					continue
				}
				var toolUseID string
				_ = json.Unmarshal(block["tool_use_id"], &toolUseID)
				call := serverSearches[strings.TrimSpace(toolUseID)]
				if call == nil {
					continue
				}
				applyAnthropicWebSearchResult(call, block["content"])
			default:
				return nil, nil, fmt.Errorf("当前不支持 Anthropic content.type=%q", typeName)
			}
		}
		flushMessage()
		if hadPending && len(pendingCalls) > 0 {
			return nil, nil, fmt.Errorf("messages[%d] 必须返回全部待处理 tool_use", messageIndex)
		}
	}
	if len(pendingCalls) > 0 {
		return nil, nil, errors.New("messages 必须为每个 tool_use 提供 tool_result")
	}
	return input, instructions, nil
}

func applyAnthropicWebSearchResult(call map[string]any, raw json.RawMessage) {
	var results []map[string]any
	if json.Unmarshal(raw, &results) == nil {
		sources := make([]any, 0, len(results))
		for _, result := range results {
			if result["type"] != "web_search_result" {
				continue
			}
			url, _ := result["url"].(string)
			if strings.TrimSpace(url) != "" {
				sources = append(sources, map[string]any{"type": "url", "url": strings.TrimSpace(url)})
			}
		}
		if action, ok := call["action"].(map[string]any); ok && len(sources) > 0 {
			action["sources"] = sources
		}
		return
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) == nil && result["type"] == "web_search_tool_result_error" {
		call["status"] = "failed"
	}
}

func anthropicSystemText(raw json.RawMessage) (string, error) {
	if isEmptyJSON(raw) {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", errors.New("system 必须是字符串或 text block 数组")
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" {
			return "", fmt.Errorf("system 不支持 type=%q", block.Type)
		}
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n"), nil
}

func anthropicImageURL(raw json.RawMessage) (string, error) {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(raw, &source) != nil {
		return "", errors.New("image.source 无效")
	}
	switch source.Type {
	case "base64":
		if source.MediaType == "" || source.Data == "" {
			return "", errors.New("base64 image 缺少 media_type 或 data")
		}
		return "data:" + source.MediaType + ";base64," + source.Data, nil
	case "url":
		if strings.TrimSpace(source.URL) == "" {
			return "", errors.New("url image 缺少 url")
		}
		return source.URL, nil
	default:
		return "", fmt.Errorf("不支持 image.source.type=%q", source.Type)
	}
}

func anthropicDocument(block map[string]json.RawMessage) (map[string]any, error) {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(block["source"], &source) != nil {
		return nil, errors.New("document.source 无效")
	}
	var title string
	_ = json.Unmarshal(block["title"], &title)
	switch source.Type {
	case "text":
		if source.Data == "" {
			return nil, errors.New("text document 缺少 data")
		}
		return map[string]any{"type": "input_text", "text": source.Data}, nil
	case "url":
		if strings.TrimSpace(source.URL) == "" {
			return nil, errors.New("url document 缺少 url")
		}
		value := map[string]any{"type": "input_file", "file_url": source.URL}
		if title != "" {
			value["filename"] = title
		}
		return value, nil
	case "base64":
		if source.MediaType == "" || source.Data == "" {
			return nil, errors.New("base64 document 缺少 media_type 或 data")
		}
		value := map[string]any{"type": "input_file", "file_data": "data:" + source.MediaType + ";base64," + source.Data}
		if title != "" {
			value["filename"] = title
		}
		return value, nil
	default:
		return nil, fmt.Errorf("不支持 document.source.type=%q", source.Type)
	}
}

func anthropicToolResult(raw json.RawMessage, declaredTools map[string]struct{}) (any, error) {
	if isEmptyJSON(raw) {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return "", errors.New("tool_result.content 无效")
	}
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		var typeName string
		_ = json.Unmarshal(block["type"], &typeName)
		switch typeName {
		case "text":
			var value string
			if json.Unmarshal(block["text"], &value) != nil {
				return nil, errors.New("tool_result text 无效")
			}
			parts = append(parts, map[string]any{"type": "input_text", "text": value})
		case "image":
			imageURL, err := anthropicImageURL(block["source"])
			if err != nil {
				return nil, err
			}
			parts = append(parts, map[string]any{"type": "input_image", "image_url": imageURL})
		case "document":
			document, err := anthropicDocument(block)
			if err != nil {
				return nil, err
			}
			parts = append(parts, document)
		case "tool_reference":
			var toolName string
			if json.Unmarshal(block["tool_name"], &toolName) != nil || strings.TrimSpace(toolName) == "" {
				return nil, errors.New("tool_reference.tool_name 无效")
			}
			toolName = strings.TrimSpace(toolName)
			if _, exists := declaredTools[toolName]; !exists {
				return nil, fmt.Errorf("tool_reference 引用了未声明的工具 %q", toolName)
			}
			// Responses 没有 Anthropic tool_reference 内容块。Messages 请求中的全部
			// 工具定义已发送给上游，因此用确定性的结果文本保留“搜索命中”语义。
			parts = append(parts, map[string]any{
				"type": "input_text",
				"text": fmt.Sprintf("Tool search matched declared tool %q; its definition is available in this request.", toolName),
			})
		default:
			return nil, fmt.Errorf("tool_result 暂不支持 type=%q", typeName)
		}
	}
	return parts, nil
}

func anthropicDeclaredToolNames(tools []map[string]json.RawMessage) map[string]struct{} {
	declared := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		var name string
		_ = json.Unmarshal(tool["name"], &name)
		if name = strings.TrimSpace(name); name != "" {
			declared[name] = struct{}{}
		}
	}
	return declared
}

func markAnthropicToolError(output any) any {
	const prefix = "Tool execution failed: "
	if text, ok := output.(string); ok {
		return prefix + text
	}
	parts, _ := output.([]any)
	return append([]any{map[string]any{"type": "input_text", "text": prefix}}, parts...)
}

func convertAnthropicTools(tools []map[string]json.RawMessage) ([]any, error) {
	result := make([]any, 0, len(tools))
	for index, tool := range tools {
		var typeName string
		_ = json.Unmarshal(tool["type"], &typeName)
		if strings.HasPrefix(typeName, "web_search_") {
			converted, err := convertAnthropicWebSearchTool(tool, index)
			if err != nil {
				return nil, err
			}
			result = append(result, converted)
			continue
		}
		if typeName != "" && typeName != "custom" {
			return nil, fmt.Errorf("当前不支持 Anthropic server tool type=%q", typeName)
		}
		var name, description string
		_ = json.Unmarshal(tool["name"], &name)
		_ = json.Unmarshal(tool["description"], &description)
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("Anthropic tool 缺少 name")
		}
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if raw := tool["input_schema"]; !isEmptyJSON(raw) {
			if json.Unmarshal(raw, &schema) != nil {
				return nil, fmt.Errorf("tool %q 的 input_schema 无效", name)
			}
		}
		converted := map[string]any{"type": "function", "name": name, "description": description, "parameters": schema}
		var strict bool
		if raw := tool["strict"]; !isEmptyJSON(raw) {
			if json.Unmarshal(raw, &strict) != nil {
				return nil, fmt.Errorf("tool %q 的 strict 必须是布尔值", name)
			}
			converted["strict"] = strict
		}
		result = append(result, converted)
	}
	return result, nil
}

func hasAnthropicWebSearchTool(tools []map[string]json.RawMessage) bool {
	for _, tool := range tools {
		var typeName string
		_ = json.Unmarshal(tool["type"], &typeName)
		if strings.HasPrefix(typeName, "web_search_") {
			return true
		}
	}
	return false
}

func anthropicWebSearchRequired(tools []map[string]json.RawMessage, choice *anthropicToolChoice) bool {
	if choice == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(choice.Type)) {
	case "tool":
		return strings.EqualFold(strings.TrimSpace(choice.Name), "web_search")
	case "any":
		return len(tools) == 1 && hasAnthropicWebSearchTool(tools)
	default:
		return false
	}
}

func anthropicWebSearchQuery(messages []anthropicMessage) string {
	const prefix = "perform a web search for the query:"
	for index := len(messages) - 1; index >= 0; index-- {
		if !strings.EqualFold(strings.TrimSpace(messages[index].Role), "user") {
			continue
		}
		text := anthropicMessageText(messages[index].Content)
		if text == "" {
			continue
		}
		if offset := strings.Index(strings.ToLower(text), prefix); offset >= 0 {
			text = strings.TrimSpace(text[offset+len(prefix):])
		}
		return truncateRunes(strings.TrimSpace(text), 4096)
	}
	return ""
}

func anthropicMessageText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	return strings.Join(parts, "\n")
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func convertAnthropicWebSearchTool(tool map[string]json.RawMessage, index int) (map[string]any, error) {
	converted := map[string]any{"type": "web_search"}
	for key, raw := range tool {
		switch key {
		case "type", "name", "cache_control":
			continue
		case "allowed_domains":
			var value any
			if json.Unmarshal(raw, &value) != nil {
				return nil, fmt.Errorf("tools[%d].%s 无效", index, key)
			}
			domains, ok := value.([]any)
			if !ok {
				return nil, fmt.Errorf("tools[%d].%s 必须是字符串数组", index, key)
			}
			if len(domains) > 5 {
				return nil, fmt.Errorf("tools[%d].%s 不能超过 5 个域名", index, key)
			}
			for domainIndex, domain := range domains {
				if text, ok := domain.(string); !ok || strings.TrimSpace(text) == "" {
					return nil, fmt.Errorf("tools[%d].%s[%d] 必须是非空字符串", index, key, domainIndex)
				}
			}
			converted["filters"] = map[string]any{"allowed_domains": value}
		case "max_uses", "blocked_domains", "user_location", "search_context_size":
			// Build 0.2.103 只支持 allowed_domains；其余 Anthropic
			// 可选控制字段不转发，避免上游因未知参数拒绝整个请求。
			continue
		default:
			// 对未来新增的 hosted-tool 可选字段保持向前兼容。
			continue
		}
	}
	return converted, nil
}

type anthropicMCPServer struct {
	Name               string `json:"name"`
	URL                string `json:"url"`
	AuthorizationToken string `json:"authorization_token"`
}

func convertAnthropicMCPServers(servers []anthropicMCPServer) ([]any, error) {
	result := make([]any, 0, len(servers))
	for index, server := range servers {
		name := strings.TrimSpace(server.Name)
		url := strings.TrimSpace(server.URL)
		if name == "" || url == "" {
			return nil, fmt.Errorf("mcp_servers[%d] 缺少 name 或 url", index)
		}
		tool := map[string]any{"type": "mcp", "server_label": name, "server_url": url}
		if server.AuthorizationToken != "" {
			tool["authorization"] = server.AuthorizationToken
		}
		result = append(result, tool)
	}
	return result, nil
}

func anthropicThinkingEffort(budget int) string {
	switch {
	case budget > 0 && budget <= 2048:
		return "low"
	case budget > 10000:
		return "high"
	default:
		return "medium"
	}
}

func convertAnthropicToolChoice(choice anthropicToolChoice, hasHostedWebSearch bool) (any, bool, error) {
	parallel := !choice.DisableParallelToolUse
	switch choice.Type {
	case "auto", "none":
		return choice.Type, parallel, nil
	case "any":
		return "required", parallel, nil
	case "tool":
		if strings.TrimSpace(choice.Name) == "" {
			return nil, false, errors.New("tool_choice.tool 缺少 name")
		}
		// Claude Code secondary search forces name=web_search (hosted). Build accepts
		// hosted tool_choice only as "required" when a single hosted tool remains.
		if hasHostedWebSearch && strings.EqualFold(strings.TrimSpace(choice.Name), "web_search") {
			return "required", parallel, nil
		}
		return map[string]any{"type": "function", "name": choice.Name}, parallel, nil
	default:
		return nil, false, fmt.Errorf("不支持 tool_choice.type=%q", choice.Type)
	}
}
