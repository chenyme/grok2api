package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// promptCacheIdentityVersion 变更 seed 算法时递增，避免新旧 identity 混用。
	promptCacheIdentityVersion = "v1"
	// freeCacheNativeToolsJSON：Free OAuth 无工具请求注入 native search
	// 以进入可缓存档，同时 tool_choice=none 禁止真实搜索。
	freeCacheNativeToolsJSON    = `[{"type":"web_search"},{"type":"x_search"}]`
	freeCacheDisabledToolChoice = "none"
)

// resolvePromptCacheKey 为上游 Grok 生成稳定、租户隔离的 prompt_cache_key。
// 优先保留客户端显式 key；否则从 session 头/body 或内容前缀推导。
// clientKeyID 必填，避免不同客户端共享同一缓存身份。
func resolvePromptCacheKey(clientKeyID uint64, publicModel, upstreamModel, explicit, sessionHeader string, body []byte) string {
	if clientKeyID == 0 {
		return strings.TrimSpace(explicit)
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if model == "" {
		model = strings.ToLower(strings.TrimSpace(publicModel))
	}
	if model == "" {
		return strings.TrimSpace(explicit)
	}

	seed := strings.TrimSpace(sessionHeader)
	if seed == "" {
		seed = strings.TrimSpace(explicit)
	}
	if seed == "" && len(body) > 0 {
		var probe struct {
			PromptCacheKey string `json:"prompt_cache_key"`
		}
		_ = json.Unmarshal(body, &probe)
		seed = strings.TrimSpace(probe.PromptCacheKey)
	}
	if seed == "" {
		seed = deriveContentSessionSeed(body)
	}
	if seed == "" {
		return ""
	}

	isolated := fmt.Sprintf("grok-prompt-cache:%s:%d:%s:%s", promptCacheIdentityVersion, clientKeyID, model, seed)
	sum := sha256.Sum256([]byte(isolated))
	// UUID 形态，兼容上游 x-grok-conv-id / prompt_cache_key 常见消费方。
	hexID := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexID[0:8], hexID[8:12], hexID[12:16], hexID[16:20], hexID[20:32])
}

// deriveContentSessionSeed 只取多轮中相对稳定的前缀字段。
// Claude Code 追加 messages 时 identity 不变。
// 关键：tools 只取排序后的 name 列表（忽略 description/schema 抖动与 MCP 元数据噪声），
// 否则 Claude Code 工具描述微调就会换 key → 换账号 → 缓存从 90%+ 掉到 0。
func deriveContentSessionSeed(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return ""
	}

	var parts []string
	if model := rawString(root["model"]); model != "" {
		parts = append(parts, "model="+model)
	}
	if tools := toolNamesSeed(root["tools"]); tools != "" {
		parts = append(parts, "tools="+tools)
	}
	if system := systemTextSeed(root["system"]); system != "" {
		parts = append(parts, "system="+system)
	}
	if instructions := rawString(root["instructions"]); instructions != "" {
		parts = append(parts, "instructions="+instructions)
	}
	// metadata.user_id 是部分客户端的会话锚点；有则优先于 first_user。
	if userID := metadataUserID(root["metadata"]); userID != "" {
		parts = append(parts, "user_id="+userID)
	} else if first := firstUserSeed(root); first != "" {
		// 仅取首条 user 的纯文本摘要，去掉 cache_control / 过大内容，降低噪声。
		parts = append(parts, "first_user="+first)
	}
	if len(parts) == 0 {
		return ""
	}
	// 内容种子可能很长（system 巨大）；最终 identity 会再 hash，这里保持可读调试字段。
	return "compat_cs_" + strings.Join(parts, "|")
}

