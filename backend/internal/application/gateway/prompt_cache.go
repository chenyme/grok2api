package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

const promptCacheIdentityVersion = "v1"

// resolvePromptCacheIdentity 将客户端缓存键或会话标识转换为固定长度的上游身份。
// 客户端、Provider、上游模型和协议共同参与摘要，防止共享账号池中的跨租户碰撞。
func resolvePromptCacheIdentity(clientKeyID uint64, provider accountdomain.Provider, upstreamModel string, operation audit.Operation, explicitKey, sessionSeed string) string {
	seed := strings.TrimSpace(explicitKey)
	if seed == "" {
		seed = strings.TrimSpace(sessionSeed)
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if clientKeyID == 0 || provider == "" || model == "" || seed == "" {
		return ""
	}
	if operation == "" {
		operation = audit.OperationResponses
	}
	source := fmt.Sprintf("grok2api:prompt-cache:%s:%d:%s:%s:%s:%s", promptCacheIdentityVersion, clientKeyID, provider, model, operation, seed)
	digest := sha256.Sum256([]byte(source))
	hexID := hex.EncodeToString(digest[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexID[0:8], hexID[8:12], hexID[12:16], hexID[16:20], hexID[20:32])
}
