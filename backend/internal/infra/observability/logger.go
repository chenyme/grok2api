package observability

import (
	"log/slog"
	"os"
)

// NewLogger 创建结构化 JSON 日志器。
func NewLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
