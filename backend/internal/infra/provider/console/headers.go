package console

import (
	"net/http"
	"strings"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/browserheaders"
)

func applyHeaders(request *http.Request, token string, lease *infraegress.Lease) {
	userAgent := strings.TrimSpace(lease.UserAgent)
	if userAgent == "" {
		userAgent = infraegress.DefaultUserAgent
	}
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	request.Header.Set("Authorization", "Bearer anonymous")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", infraegress.BuildSSOCookie(token, lease.CFCookies))
	request.Header.Set("Origin", "https://console.x.ai")
	request.Header.Set("Referer", "https://console.x.ai/")
	request.Header.Set("Sec-Fetch-Dest", "empty")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Priority", "u=1, i")
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("x-cluster", "https://us-east-1.api.x.ai")
	applyChromiumClientHints(request.Header, userAgent)
}

// applyChromiumClientHints keeps the HTTP headers aligned with the Chromium
// TLS profile used by the Console transport. Non-Chromium User-Agents do not
// receive synthetic hints, avoiding contradictory browser fingerprints.
func applyChromiumClientHints(header http.Header, userAgent string) {
	browserheaders.ApplyChromiumClientHints(header, userAgent)
}
