package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// normalizeShellTool 保留 OpenAI 新版 shell；同一请求不能同时声明新旧两套本地 shell。
func (c *responsesToolCompatibility) normalizeShellTool(tool map[string]any, param string) ([]any, error) {
	if c.legacyLocalShell {
		return nil, &responsesRequestError{
			Message: "同一请求不能同时声明 shell 和 local_shell",
			Param:   param + ".type", Code: "invalid_parameter",
		}
	}
	c.nativeShell = true
	return c.normalizeNativeTool(tool, param)
}

// normalizeLegacyLocalShellTool 将旧 Codex local_shell 升级为 0.2.106 原生 local shell 环境。
func (c *responsesToolCompatibility) normalizeLegacyLocalShellTool(tool map[string]any, param string) ([]any, error) {
	if c.nativeShell || c.legacyLocalShell {
		return nil, &responsesRequestError{
			Message: "单次请求只能声明一个 shell/local_shell 工具",
			Param:   param + ".type", Code: "invalid_parameter",
		}
	}
	if len(tool) > 1 {
		c.addWarning("legacy_local_shell_controls_ignored")
	}
	c.legacyLocalShell = true
	c.changed = true
	c.addWarning("legacy_local_shell_upgraded")
	return []any{map[string]any{
		"type":        "shell",
		"environment": map[string]any{"type": "local"},
	}}, nil
}

// normalizeApplyPatchTool 将客户端执行的 apply_patch 包装为严格 function。
func (c *responsesToolCompatibility) normalizeApplyPatchTool(tool map[string]any, param string) ([]any, error) {
	if len(tool) > 1 {
		c.addWarning("apply_patch_controls_ignored")
	}
	identity := responsesToolIdentity{Kind: responsesApplyPatchTool, Name: "apply_patch"}
	c.changed = true
	c.addWarning("apply_patch_emulated")
	return []any{map[string]any{
		"type": "function",
		"name": c.alias(identity),
		"description": "Create, update, or delete one file using a structured V4A patch operation. " +
			"create_file and update_file require path and diff; delete_file requires path.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type": map[string]any{"type": "string", "enum": []any{"create_file", "update_file", "delete_file"}},
						"path": map[string]any{"type": "string", "minLength": 1},
						"diff": map[string]any{"type": "string"},
					},
					"required": []any{"type", "path"}, "additionalProperties": false,
				},
			},
			"required": []any{"operation"}, "additionalProperties": false,
		},
		"strict": true,
	}}, nil
}

func (c *responsesToolCompatibility) normalizeApplyPatchCallInput(item map[string]any, param string) (map[string]any, error) {
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if callID == "" {
		return nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
	}
	operation, err := validateApplyPatchOperation(item["operation"], param+".operation")
	if err != nil {
		return nil, err
	}
	arguments, err := json.Marshal(map[string]any{"operation": operation})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type": "function_call", "call_id": callID,
		"name":      c.alias(responsesToolIdentity{Kind: responsesApplyPatchTool, Name: "apply_patch"}),
		"arguments": string(arguments),
	}, nil
}

func normalizeApplyPatchOutputInput(item map[string]any, param string) (map[string]any, error) {
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if callID == "" {
		return nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
	}
	status := strings.TrimSpace(stringField(item, "status"))
	if status == "" {
		status = "completed"
	}
	if status != "completed" && status != "failed" {
		return nil, &responsesRequestError{Message: "apply_patch_call_output.status 只支持 completed 或 failed", Param: param + ".status", Code: "invalid_parameter"}
	}
	output := ""
	if value, exists := item["output"]; exists && value != nil {
		if text, ok := value.(string); ok {
			output = text
		} else {
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, &responsesRequestError{Message: "apply_patch_call_output.output 无法编码", Param: param + ".output", Code: "invalid_parameter"}
			}
			output = string(encoded)
		}
	}
	message := "Apply patch status: " + status
	if output != "" {
		message += "\n" + output
	}
	return map[string]any{"type": "function_call_output", "call_id": callID, "output": message}, nil
}

