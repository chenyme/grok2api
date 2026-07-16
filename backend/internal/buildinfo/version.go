package buildinfo

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// Version 可在发布构建时通过 -ldflags -X 注入。
var Version string

// CurrentVersion 返回当前运行实例的版本。源码运行优先读取仓库 VERSION，
// 容器和发行包可将 VERSION 放在可执行文件同目录。
func CurrentVersion() string {
	if value := cleanVersion(Version); value != "" {
		return value
	}
	if value := cleanVersion(os.Getenv("GROK2API_VERSION")); value != "" {
		return value
	}
	candidates := []string{"VERSION", filepath.Join("..", "VERSION")}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "VERSION"))
	}
	for _, candidate := range candidates {
		if data, err := os.ReadFile(candidate); err == nil {
			if value := cleanVersion(string(data)); value != "" {
				return value
			}
		}
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func cleanVersion(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return ""
	}
	return value
}
