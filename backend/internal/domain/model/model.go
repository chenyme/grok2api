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
)

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

// PublicIDCandidates 将下游模型名称展开为按 Provider 优先级排列的内部路由 ID。
// 已显式携带 Provider 前缀的名称只会匹配指定来源。
func PublicIDCandidates(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	for _, providerValue := range account.Providers() {
		prefix := providerValue.ModelNamespace() + "/"
		if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
			normalized, ok := NormalizePublicID(providerValue, value)
			if !ok {
				return nil
			}
			return []string{normalized}
		}
	}
	result := make([]string, 0, len(account.Providers()))
	for _, providerValue := range account.Providers() {
		if normalized, ok := NormalizePublicID(providerValue, value); ok {
			result = append(result, normalized)
		}
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
