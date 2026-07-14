package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

func buildHeaders(token string, lease *infraegress.Lease, contentType string) http.Header {
	if contentType == "" {
		contentType = "application/json"
	}
	value := http.Header{}
	value.Set("Content-Type", contentType)
	value.Set("Accept", "*/*")
	value.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	value.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	value.Set("User-Agent", lease.UserAgent)
	value.Set("Cookie", infraegress.BuildSSOCookie(token, lease.CFCookies))
	value.Set("x-xai-request-id", newRequestUUID())
	return value
}

// applyAppHeaders 补齐真实浏览器同源 fetch 会携带的稳定请求头，不伪造 Sentry 或 Client Hints。
func applyAppHeaders(value http.Header, origin, referer string) {
	value.Set("Origin", origin)
	value.Set("Referer", referer)
	value.Set("Cache-Control", "no-cache")
	value.Set("Pragma", "no-cache")
	value.Set("Priority", "u=1, i")
	value.Set("Sec-Fetch-Dest", "empty")
	value.Set("Sec-Fetch-Mode", "cors")
	value.Set("Sec-Fetch-Site", "same-origin")
}

func newRequestUUID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return newWebID("req")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
