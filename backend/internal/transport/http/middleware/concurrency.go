package middleware

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
)

// ConcurrencyGate 为单实例推理入口提供可热更新、立即拒绝的总容量保护。
type ConcurrencyGate struct {
	mu     sync.Mutex
	limit  int
	active int
}

// NewConcurrencyGate 创建指定上限的推理入口并发闸门。
func NewConcurrencyGate(limit int) *ConcurrencyGate {
	if limit < 1 {
		panic("middleware: 并发上限必须大于零")
	}
	return &ConcurrencyGate{limit: limit}
}

// UpdateLimit 热更新并发上限；降低上限不会中断正在执行的请求。
func (g *ConcurrencyGate) UpdateLimit(limit int) {
	if limit < 1 {
		panic("middleware: 并发上限必须大于零")
	}
	g.mu.Lock()
	g.limit = limit
	g.mu.Unlock()
}

// Middleware 返回绑定当前 Gate 状态的 Gin 中间件。
func (g *ConcurrencyGate) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		g.mu.Lock()
		if g.active >= g.limit {
			g.mu.Unlock()
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
				"code": "server_overloaded", "message": "服务并发已达到上限，请稍后重试", "param": nil, "type": "server_error",
			}})
			return
		}
		g.active++
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			g.active--
			g.mu.Unlock()
		}()
		c.Next()
	}
}
