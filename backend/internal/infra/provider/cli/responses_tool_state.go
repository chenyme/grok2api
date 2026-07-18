package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	aliases             map[string]responsesToolIdentity
	identityAliases     map[string]string
	visibleTools        []any
	deferredSurfaces    []string
	clientSearchTool    map[string]any
	clientSearchParam   string
	serverSearchEager   bool
	streamCalls         map[string]*responsesStreamCall
	legacyLocalShell    bool
	nativeShell         bool
	webSearchDisabled   bool
	compactionRequested bool
	warnings            []string
	warningSet          map[string]struct{}
	changed             bool
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
	}
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
