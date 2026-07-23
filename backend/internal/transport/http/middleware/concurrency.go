package middleware

import (
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// ConcurrencyGate 为单实例推理入口提供可热更新、立即拒绝的总容量保护。
type ConcurrencyGate struct {
	limit  atomic.Int64
	active atomic.Int64
}

// NewConcurrencyGate 创建指定上限的推理入口并发闸门。
func NewConcurrencyGate(limit int) *ConcurrencyGate {
	if limit < 1 {
		panic("middleware: 并发上限必须大于零")
	}
	g := &ConcurrencyGate{}
	g.limit.Store(int64(limit))
	return g
}

// UpdateLimit 热更新并发上限；降低上限不会中断正在执行的请求。
func (g *ConcurrencyGate) UpdateLimit(limit int) {
	if limit < 1 {
		panic("middleware: 并发上限必须大于零")
	}
	g.limit.Store(int64(limit))
}

// Middleware 返回绑定当前 Gate 状态的 Gin 中间件。
func (g *ConcurrencyGate) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		for {
			current := g.active.Load()
			if current >= g.limit.Load() {
				c.Header("Retry-After", "1")
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
					"code": "server_overloaded", "message": "服务并发已达到上限，请稍后重试", "param": nil, "type": "server_error",
				}})
				return
			}
			if g.active.CompareAndSwap(current, current+1) {
				break
			}
		}
		defer g.active.Add(-1)
		c.Next()
	}
}
