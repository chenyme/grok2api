package httpserver

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// registerFrontend 在构建产物存在时托管静态文件，并为前端路由提供 SPA 回退。
func registerFrontend(router *gin.Engine, staticPath string) {
	root, indexPath, ok := frontendRoot(staticPath)
	if !ok {
		return
	}
	files := http.FileServer(http.Dir(root))
	router.NoRoute(func(c *gin.Context) {
		requestPath := c.Request.URL.Path
		if (c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) || isBackendPath(requestPath) {
			c.Status(http.StatusNotFound)
			return
		}
		if filePath, exists := frontendFile(root, requestPath); exists {
			if strings.HasPrefix(path.Clean(requestPath), "/assets/") {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				c.Header("Cache-Control", "no-cache")
			}
			c.Request.URL.Path = "/" + filepath.ToSlash(filePath)
			files.ServeHTTP(c.Writer, c.Request)
			return
		}
		if path.Ext(path.Clean(requestPath)) != "" {
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "no-cache")
		http.ServeFile(c.Writer, c.Request, indexPath)
	})
}

func frontendRoot(staticPath string) (string, string, bool) {
	staticPath = strings.TrimSpace(staticPath)
	if staticPath == "" {
		return "", "", false
	}
	root, err := filepath.Abs(staticPath)
	if err != nil {
		return "", "", false
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", "", false
	}
	indexPath := filepath.Join(root, "index.html")
	indexInfo, err := os.Stat(indexPath)
	if err != nil || !indexInfo.Mode().IsRegular() {
		return "", "", false
	}
	return filepath.Clean(root), indexPath, true
}

func frontendFile(root, requestPath string) (string, bool) {
	cleanPath := strings.TrimPrefix(path.Clean("/"+requestPath), "/")
	if cleanPath == "" || cleanPath == "." {
		return "", false
	}
	fullPath := filepath.Join(root, filepath.FromSlash(cleanPath))
	relative, err := filepath.Rel(root, fullPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Stat(fullPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return relative, true
}

func isBackendPath(value string) bool {
	cleanPath := path.Clean("/" + value)
	for _, prefix := range []string{"/api", "/v1", "/swagger"} {
		if cleanPath == prefix || strings.HasPrefix(cleanPath, prefix+"/") {
			return true
		}
	}
	return cleanPath == "/healthz" || cleanPath == "/readyz"
}