func validateApplyPatchOperation(value any, param string) (map[string]any, error) {
	operation, ok := value.(map[string]any)
	if !ok {
		return nil, &responsesRequestError{Message: "apply_patch operation 必须是对象", Param: param, Code: "invalid_parameter"}
	}
	kind := strings.TrimSpace(stringField(operation, "type"))
	path := strings.TrimSpace(stringField(operation, "path"))
	if path == "" {
		return nil, &responsesRequestError{Message: "apply_patch operation.path 不能为空", Param: param + ".path", Code: "invalid_parameter"}
	}
	switch kind {
	case "create_file", "update_file":
		if _, ok := operation["diff"].(string); !ok {
			return nil, &responsesRequestError{Message: kind + " 必须提供 diff 字符串", Param: param + ".diff", Code: "invalid_parameter"}
		}
	case "delete_file":
	default:
		return nil, &responsesRequestError{Message: "apply_patch operation.type 无效", Param: param + ".type", Code: "invalid_parameter"}
	}
	return cloneJSONObject(operation), nil
}

func decodeApplyPatchArguments(value any, param string) (map[string]any, error) {
	text, ok := value.(string)
	if !ok {
		return nil, &responsesRequestError{Message: "apply_patch function arguments 必须是字符串", Param: param, Code: "invalid_parameter"}
	}
	var wrapper map[string]any
	if err := json.Unmarshal([]byte(text), &wrapper); err != nil {
		return nil, &responsesRequestError{Message: "apply_patch function arguments 不是有效 JSON", Param: param, Code: "invalid_parameter"}
	}
	return validateApplyPatchOperation(wrapper["operation"], param+".operation")
}

func normalizeLegacyLocalShellCallInput(item map[string]any, param string) (map[string]any, error) {
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if callID == "" {
		return nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
	}
	action, err := legacyShellAction(item["action"], param+".action")
	if err != nil {
		return nil, err
	}
	converted := map[string]any{"type": "shell_call", "call_id": callID, "action": action}
	for _, key := range []string{"id", "status", "timeout_ms", "max_output_length"} {
		if value, exists := item[key]; exists {
			converted[key] = cloneJSONValue(value)
		}
	}
	return converted, nil
}

func legacyShellAction(value any, param string) (map[string]any, error) {
	action, ok := value.(map[string]any)
	if !ok {
		return nil, &responsesRequestError{Message: "local_shell_call.action 必须是对象", Param: param, Code: "invalid_parameter"}
	}
	if kind := strings.TrimSpace(stringField(action, "type")); kind != "" && kind != "exec" {
		return nil, &responsesRequestError{Message: "local_shell_call.action.type 必须是 exec", Param: param + ".type", Code: "invalid_parameter"}
	}
	command, err := legacyShellCommand(action, param)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "exec", "commands": []any{command}}, nil
}

