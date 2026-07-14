package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"
)

const (
	maxFunctionTools       = 128
	maxToolDescriptionSize = 16 << 10
)

var (
	toolNamePattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	toolSyntaxPattern    = regexp.MustCompile(`(?i)<tool_calls|<tool_call|<function_call|<invoke\s|"tool_calls"\s*:`)
	toolCallsRootPattern = regexp.MustCompile(`(?is)<tool_calls\s*>(.*?)</tool_calls\s*>`)
	toolCallPattern      = regexp.MustCompile(`(?is)<tool_call\s*>(.*?)</tool_call\s*>`)
	toolNameTagPattern   = regexp.MustCompile(`(?is)<tool_name\s*>(.*?)</tool_name\s*>`)
	toolParamsTagPattern = regexp.MustCompile(`(?is)<parameters\s*>(.*?)</parameters\s*>`)
	functionCallPattern  = regexp.MustCompile(`(?is)<function_call\s*>(.*?)</function_call\s*>`)
	functionNamePattern  = regexp.MustCompile(`(?is)<name\s*>(.*?)</name\s*>`)
	functionArgsPattern  = regexp.MustCompile(`(?is)<arguments\s*>(.*?)</arguments\s*>`)
	invokePattern        = regexp.MustCompile(`(?is)<invoke\s+name=["']?([A-Za-z0-9_-]+)["']?\s*>(.*?)</invoke\s*>`)
)

type functionTool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type toolConfiguration struct {
	Functions      []functionTool
	available      map[string]struct{}
	Choice         string
	ForcedName     string
	ResponseTools  []any
	ResponseChoice any
}

type parsedToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type toolParseResult struct {
	Calls     []parsedToolCall
	SawSyntax bool
	Start     int
	End       int
}

type toolStreamResult struct {
	SafeText string
	Calls    []parsedToolCall
	Complete bool
	Raw      string
}

type toolStreamSieve struct {
	available map[string]struct{}
	buffer    string
	capturing bool
	done      bool
}

// parseToolConfiguration 兼容 Chat Completions 与 Responses 的函数工具结构。
func parseToolConfiguration(rawTools, rawChoice json.RawMessage) (toolConfiguration, error) {
	configuration := toolConfiguration{Choice: "auto", ResponseChoice: "auto"}
	trimmed := bytes.TrimSpace(rawTools)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		var values []map[string]any
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return toolConfiguration{}, errors.New("tools 必须是数组")
		}
		if len(values) > maxFunctionTools {
			return toolConfiguration{}, fmt.Errorf("tools 不能超过 %d 个", maxFunctionTools)
		}
		configuration.ResponseTools = make([]any, 0, len(values))
		for _, value := range values {
			configuration.ResponseTools = append(configuration.ResponseTools, value)
			function, supported, err := parseFunctionTool(value)
			if err != nil {
				return toolConfiguration{}, err
			}
			if supported {
				configuration.Functions = append(configuration.Functions, function)
				continue
			}
			typeName, _ := value["type"].(string)
			switch strings.ToLower(strings.TrimSpace(typeName)) {
			case "web_search", "web_search_preview":
				// Grok Web 原生搜索始终由上游执行，这两个标准声明无需注入函数提示词。
			default:
				return toolConfiguration{}, fmt.Errorf("Grok Web 暂不支持 tools.type=%q", typeName)
			}
		}
	}

	choice, forcedName, responseChoice, err := parseToolChoice(rawChoice)
	if err != nil {
		return toolConfiguration{}, err
	}
	configuration.Choice = choice
	configuration.ForcedName = forcedName
	configuration.ResponseChoice = responseChoice
	configuration.available = make(map[string]struct{}, len(configuration.Functions))
	for _, function := range configuration.Functions {
		if _, exists := configuration.available[function.Name]; exists {
			return toolConfiguration{}, fmt.Errorf("function tool 名称 %q 重复", function.Name)
		}
		configuration.available[function.Name] = struct{}{}
	}
	if forcedName != "" {
		if _, ok := configuration.available[forcedName]; !ok {
			return toolConfiguration{}, fmt.Errorf("tool_choice 指定的函数 %q 不存在", forcedName)
		}
	}
	if (choice == "required" || forcedName != "") && len(configuration.Functions) == 0 {
		return toolConfiguration{}, errors.New("tool_choice 要求调用函数，但 tools 中没有可用函数")
	}
	return configuration, nil
}

