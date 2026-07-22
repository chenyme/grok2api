package egress

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxSubscriptionBytes     = 2 << 20
	maxSubscriptionEntries   = 10000
	maxSubscriptionHops      = 3
	subscriptionFetchTimeout = 20 * time.Second
)

var blockedSubscriptionPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

type subscriptionEntry struct {
	ProxyURL string
	Key      string
}

func normalizeSubscriptionURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxProxyURLBytes {
		return "", errors.New("订阅地址为空或过长")
	}
	if strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return "", errors.New("订阅地址包含控制字符")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("订阅地址格式无效")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("订阅地址必须使用 HTTP 或 HTTPS")
	}
	if parsed.Fragment != "" {
		return "", errors.New("订阅地址不能包含片段")
	}
	return parsed.String(), nil
}

func fetchProxySubscription(ctx context.Context, value string) ([]byte, error) {
	normalized, err := normalizeSubscriptionURL(value)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           publicDialContext(net.DefaultResolver),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       15 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= maxSubscriptionHops {
				return errors.New("订阅重定向次数过多")
			}
			if _, err := normalizeSubscriptionURL(request.URL.String()); err != nil {
				return errors.New("订阅重定向地址无效")
			}
			return nil
		},
	}
	requestCtx, cancel := context.WithTimeout(ctx, subscriptionFetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, normalized, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "text/plain, text/*;q=0.9, */*;q=0.1")
	request.Header.Set("User-Agent", "grok2api-egress-subscription/1")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("订阅服务返回 HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSubscriptionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxSubscriptionBytes {
		return nil, errors.New("订阅内容超过大小限制")
	}
	return body, nil
}

// publicDialContext resolves every destination immediately before dialing. It
// rejects loopback, private, link-local, multicast, and carrier-grade NAT
// addresses, including redirect destinations, to avoid subscription SSRF.
func publicDialContext(resolver *net.Resolver) func(context.Context, string, string) (net.Conn, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := resolvePublicAddresses(ctx, resolver, host)
		if err != nil {
			return nil, err
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
		var lastErr error
		for _, ip := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("订阅地址没有可用的公网 IP")
	}
}

func resolvePublicAddresses(ctx context.Context, resolver *net.Resolver, host string) ([]netip.Addr, error) {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if parsed, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddress(parsed) {
			return nil, errors.New("订阅地址不能指向内网")
		}
		return []netip.Addr{parsed.Unmap()}, nil
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	public := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		if isPublicAddress(address) {
			public = append(public, address.Unmap())
		}
	}
	if len(public) == 0 {
		return nil, errors.New("订阅地址不能指向内网")
	}
	return public, nil
}

func isPublicAddress(value netip.Addr) bool {
	value = value.Unmap()
	if !value.IsValid() || !value.IsGlobalUnicast() || value.IsLoopback() || value.IsPrivate() || value.IsLinkLocalUnicast() || value.IsLinkLocalMulticast() || value.IsMulticast() || value.IsUnspecified() {
		return false
	}
	for _, prefix := range blockedSubscriptionPrefixes {
		if prefix.Contains(value) {
			return false
		}
	}
	return true
}

func parseProxySubscription(value string) ([]subscriptionEntry, int, error) {
	entries, skipped := parseProxyLines(value)
	if len(entries) > 0 {
		return entries, skipped, nil
	}
	compact := strings.Map(func(character rune) rune {
		if character == ' ' || character == '\t' || character == '\r' || character == '\n' {
			return -1
		}
		return character
	}, strings.TrimPrefix(value, "\ufeff"))
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(compact)
		if err != nil || len(decoded) == 0 || len(decoded) > maxSubscriptionBytes {
			continue
		}
		entries, decodedSkipped := parseProxyLines(string(decoded))
		if len(entries) > 0 {
			// The original Base64 text is not an invalid proxy entry. Once it
			// decodes to a valid list, report only invalid decoded entries.
			return entries, decodedSkipped, nil
		}
	}
	return nil, skipped, errors.New("订阅中没有可用的 HTTP 或 SOCKS 代理")
}

func parseProxyLines(value string) ([]subscriptionEntry, int) {
	value = strings.TrimPrefix(value, "\ufeff")
	seen := make(map[string]struct{})
	entries := make([]subscriptionEntry, 0)
	skipped := 0
	for line := range strings.SplitSeq(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		normalized, err := NormalizeProxyURL(line)
		if err != nil {
			skipped++
			continue
		}
		digest := sha256.Sum256([]byte(normalized))
		key := hex.EncodeToString(digest[:])
		if _, exists := seen[key]; exists {
			skipped++
			continue
		}
		seen[key] = struct{}{}
		entries = append(entries, subscriptionEntry{ProxyURL: normalized, Key: key})
		if len(entries) > maxSubscriptionEntries {
			return nil, skipped
		}
	}
	return entries, skipped
}

func sourceNodeName(sourceName string, index int) string {
	suffix := fmt.Sprintf(" %03d", index+1)
	value := strings.TrimSpace(sourceName)
	for len(value)+len(suffix) > 160 && value != "" {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value) + suffix
}