func legacyShellCommand(action map[string]any, param string) (string, error) {
	command := ""
	switch value := action["command"].(type) {
	case string:
		command = strings.TrimSpace(value)
	case []any:
		parts := make([]string, 0, len(value))
		for index, raw := range value {
			part, ok := raw.(string)
			if !ok {
				return "", &responsesRequestError{Message: "local_shell command 参数必须是字符串", Param: fmt.Sprintf("%s.command[%d]", param, index), Code: "invalid_parameter"}
			}
			parts = append(parts, quoteShellArgument(part))
		}
		command = strings.Join(parts, " ")
	default:
		if commands, ok := action["commands"].([]any); ok {
			parts := make([]string, 0, len(commands))
			for index, raw := range commands {
				part, ok := raw.(string)
				if !ok {
					return "", &responsesRequestError{Message: "shell commands 必须是字符串", Param: fmt.Sprintf("%s.commands[%d]", param, index), Code: "invalid_parameter"}
				}
				parts = append(parts, part)
			}
			command = strings.Join(parts, "\n")
		}
	}
	if command == "" {
		return "", &responsesRequestError{Message: "local_shell_call.action.command 不能为空", Param: param + ".command", Code: "invalid_parameter"}
	}
	if environment, ok := action["env"].(map[string]any); ok && len(environment) > 0 {
		keys := make([]string, 0, len(environment))
		for key := range environment {
			if !validEnvironmentName(key) {
				return "", &responsesRequestError{Message: "local_shell env 名称无效", Param: param + ".env." + key, Code: "invalid_parameter"}
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		assignments := make([]string, 0, len(keys))
		for _, key := range keys {
			value, ok := environment[key].(string)
			if !ok {
				return "", &responsesRequestError{Message: "local_shell env 值必须是字符串", Param: param + ".env." + key, Code: "invalid_parameter"}
			}
			assignments = append(assignments, key+"="+quoteShellArgument(value))
		}
		command = "env " + strings.Join(assignments, " ") + " " + command
	}
	if directory := strings.TrimSpace(stringField(action, "working_directory")); directory != "" {
		command = "cd " + quoteShellArgument(directory) + " && " + command
	}
	return command, nil
}

func normalizeLegacyLocalShellOutputInput(item map[string]any, param string) (map[string]any, error) {
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if callID == "" {
		return nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
	}
	var output []any
	switch value := item["output"].(type) {
	case []any:
		output = cloneJSONArray(value)
	case string:
		exitCode := 0
		if strings.EqualFold(stringField(item, "status"), "failed") {
			exitCode = 1
		}
		if number, ok := item["exit_code"].(float64); ok {
			exitCode = int(number)
		}
		output = []any{map[string]any{
			"stdout": value, "stderr": "",
			"outcome": map[string]any{"type": "exit", "exit_code": exitCode},
		}}
	default:
		return nil, &responsesRequestError{Message: "local_shell_call_output.output 必须是字符串或数组", Param: param + ".output", Code: "invalid_parameter"}
	}
	converted := map[string]any{"type": "shell_call_output", "call_id": callID, "output": output}
	if value, exists := item["max_output_length"]; exists && value != nil {
		converted["max_output_length"] = cloneJSONValue(value)
	}
	return converted, nil
}

func normalizeShellCallOutputInput(item map[string]any, param string) (map[string]any, error) {
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if callID == "" {
		return nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
	}
	output, err := normalizeShellOutputBlocks(item["output"], item["status"], param+".output")
	if err != nil {
		return nil, err
	}
	converted := map[string]any{"type": "shell_call_output", "call_id": callID, "output": output}
	if value, exists := item["max_output_length"]; exists && value != nil {
		converted["max_output_length"] = cloneJSONValue(value)
	}
	return converted, nil
}

func (c *responsesToolCompatibility) normalizeFunctionCallOutputInput(item map[string]any, param string) (map[string]any, error) {
	return c.normalizeFunctionLikeCallOutputInput(item, param, true)
}

func (c *responsesToolCompatibility) normalizeCustomToolCallOutputInput(item map[string]any, param string) (map[string]any, error) {
	return c.normalizeFunctionLikeCallOutputInput(item, param, false)
}

func (c *responsesToolCompatibility) normalizeFunctionLikeCallOutputInput(item map[string]any, param string, allowContentBlocks bool) (map[string]any, error) {
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if callID == "" {
		return nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
	}
	output := item["output"]
	var err error
	if blocks, ok := output.([]any); ok && allowContentBlocks && isFunctionCallOutputContentArray(blocks) {
		output, err = c.normalizeFunctionCallOutputBlocks(blocks, param+".output")
	} else {
		output, err = encodeToolOutput(output, param+".output")
	}
	if err != nil {
		return nil, err
	}
	// 按官方 Build 回放结构重建，不携带输出态 id/status。
	return map[string]any{"type": "function_call_output", "call_id": callID, "output": output}, nil
}

// isFunctionCallOutputContentArray 区分 Responses 内容数组与普通结构化 JSON 数组。
// 只要出现一个 input_* 内容块，整个数组就按内容数组严格校验，避免混合数组
// 中的图片被静默字符串化；普通对象/标量/空数组仍沿用 JSON 字符串契约。
func isFunctionCallOutputContentArray(blocks []any) bool {
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		blockType := strings.TrimSpace(stringField(block, "type"))
		if strings.HasPrefix(blockType, "input_") {
			return true
		}
	}
	return false
}

func (c *responsesToolCompatibility) normalizeFunctionCallOutputBlocks(blocks []any, param string) ([]any, error) {
	normalized := make([]any, 0, len(blocks))
	for index, raw := range blocks {
		blockParam := fmt.Sprintf("%s[%d]", param, index)
		block, ok := raw.(map[string]any)
		if !ok {
			return nil, &responsesRequestError{Message: blockParam + " 必须是对象", Param: blockParam, Code: "invalid_parameter"}
		}
		blockType := strings.TrimSpace(stringField(block, "type"))
		if blockType == "" {
			return nil, &responsesRequestError{Message: blockParam + ".type 不能为空", Param: blockParam + ".type", Code: "invalid_parameter"}
		}
		switch blockType {
		case "input_text":
			text, ok := block["text"].(string)
			if !ok {
				return nil, &responsesRequestError{Message: blockParam + ".text 必须是字符串", Param: blockParam + ".text", Code: "invalid_parameter"}
			}
			normalized = append(normalized, map[string]any{"type": "input_text", "text": text})
		case "input_image":
			converted, err := c.normalizeFunctionCallOutputImageBlock(block, blockParam)
			if err != nil {
				return nil, err
			}
			normalized = append(normalized, converted)
		case "input_file":
			converted, err := normalizeFunctionCallOutputFileBlock(block, blockParam)
			if err != nil {
				return nil, err
			}
			normalized = append(normalized, converted)
		default:
			return nil, &responsesRequestError{Message: "Grok Build 0.2.106 不支持该 function_call_output.output 类型", Param: blockParam + ".type", Code: "unsupported_parameter"}
		}
	}
	return normalized, nil
}

func (c *responsesToolCompatibility) normalizeFunctionCallOutputImageBlock(block map[string]any, param string) (map[string]any, error) {
	_, hasImageURL, err := nonEmptyContentBlockString(block, "image_url", param)
	if err != nil {
		return nil, err
	}
	_, hasFileID, err := nonEmptyContentBlockString(block, "file_id", param)
	if err != nil {
		return nil, err
	}
	if !hasImageURL && !hasFileID {
		return nil, &responsesRequestError{Message: param + ".image_url 或 .file_id 至少需要一个", Param: param + ".image_url", Code: "invalid_parameter"}
	}
	return c.normalizeInputImagePart(block, param)
}

func normalizeFunctionCallOutputFileBlock(block map[string]any, param string) (map[string]any, error) {
	hasSource := false
	for _, key := range []string{"file_data", "file_id", "file_url", "filename"} {
		_, exists, err := nonEmptyContentBlockString(block, key, param)
		if err != nil {
			return nil, err
		}
		if key != "filename" && exists {
			hasSource = true
		}
	}
	if !hasSource {
		return nil, &responsesRequestError{Message: param + " 至少需要 file_data、file_id 或 file_url 之一", Param: param + ".file_data", Code: "invalid_parameter"}
	}
	return normalizeInputFilePart(block), nil
}

func nonEmptyContentBlockString(block map[string]any, key, param string) (string, bool, error) {
	raw, exists := block[key]
	if !exists || raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", false, &responsesRequestError{Message: param + "." + key + " 必须是非空字符串", Param: param + "." + key, Code: "invalid_parameter"}
	}
	return value, true, nil
}

