package searchresult

import (
	"net/url"
	"strings"
)

const (
	maxURLBytes   = 8 << 10
	MaxResults    = 50
	MaxTitleRunes = 512
)

// NormalizeURL accepts only public-link schemes and rejects credential-bearing
// URLs so upstream search data cannot introduce active or privacy-sensitive links.
func NormalizeURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLBytes || strings.IndexAny(raw, "\r\n\t") >= 0 {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Hostname() == "" || parsed.User != nil {
		return "", false
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false
	}
	parsed.Host = strings.ToLower(parsed.Host)
	normalized := parsed.String()
	if len(normalized) > maxURLBytes {
		return "", false
	}
	return normalized, true
}

func NormalizeTitle(raw, fallback string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = strings.TrimSpace(fallback)
	}
	runes := []rune(value)
	if len(runes) > MaxTitleRunes {
		value = string(runes[:MaxTitleRunes])
	}
	return value
}
