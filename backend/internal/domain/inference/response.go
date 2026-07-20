package inference

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// ResponseOwnership 记录上游 Response 资源所属账号，不保存请求或响应正文。
type ResponseOwnership struct {
	ResponseID         string
	AccountID          uint64
	ClientKeyID        uint64
	Provider           account.Provider
	PromptCacheKey     string
	ReasoningReplayKey string
	ExpiresAt          time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// WebResponseState 保存 Grok Web 本地 Responses 资源及其上游会话游标。
type WebResponseState struct {
	ResponseID               string
	AccountID                uint64
	ConversationID           string
	UpstreamParentResponseID string
	ResponseJSON             string
	Status                   string
	ExpiresAt                time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
}