func parseFunctionTool(value map[string]any) (functionTool, bool, error) {
	typeName, _ := value["type"].(string)
	if strings.ToLower(strings.TrimSpace(typeName)) != "function" {
		return functionTool{}, false, nil
	}
	definition := value
	if nested, ok := value["function"].(map[string]any); ok {
		definition = nested
	}
	name, _ := definition["name"].(string)
	name = strings.TrimSpace(name)
	if !toolNamePattern.MatchString(name) {
		return functionTool{}, false, errors.New("function tool 的 name 必须是 1 到 64 位字母、数字、下划线或连字符")
	}
	description, _ := definition["description"].(string)
	if len(description) > maxToolDescriptionSize {
		return functionTool{}, false, fmt.Errorf("函数 %q 的 description 过长", name)
	}
	parameters := json.RawMessage(`{"type":"object","properties":{}}`)
	if raw, ok := definition["parameters"]; ok {
		encoded, err := json.Marshal(raw)
		if err != nil || !json.Valid(encoded) {
			return functionTool{}, false, fmt.Errorf("函数 %q 的 parameters 不是有效 JSON", name)
		}
		parameters = encoded
	}
	return functionTool{Name: name, Description: strings.TrimSpace(description), Parameters: parameters}, true, nil
}

func parseToolChoice(raw json.RawMessage) (string, string, any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "auto", "", "auto", nil
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		text = strings.ToLower(strings.TrimSpace(text))
		switch text {
		case "auto", "none", "required":
			return text, "", text, nil
		default:
			return "", "", nil, errors.New("tool_choice 必须是 auto、none、required 或函数对象")
		}
	}
	var value map[string]any
	if json.Unmarshal(trimmed, &value) != nil {
		return "", "", nil, errors.New("tool_choice 格式无效")
	}
	typeName, _ := value["type"].(string)
	typeName = strings.ToLower(strings.TrimSpace(typeName))
	switch typeName {
	case "none", "auto", "required":
		return typeName, "", value, nil
	case "function":
		name, _ := value["name"].(string)
		if nested, ok := value["function"].(map[string]any); ok {
			name, _ = nested["name"].(string)
		}
		name = strings.TrimSpace(name)
		if !toolNamePattern.MatchString(name) {
			return "", "", nil, errors.New("tool_choice.function.name 无效")
		}
		return "required", name, value, nil
	default:
		return "", "", nil, fmt.Errorf("Grok Web 暂不支持 tool_choice.type=%q", typeName)
	}
}

// injectToolPrompt 将函数定义转换为 Grok Web 可稳定生成的 XML 调用约定。
func injectToolPrompt(prompt string, configuration toolConfiguration) string {
	if len(configuration.Functions) == 0 || configuration.Choice == "none" {
		return prompt
	}
	var definitions strings.Builder
	for index, function := range configuration.Functions {
		if index > 0 {
			definitions.WriteString("\n\n")
		}
		definitions.WriteString("Tool: ")
		definitions.WriteString(function.Name)
		if function.Description != "" {
			definitions.WriteString("\nDescription: ")
			definitions.WriteString(function.Description)
		}
		definitions.WriteString("\nParameters: ")
		definitions.Write(function.Parameters)
	}
	choiceInstruction := "Call a tool when it is clearly needed. Otherwise respond in plain text."
	if configuration.ForcedName != "" {
		choiceInstruction = fmt.Sprintf("You MUST call the tool named %q and must not write a plain-text reply.", configuration.ForcedName)
	} else if configuration.Choice == "required" {
		choiceInstruction = "You MUST call at least one available tool and must not write a plain-text reply."
	}
	system := fmt.Sprintf(`You have access to the following tools.

AVAILABLE TOOLS:
%s

TOOL CALL FORMAT - follow these rules exactly:
- When calling a tool, output only the XML block below, with no text before or after it.
- <parameters> must contain one valid JSON object.
- Put multiple calls inside one <tool_calls> element.
- Do not use Markdown code fences.

<tool_calls>
  <tool_call>
    <tool_name>TOOL_NAME</tool_name>
    <parameters>{"key":"value"}</parameters>
  </tool_call>
</tool_calls>

WHEN TO CALL: %s`, definitions.String(), choiceInstruction)
	return "[system]\n" + system + "\n\n" + prompt
}

