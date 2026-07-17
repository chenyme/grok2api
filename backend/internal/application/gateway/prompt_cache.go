package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

const buildSessionIdentityVersion = "v2"

type buildSessionIdentity struct {
	upstreamID  string
	affinityKey string
}

// resolveBuildSessionIdentity 将客户端缓存键或会话标识拆分为两种用途：
// upstreamID 在同一客户端会话内跨协议、跨模型保持稳定，用于上游缓存和 CLI 会话 Header；
// affinityKey 额外包含上游模型，用于账号粘滞，避免不同模型能力的账号相互覆盖绑定。
func resolveBuildSessionIdentity(clientKeyID uint64, provider accountdomain.Provider, upstreamModel, explicitKey, sessionSeed string) buildSessionIdentity {
	seed := strings.TrimSpace(explicitKey)
	if seed == "" {
		seed = strings.TrimSpace(sessionSeed)
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if clientKeyID == 0 || provider == "" || model == "" || seed == "" {
		return buildSessionIdentity{}
	}
	upstreamSource := fmt.Sprintf("grok2api:build-session:%s:%d:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, seed)
	affinitySource := fmt.Sprintf("grok2api:build-affinity:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, seed)
	return buildSessionIdentity{
		upstreamID:  digestUUID(upstreamSource),
		affinityKey: hexDigest(affinitySource),
	}
}

func digestUUID(source string) string {
	digest := sha256.Sum256([]byte(source))
	hexID := hex.EncodeToString(digest[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexID[0:8], hexID[8:12], hexID[12:16], hexID[16:20], hexID[20:32])
}

func hexDigest(source string) string {
	digest := sha256.Sum256([]byte(source))
	return hex.EncodeToString(digest[:])
}
