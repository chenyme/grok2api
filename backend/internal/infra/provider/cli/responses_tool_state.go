package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	maxBuildToolAliasLength       = 128
	maxToolSearchDescriptionBytes = 16 << 10
)

type responsesToolKind uint8

const (
	responsesFunctionTool responsesToolKind = iota
	responsesCustomTool
	responsesToolSearch
	responsesApplyPatchTool
)

type responsesToolIdentity struct {
	Kind      responsesToolKind
	Namespace string
	Name      string
}

func (i responsesToolIdentity) key() string {
	return fmt.Sprintf("%d\x00%s\x00%s", i.Kind, i.Namespace, i.Name)
}

// responsesToolCompatibility 保存一次请求内的工具别名和响应恢复状态；实例不得跨请求复用。
type responsesToolCompatibility struct {
	aliases           map[string]responsesToolIdentity
	identityAliases   map[string]string
	visibleTools      []any
	deferredSurfaces  []string
	clientSearchTool  map[string]any
	clientSearchParam string
	serverSearchEager bool
	streamCalls       map[string]*responsesStreamCall
	legacyLocalShell  bool
	nativeShell       bool
	webSearchDisabled bool
	warnings          []string
	warningSet        map[string]struct{}
	changed           bool
	stripExternal     bool
	droppedTools      []string
}

// responsesRequestError 表示可直接映射为 OpenAI 错误结构的 Provider 请求错误。
type responsesRequestError struct {
	Message string
	Param   string
	Code    string
}

func (e *responsesRequestError) Error() string { return e.Message }

func newResponsesToolCompatibility() *responsesToolCompatibility {
	return &responsesToolCompatibility{
		aliases:         make(map[string]responsesToolIdentity),
		identityAliases: make(map[string]string),
		streamCalls:     make(map[string]*responsesStreamCall),
		warningSet:      make(map[string]struct{}),
		stripExternal:   stripExternalClientTools(),
	}
}


func stripExternalClientTools() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GROK2API_STRIP_EXTERNAL_TOOLS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func isExternalClientToolKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "computer_use_preview",
		"web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":
		return true
	default:
		return false
	}
}

func (c *responsesToolCompatibility) dropExternalTool(tool map[string]any, namespace string) {
	name := strings.TrimSpace(stringField(tool, "name"))
	if name == "" {
		name = strings.TrimSpace(stringField(tool, "server_label"))
	}
	if name == "" {
		name = strings.TrimSpace(stringField(tool, "type"))
	}
	if namespace != "" && name != "" {
		name = namespace + "." + name
	}
	if name != "" {
		c.droppedTools = append(c.droppedTools, name)
	}
	c.changed = true
	c.addWarning("external_tools_omitted")
}

func (c *responsesToolCompatibility) externalHistoryBoundary(item map[string]any, label string) map[string]any {
	c.changed = true
	c.addWarning("external_tool_history_omitted")
	name := strings.TrimSpace(stringField(item, "name"))
	if name == "" {
		name = strings.TrimSpace(stringField(item, "type"))
	}
	callID := strings.TrimSpace(stringField(item, "call_id"))
	text := "External tool history omitted for Grok Build compatibility."
	if label != "" {
		text += "\nKind: " + label
	}
	if name != "" {
		text += "\nName: " + name
	}
	if callID != "" {
		text += "\nCall ID: " + callID
	}
	return compatibilityBoundaryMessage(text)
}

func (c *responsesToolCompatibility) addDroppedToolsBoundary(payload map[string]json.RawMessage) error {
	if !c.stripExternal || len(c.droppedTools) == 0 {
		return nil
	}
	text := "Client-side external tools are not available through this Grok Build upstream. Continue without calling those tools."
	boundary := compatibilityBoundaryMessage(text)
	raw := payload["input"]
	if isEmptyJSON(raw) {
		payload["input"] = mustJSON([]any{boundary})
		return nil
	}
	var input any
	if err := json.Unmarshal(raw, &input); err != nil {
		return &responsesRequestError{Message: "input 必须是字符串或数组", Param: "input", Code: "invalid_parameter"}
	}
	switch typed := input.(type) {
	case string:
		payload["input"] = mustJSON([]any{
			boundary,
			map[string]any{"type": "message", "role": "user", "content": typed},
		})
	case []any:
		payload["input"] = mustJSON(append([]any{boundary}, typed...))
	default:
		return &responsesRequestError{Message: "input 必须是字符串或数组", Param: "input", Code: "invalid_parameter"}
	}
	c.changed = true
	return nil
}

func (c *responsesToolCompatibility) alias(identity responsesToolIdentity) string {
	key := identity.key()
	if alias, exists := c.identityAliases[key]; exists {
		return alias
	}
	base := identity.Name
	if identity.Kind == responsesToolSearch {
		base = "grok2api_tool_search"
	} else if identity.Kind == responsesApplyPatchTool {
		base = "grok2api_apply_patch"
	} else if identity.Namespace != "" {
		separator := "__"
		if strings.HasSuffix(identity.Namespace, separator) {
			separator = ""
		}
		base = identity.Namespace + separator + identity.Name
	}
	alias := truncateToolAlias(base, key)
	if existing, collision := c.aliases[alias]; collision && existing.key() != key {
		alias = hashedToolAlias(base, key)
	}
	c.aliases[alias] = identity
	c.identityAliases[key] = alias
	return alias
}

func truncateToolAlias(base, key string) string {
	if len(base) <= maxBuildToolAliasLength {
		return base
	}
	return hashedToolAlias(base, key)
}

func hashedToolAlias(base, key string) string {
	suffix := "__" + shortToolHash(key)
	limit := maxBuildToolAliasLength - len(suffix)
	if len(base) > limit {
		base = base[:limit]
	}
	return base + suffix
}

func shortToolHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:9]
}

func stringField(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}

func cloneJSONArray(values []any) []any {
	cloned := make([]any, len(values))
	for index, value := range values {
		cloned[index] = cloneJSONValue(value)
	}
	return cloned
}

func cloneJSONObject(value map[string]any) map[string]any {
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = cloneJSONValue(item)
	}
	return cloned
}

func cloneJSONValue(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned any
	if json.Unmarshal(data, &cloned) != nil {
		return value
	}
	return cloned
}