func toolCallsToXML(raw json.RawMessage) string {
	var values []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("<tool_calls>")
	for _, value := range values {
		if !toolNamePattern.MatchString(value.Function.Name) {
			continue
		}
		arguments := normalizeToolArguments(value.Function.Arguments)
		builder.WriteString("\n  <tool_call>\n    <tool_name>")
		builder.WriteString(html.EscapeString(value.Function.Name))
		builder.WriteString("</tool_name>\n    <parameters>")
		builder.WriteString(arguments)
		builder.WriteString("</parameters>\n  </tool_call>")
	}
	builder.WriteString("\n</tool_calls>")
	return builder.String()
}

// parseToolCalls 解析模型输出，并仅保留请求中声明过的函数名。
func parseToolCalls(text string, available map[string]struct{}) toolParseResult {
	result := toolParseResult{SawSyntax: toolSyntaxPattern.MatchString(text), Start: -1, End: -1}
	if !result.SawSyntax {
		return result
	}
	if match := toolCallsRootPattern.FindStringSubmatchIndex(text); match != nil {
		result.Start, result.End = match[0], match[1]
		root := text[match[2]:match[3]]
		for _, raw := range toolCallPattern.FindAllStringSubmatch(root, -1) {
			nameMatch := toolNameTagPattern.FindStringSubmatch(raw[1])
			if len(nameMatch) == 0 {
				continue
			}
			arguments := "{}"
			if paramsMatch := toolParamsTagPattern.FindStringSubmatch(raw[1]); len(paramsMatch) > 0 {
				arguments = paramsMatch[1]
			}
			appendParsedToolCall(&result.Calls, html.UnescapeString(strings.TrimSpace(nameMatch[1])), arguments, available)
		}
		return result
	}
	if match := functionCallPattern.FindStringSubmatchIndex(text); match != nil {
		result.Start, result.End = match[0], match[1]
		inner := text[match[2]:match[3]]
		nameMatch := functionNamePattern.FindStringSubmatch(inner)
		if len(nameMatch) > 0 {
			arguments := "{}"
			if argsMatch := functionArgsPattern.FindStringSubmatch(inner); len(argsMatch) > 0 {
				arguments = argsMatch[1]
			}
			appendParsedToolCall(&result.Calls, html.UnescapeString(strings.TrimSpace(nameMatch[1])), arguments, available)
		}
		return result
	}
	if match := invokePattern.FindStringSubmatchIndex(text); match != nil {
		result.Start, result.End = match[0], match[1]
		appendParsedToolCall(&result.Calls, text[match[2]:match[3]], text[match[4]:match[5]], available)
		return result
	}
	return parseJSONToolCalls(text, available, result)
}