func encodeToolOutput(value any, param string) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "", &responsesRequestError{Message: param + " 无法编码", Param: param, Code: "invalid_parameter"}
		}
		return string(encoded), nil
	}
}

func normalizeShellOutputBlocks(value, status any, param string) ([]any, error) {
	switch typed := value.(type) {
	case []any:
		output := make([]any, 0, len(typed))
		for index, raw := range typed {
			block, ok := raw.(map[string]any)
			if !ok {
				return nil, &responsesRequestError{Message: fmt.Sprintf("%s[%d] 必须是对象", param, index), Param: fmt.Sprintf("%s[%d]", param, index), Code: "invalid_parameter"}
			}
			normalized, err := normalizeShellOutputBlock(block, fmt.Sprintf("%s[%d]", param, index))
			if err != nil {
				return nil, err
			}
			output = append(output, normalized)
		}
		return output, nil
	case string:
		exitCode := 0
		if strings.EqualFold(fmt.Sprint(status), "failed") {
			exitCode = 1
		}
		return []any{map[string]any{
			"stdout": typed, "stderr": "",
			"outcome": map[string]any{"type": "exit", "exit_code": exitCode},
		}}, nil
	default:
		return nil, &responsesRequestError{Message: param + " 必须是字符串或数组", Param: param, Code: "invalid_parameter"}
	}
}