// toolNamesSeed 只保留工具名并排序，避免 schema/description 变化打穿会话身份。
func toolNamesSeed(raw json.RawMessage) string {
	if len(raw) == 0 || isJSONNull(raw) || isJSONEmptyArray(raw) {
		return ""
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return ""
	}
	names := make([]string, 0, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		var name string
		_ = json.Unmarshal(tool["name"], &name)
		name = strings.TrimSpace(name)
		if name == "" {
			// OpenAI 扁平 tools 可能把 name 放在 function.name
			if fn := tool["function"]; len(fn) > 0 {
				var function struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal(fn, &function)
				name = strings.TrimSpace(function.Name)
			}
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	// 简单插入排序，避免额外 import sort 在极小切片上的依赖噪音；数量可达数百，改用标准库。
	return strings.Join(sortedStrings(names), ",")
}

func systemTextSeed(raw json.RawMessage) string {
	if len(raw) == 0 || isJSONNull(raw) {
		return ""
	}
	if text := rawString(raw); text != "" {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return normalizeJSONSeed(raw)
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "" && block.Type != "text" {
			continue
		}
		text := strings.TrimSpace(block.Text)
		if text == "" || strings.HasPrefix(text, "x-anthropic-billing-header") {
			continue
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n")
}

func metadataUserID(raw json.RawMessage) string {
	if len(raw) == 0 || isJSONNull(raw) {
		return ""
	}
	var meta map[string]any
	if json.Unmarshal(raw, &meta) != nil {
		return ""
	}
	for _, key := range []string{"user_id", "userId", "session_id", "sessionId"} {
		if value, ok := meta[key]; ok {
			switch typed := value.(type) {
			case string:
				if strings.TrimSpace(typed) != "" {
					return strings.TrimSpace(typed)
				}
			}
		}
	}
	return ""
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j-1] > out[j] {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}

func firstUserSeed(root map[string]json.RawMessage) string {
	if raw := root["messages"]; len(raw) > 0 {
		var messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &messages) == nil {
			for _, msg := range messages {
				if strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
					return firstUserTextSummary(msg.Content)
				}
			}
		}
	}
	if raw := root["input"]; len(raw) > 0 {
		if text := rawString(raw); text != "" {
			return text
		}
		var items []struct {
			Role    string          `json:"role"`
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &items) == nil {
			for _, item := range items {
				role := strings.ToLower(strings.TrimSpace(item.Role))
				if role == "user" {
					if len(item.Content) > 0 {
						return firstUserTextSummary(item.Content)
					}
					if strings.TrimSpace(item.Text) != "" {
						return clipSeedText(strings.TrimSpace(item.Text))
					}
				}
				if item.Type == "input_text" && strings.TrimSpace(item.Text) != "" {
					return clipSeedText(strings.TrimSpace(item.Text))
				}
			}
		}
	}
	return ""
}

// firstUserTextSummary 提取首条 user 的文本锚点。
// Claude Code 首条 user 可能很大；只保留纯文本并截断，足够区分会话即可。
func firstUserTextSummary(raw json.RawMessage) string {
	if text := rawString(raw); text != "" {
		return clipSeedText(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Type != "" && block.Type != "text" {
				continue
			}
			if text := strings.TrimSpace(block.Text); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return clipSeedText(strings.Join(parts, "\n"))
		}
	}
	return clipSeedText(normalizeJSONSeed(raw))
}

func clipSeedText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	const maxRunes = 512
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes])
}

// ensurePromptCacheKeyInBody 把 identity 写入 Responses 请求体的 prompt_cache_key。
// 客户端原有值会被租户隔离 identity 覆盖，避免共享 OAuth 账号时 key 碰撞。
func ensurePromptCacheKeyInBody(body []byte, identity string) ([]byte, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return body, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		payload = make(map[string]json.RawMessage)
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return nil, err
	}
	payload["prompt_cache_key"] = encoded
	return json.Marshal(payload)
}

// maybeInjectFreeCacheTools 在 Grok Build OAuth、且客户端未声明 tools/tool_choice 时，
// 注入 native web_search/x_search + tool_choice=none，帮助 Free 请求进入可缓存档。
// intentBody 使用注入前的原始意图，避免规范化删掉不支持工具后误注入。
func maybeInjectFreeCacheTools(body, intentBody []byte, providerName, authType string) ([]byte, bool, error) {
	if !strings.EqualFold(strings.TrimSpace(providerName), "grok_build") {
		return body, false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(authType), "oauth") {
		return body, false, nil
	}
	if hasToolsOrToolChoice(intentBody) || hasToolsOrToolChoice(body) {
		return body, false, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, err
	}
	if payload == nil {
		payload = make(map[string]json.RawMessage)
	}
	payload["tools"] = json.RawMessage(freeCacheNativeToolsJSON)
	payload["tool_choice"] = mustRawJSON(freeCacheDisabledToolChoice)
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func hasToolsOrToolChoice(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var probe struct {
		Tools      json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	if len(probe.ToolChoice) > 0 && !isJSONNull(probe.ToolChoice) {
		// tool_choice=none/auto 也算客户端意图，不覆盖。
		return true
	}
	if len(probe.Tools) == 0 || isJSONNull(probe.Tools) {
		return false
	}
	return !isJSONEmptyArray(probe.Tools)
}

func normalizeJSONSeed(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	// Anthropic system/content 可能是 block 数组；去掉 cache_control 再归一，
	// 避免 Claude Code 断点位置变化导致 seed 漂移。
	value = stripCacheControlValue(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return string(raw)
	}
	return string(encoded)
}

func stripCacheControlValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		delete(typed, "cache_control")
		for key, child := range typed {
			typed[key] = stripCacheControlValue(child)
		}
		return typed
	case []any:
		for i, child := range typed {
			typed[i] = stripCacheControlValue(child)
		}
		return typed
	default:
		return value
	}
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || isJSONNull(raw) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	return ""
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func isJSONEmptyArray(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "[]"
}

func mustRawJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