func parseJSONToolCalls(text string, available map[string]struct{}, result toolParseResult) toolParseResult {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return result
	}
	decoder := json.NewDecoder(strings.NewReader(text[start:]))
	var envelope struct {
		ToolCalls []map[string]any `json:"tool_calls"`
	}
	if decoder.Decode(&envelope) != nil || len(envelope.ToolCalls) == 0 {
		return result
	}
	result.Start = start
	result.End = len(text)
	for _, value := range envelope.ToolCalls {
		name, _ := value["name"].(string)
		arguments := value["arguments"]
		if function, ok := value["function"].(map[string]any); ok {
			name, _ = function["name"].(string)
			arguments = function["arguments"]
		}
		if name == "" {
			name, _ = value["tool_name"].(string)
		}
		if arguments == nil {
			arguments = value["parameters"]
		}
		argumentText := "{}"
		if text, ok := arguments.(string); ok {
			argumentText = text
		} else if arguments != nil {
			encoded, _ := json.Marshal(arguments)
			argumentText = string(encoded)
		}
		appendParsedToolCall(&result.Calls, strings.TrimSpace(name), argumentText, available)
	}
	return result
}

func appendParsedToolCall(calls *[]parsedToolCall, name, arguments string, available map[string]struct{}) {
	if _, ok := available[name]; !ok {
		return
	}
	arguments = normalizeToolArguments(html.UnescapeString(strings.TrimSpace(arguments)))
	if !json.Valid([]byte(arguments)) {
		return
	}
	var object map[string]any
	if json.Unmarshal([]byte(arguments), &object) != nil {
		return
	}
	*calls = append(*calls, parsedToolCall{ID: newWebID("call"), Name: name, Arguments: arguments})
}

func normalizeToolArguments(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}"
	}
	var parsed any
	if json.Unmarshal([]byte(value), &parsed) != nil {
		return value
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return value
	}
	return string(encoded)
}

func removeToolSyntax(text string, parsed toolParseResult) string {
	if parsed.Start < 0 || parsed.End <= parsed.Start || parsed.End > len(text) {
		return text
	}
	return strings.TrimSpace(text[:parsed.Start] + text[parsed.End:])
}

func newToolStreamSieve(available map[string]struct{}) *toolStreamSieve {
	return &toolStreamSieve{available: available}
}

// Feed 在流中发现工具 XML 后开始缓存，完整解析前不向客户端泄露内部标记。
func (s *toolStreamSieve) Feed(chunk string) toolStreamResult {
	if s.done || chunk == "" {
		return toolStreamResult{SafeText: chunk}
	}
	combined := s.buffer + chunk
	s.buffer = ""
	if !s.capturing {
		lower := strings.ToLower(combined)
		index := strings.Index(lower, "<tool_calls")
		if index < 0 {
			safe, pending := splitToolPrefix(combined)
			s.buffer = pending
			return toolStreamResult{SafeText: safe}
		}
		s.capturing = true
		s.buffer = combined[index:]
		combined = combined[:index]
	}
	lower := strings.ToLower(s.buffer)
	endIndex := strings.Index(lower, "</tool_calls>")
	if endIndex < 0 {
		return toolStreamResult{SafeText: combined}
	}
	endIndex += len("</tool_calls>")
	raw := s.buffer[:endIndex]
	remainder := s.buffer[endIndex:]
	parsed := parseToolCalls(raw, s.available)
	s.buffer = ""
	s.capturing = false
	s.done = len(parsed.Calls) > 0
	if len(parsed.Calls) == 0 {
		raw += remainder
	}
	return toolStreamResult{SafeText: combined, Calls: parsed.Calls, Complete: true, Raw: raw}
}

func (s *toolStreamSieve) Flush() toolStreamResult {
	if s.done || s.buffer == "" {
		return toolStreamResult{}
	}
	raw := s.buffer
	s.buffer = ""
	parsed := parseToolCalls(raw, s.available)
	if len(parsed.Calls) > 0 {
		s.done = true
		return toolStreamResult{Calls: parsed.Calls, Complete: true, Raw: raw}
	}
	return toolStreamResult{SafeText: raw, Complete: parsed.SawSyntax, Raw: raw}
}

func splitToolPrefix(value string) (string, string) {
	prefix := "<tool_calls"
	lower := strings.ToLower(value)
	for size := min(len(prefix)-1, len(lower)); size > 0; size-- {
		if strings.HasSuffix(lower, prefix[:size]) {
			return value[:len(value)-size], value[len(value)-size:]
		}
	}
	return value, ""
}
