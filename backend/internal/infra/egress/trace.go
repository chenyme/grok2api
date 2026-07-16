package egress

import (
	"context"
	"fmt"
	"strings"
	"sync"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// Selection 是一次上游请求实际选择的出口快照。它只包含可安全写入审计的元数据，
// 不包含代理 URL、认证信息、User-Agent 或 Cookie。
type Selection struct {
	NodeID   uint64
	NodeName string
	Scope    domain.Scope
	Proxied  bool
}

// Trace 按作用域保留最后一次实际出口选择。相同请求发生出口重试时，审计记录最终尝试；
// Web 资源归档使用独立作用域，不会覆盖主要的 Grok Web 推理出口。
type Trace struct {
	mu         sync.RWMutex
	selections map[domain.Scope]Selection
}

type traceContextKey struct{}
type accountContextKey struct{}

// WithAccount 将稳定的 Provider 账号身份传递到出口层。该值只用于渲染
// Resin 等粘性代理的认证用户名，不会写入上游 Header 或审计。
func WithAccount(ctx context.Context, provider string, accountID uint64) context.Context {
	if ctx == nil || strings.TrimSpace(provider) == "" || accountID == 0 {
		return ctx
	}
	value := strings.TrimSpace(provider) + "_" + fmt.Sprintf("%d", accountID)
	return context.WithValue(ctx, accountContextKey{}, value)
}

func accountFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(accountContextKey{}).(string)
	return strings.TrimSpace(value)
}

// AccountFromContext exposes the non-sensitive sticky account identity to
// provider transports while keeping the context key private.
func AccountFromContext(ctx context.Context) string { return accountFromContext(ctx) }

// WithTrace 为一次网关请求创建或复用并发安全的出口选择轨迹。
func WithTrace(ctx context.Context) (context.Context, *Trace) {
	if existing := TraceFromContext(ctx); existing != nil {
		return ctx, existing
	}
	trace := &Trace{selections: make(map[domain.Scope]Selection)}
	return context.WithValue(ctx, traceContextKey{}, trace), trace
}

// TraceFromContext 返回上下文中的出口轨迹；未配置时返回 nil。
func TraceFromContext(ctx context.Context) *Trace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(traceContextKey{}).(*Trace)
	return trace
}

// Selection 返回指定作用域最后一次实际出口选择的安全快照。
func (t *Trace) Selection(scope domain.Scope) (Selection, bool) {
	if t == nil {
		return Selection{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	value, ok := t.selections[scope]
	return value, ok
}

func recordSelection(ctx context.Context, value Selection) {
	trace := TraceFromContext(ctx)
	if trace == nil {
		return
	}
	trace.mu.Lock()
	trace.selections[value.Scope] = value
	trace.mu.Unlock()
}