func normalizeShellOutputBlock(block map[string]any, param string) (map[string]any, error) {
	stdout, _ := block["stdout"].(string)
	stderr, _ := block["stderr"].(string)
	outcome, ok := block["outcome"].(map[string]any)
	if !ok {
		return nil, &responsesRequestError{Message: param + ".outcome 必须是对象", Param: param + ".outcome", Code: "invalid_parameter"}
	}
	normalizedOutcome := map[string]any{"type": strings.TrimSpace(stringField(outcome, "type"))}
	switch normalizedOutcome["type"] {
	case "exit":
		exitCode, ok := shellExitCode(outcome["exit_code"])
		if !ok {
			exitCode, ok = shellExitCode(outcome["exitCode"])
		}
		if !ok {
			return nil, &responsesRequestError{Message: param + ".outcome.exit_code 必须是数字", Param: param + ".outcome.exit_code", Code: "invalid_parameter"}
		}
		normalizedOutcome["exit_code"] = exitCode
	case "timeout":
	default:
		return nil, &responsesRequestError{Message: param + ".outcome.type 无效", Param: param + ".outcome.type", Code: "invalid_parameter"}
	}
	return map[string]any{"stdout": stdout, "stderr": stderr, "outcome": normalizedOutcome}, nil
}

func shellExitCode(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func rewriteLegacyShellAction(value any) map[string]any {
	action, _ := value.(map[string]any)
	commands, _ := action["commands"].([]any)
	parts := make([]string, 0, len(commands))
	for _, raw := range commands {
		if command, ok := raw.(string); ok {
			parts = append(parts, command)
		}
	}
	legacy := map[string]any{"type": "exec", "command": strings.Join(parts, "\n")}
	return legacy
}

func quoteShellArgument(value string) string {
	if value == "" {
		return "''"
	}
	safe := true
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("_@%+=:,./-", character)) {
			safe = false
			break
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func validEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' || (index > 0 && character >= '0' && character <= '9')) {
			return false
		}
	}
	return true
}

func (c *responsesToolCompatibility) normalizeAdditionalToolsInput(item map[string]any, param string) (map[string]any, []any, []any, error) {
	if role := strings.TrimSpace(stringField(item, "role")); role != "" && role != "developer" {
		c.addWarning("additional_tools_role_approximated")
	}
	tools, ok := item["tools"].([]any)
	if !ok {
		return nil, nil, nil, &responsesRequestError{Message: param + ".tools 必须是数组", Param: param + ".tools", Code: "invalid_parameter"}
	}
	c.addWarning("additional_tools_position_approximated")
	normalized := make([]any, 0, len(tools))
	names := make([]string, 0, len(tools))
	for index, rawTool := range tools {
		converted, err := c.normalizeTool(rawTool, "", false, true, fmt.Sprintf("%s.tools[%d]", param, index))
		if err != nil {
			return nil, nil, nil, err
		}
		normalized = append(normalized, converted...)
		if tool, ok := rawTool.(map[string]any); ok {
			name := strings.TrimSpace(stringField(tool, "name"))
			if name == "" {
				name = strings.TrimSpace(stringField(tool, "server_label"))
			}
			if name == "" {
				name = strings.TrimSpace(stringField(tool, "type"))
			}
			if name != "" {
				names = append(names, name)
			}
		}
	}
	message := "Additional tools become available at this point in the conversation."
	if len(names) > 0 {
		message += "\nTools: " + strings.Join(names, ", ")
	}
	return compatibilityBoundaryMessage(message), normalized, cloneJSONArray(tools), nil
}

func compatibilityBoundaryMessage(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "developer",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

func dedupeNormalizedTools(tools []any) []any {
	result := make([]any, 0, len(tools))
	positions := make(map[string]int)
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			result = append(result, raw)
			continue
		}
		kind := stringField(tool, "type")
		name := stringField(tool, "name")
		if name == "" {
			name = stringField(tool, "server_label")
		}
		key := kind + "\x00" + name
		if name == "" {
			key = kind
		}
		if index, exists := positions[key]; exists {
			result[index] = raw
			continue
		}
		positions[key] = len(result)
		result = append(result, raw)
	}
	return result
}

func (c *responsesToolCompatibility) addWarning(code string) {
	if c == nil || code == "" {
		return
	}
	if _, exists := c.warningSet[code]; exists {
		return
	}
	c.warningSet[code] = struct{}{}
	c.warnings = append(c.warnings, code)
}

func (c *responsesToolCompatibility) warningHeader() string {
	if c == nil {
		return ""
	}
	return strings.Join(c.warnings, ",")
}
