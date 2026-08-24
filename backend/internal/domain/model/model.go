package model

import (
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

const (
	MaxPublicIDLength      = 255
	MaxUpstreamModelLength = 255
)

type Capability string

type Origin string

const (
	CapabilityResponses Capability = "responses"
	CapabilityChat      Capability = "chat"
	CapabilityImage     Capability = "image"
	CapabilityImageEdit Capability = "image_edit"
	CapabilityVideo     Capability = "video"
	CapabilityTTS       Capability = "tts"
	CapabilitySTT       Capability = "stt"
	CapabilityRealtime  Capability = "realtime"
)

// Capabilities returns every persisted route capability. Callers use this as
// the single source of truth for bounds that depend on the maximum group size.
func Capabilities() []Capability {
	return []Capability{
		CapabilityResponses,
		CapabilityChat,
		CapabilityImage,
		CapabilityImageEdit,
		CapabilityVideo,
		CapabilityTTS,
		CapabilitySTT,
		CapabilityRealtime,
	}
}

const (
	OriginCatalog    Origin = "catalog"
	OriginDiscovered Origin = "discovered"
	OriginManual     Origin = "manual"
)

// Route 表示带 Provider 前缀的内部路由 ID 到真实上游模型名的稳定映射。
type Route struct {
	ID                uint64
	PublicID          string
	Provider          account.Provider
	UpstreamModel     string
	Capability        Capability
	Origin            Origin
	Enabled           bool
	BoundAccountIDs   []uint64
	SupportedAccounts int
	SyncedAccounts    int
	TotalAccounts     int
	LastSyncedAt      *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RouteGroup is the admin-facing projection of one logical model target.
// Managed catalog capabilities share a group. Manual routes remain independent
// scheduling targets even when their visible names happen to match: neither a
// display name nor an upstream model is a stable identity for account bindings.
type RouteGroup struct {
	Routes []Route
}

// NormalizePublicID 将内部路由 ID 规范化为稳定的 Provider 命名空间。
// Provider 前缀只用于区分内部路由目标，不应直接暴露给下游客户端。
func NormalizePublicID(provider account.Provider, value string) (string, bool) {
	if !provider.IsValid() {
		return "", false
	}
	localID := strings.TrimSpace(value)
	if localID == "" {
		return "", false
	}
	for _, candidate := range account.Providers() {
		prefix := candidate.ModelNamespace() + "/"
		if len(localID) < len(prefix) || !strings.EqualFold(localID[:len(prefix)], prefix) {
			continue
		}
		if candidate != provider {
			return "", false
		}
		localID = strings.TrimSpace(localID[len(prefix):])
		break
	}
	if localID == "" {
		return "", false
	}
	publicID := provider.ModelNamespace() + "/" + localID
	if len([]rune(publicID)) > MaxPublicIDLength {
		return "", false
	}
	return publicID, true
}

// NormalizeExternalPublicID 将客户端可见名称放入 Provider 的内部命名空间。
// 与 NormalizePublicID 不同，同来源前缀属于客户端名称本身：例如 Build/grok
// 会保存为 Build/Build/grok，并由 ExternalPublicID 还原为 Build/grok。
func NormalizeExternalPublicID(provider account.Provider, value string) (string, bool) {
	if !provider.IsValid() {
		return "", false
	}
	publicName := strings.TrimSpace(value)
	if publicName == "" {
		return "", false
	}
	for _, candidate := range account.Providers() {
		prefix := candidate.ModelNamespace() + "/"
		if len(publicName) < len(prefix) || !strings.EqualFold(publicName[:len(prefix)], prefix) {
			continue
		}
		if candidate != provider {
			return "", false
		}
		break
	}
	publicID := provider.ModelNamespace() + "/" + publicName
	if len([]rune(publicID)) > MaxPublicIDLength {
		return "", false
	}
	return publicID, true
}

// IsCanonicalPublicID 判断内部路由 ID 是否已经采用精确的稳定命名空间。
func IsCanonicalPublicID(provider account.Provider, value string) bool {
	normalized, ok := NormalizePublicID(provider, value)
	return ok && normalized == value
}

// ExternalPublicID 返回下游客户端使用的不带 Provider 前缀的模型名称。
func ExternalPublicID(provider account.Provider, value string) string {
	value = strings.TrimSpace(value)
	prefix := provider.ModelNamespace() + "/"
	if provider.IsValid() && len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return strings.TrimSpace(value[len(prefix):])
	}
	return value
}

// PublicIDCandidateGroups 将下游模型名称展开为按匹配优先级排列的内部路由 ID 组。
// 无前缀名称同时匹配所有 Provider。带前缀名称先按字面对外名称匹配，若不存在，
// 再回退为历史上的显式 Provider 路由语法。
func PublicIDCandidateGroups(value string) [][]string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	for _, providerValue := range account.Providers() {
		prefix := providerValue.ModelNamespace() + "/"
		if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
			literal, literalOK := NormalizeExternalPublicID(providerValue, value)
			qualified, qualifiedOK := NormalizePublicID(providerValue, value)
			if !literalOK || !qualifiedOK {
				return nil
			}
			if literal == qualified {
				return [][]string{{literal}}
			}
			return [][]string{{literal}, {qualified}}
		}
	}
	group := make([]string, 0, len(account.Providers()))
	for _, providerValue := range account.Providers() {
		if normalized, ok := NormalizePublicID(providerValue, value); ok {
			group = append(group, normalized)
		}
	}
	if len(group) == 0 {
		return nil
	}
	return [][]string{group}
}

// PublicIDCandidates 返回扁平化的内部路由候选，供无状态解析器按顺序尝试。
func PublicIDCandidates(value string) []string {
	groups := PublicIDCandidateGroups(value)
	result := make([]string, 0, len(account.Providers())+1)
	for _, group := range groups {
		result = append(result, group...)
	}
	return result
}

// NormalizeUpstreamModel 接受带或不带来源前缀的上游模型名称，并返回 Provider 实际接收的名称。
func NormalizeUpstreamModel(provider account.Provider, value string) (string, bool) {
	if !provider.IsValid() {
		return "", false
	}
	upstream := strings.TrimSpace(value)
	if upstream == "" {
		return "", false
	}
	for _, candidate := range account.Providers() {
		prefix := candidate.ModelNamespace() + "/"
		if len(upstream) < len(prefix) || !strings.EqualFold(upstream[:len(prefix)], prefix) {
			continue
		}
		if candidate != provider {
			return "", false
		}
		upstream = strings.TrimSpace(upstream[len(prefix):])
		break
	}
	if upstream == "" || len([]rune(upstream)) > MaxUpstreamModelLength {
		return "", false
	}
	return upstream, true
}

// DisplayUpstreamModel 返回管理界面使用的带来源前缀上游模型名称。
func DisplayUpstreamModel(provider account.Provider, value string) string {
	upstream, ok := NormalizeUpstreamModel(provider, value)
	if !ok {
		return strings.TrimSpace(value)
	}
	return provider.ModelNamespace() + "/" + upstream
}
