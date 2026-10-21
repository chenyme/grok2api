package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/encoding/protowire"
)

// Free Console accounts must send the team's apiKeyId as x-apikey on every
// /v1 inference request; without it the upstream answers 429 "provision an API
// key". The key is read from the Console home page SSR payload and created
// only when the team has none, because CreateApiKey has no idempotency guard.
const (
	consoleAPIKeyHeader       = "x-apikey"
	consoleAPIKeyName         = "xAI Chat Playground"
	consoleCreateAPIKeyPath   = "/auth_mgmt.AuthManagement/CreateApiKey"
	consoleHomeBodyLimit      = 8 << 20
	consoleAPIKeyCacheLimit   = 8192
	consoleAPIKeyACLEndpoint  = "api-key:endpoint:*"
	consoleAPIKeyACLModel     = "api-key:model:*"
	consoleAPIKeyScanDistance = 4096
	consoleHomeMaxRedirects   = 3
)

var (
	errConsoleAPIKeyUnavailable = errors.New("Console 首页未提供可用的 apiKeyId")
	consoleAPIKeyIDPattern      = regexp.MustCompile(`"apiKeyId"\s*:\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)
	consoleDisabledPattern      = regexp.MustCompile(`"disabled"\s*:\s*true`)
	consoleTeamPathPattern      = regexp.MustCompile(`/team/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)
	consoleTeamIDPattern        = regexp.MustCompile(`"teamId"\s*:\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)
)

// CredentialCacheStore persists Console-only acceleration caches. Both values
// are recoverable from the SSO alone, so write failures never fail a request.
type CredentialCacheStore interface {
	UpdateConsoleAPIKeyID(ctx context.Context, accountID uint64, apiKeyID string) error
	UpdateConsoleWorkOSSession(ctx context.Context, accountID uint64, encryptedSession string) error
}

// consoleAPIKeyCache keeps resolved keys between the persistent write and the
// next candidate snapshot reload. Entries are keyed by account and SSO hash so
// a replaced SSO can never reuse another team's key.
type consoleAPIKeyCache struct {
	mu     sync.Mutex
	values map[string]string
	loads  singleflight.Group
}

func newConsoleAPIKeyCache() *consoleAPIKeyCache {
	return &consoleAPIKeyCache{values: make(map[string]string)}
}

func consoleAPIKeyCacheKey(credential account.Credential, ssoToken string) string {
	return fmt.Sprintf("%d|%s", credential.ID, security.HashToken(ssoToken))
}

func (c *consoleAPIKeyCache) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key]
}

func (c *consoleAPIKeyCache) set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.values) >= consoleAPIKeyCacheLimit {
		for existing := range c.values {
			delete(c.values, existing)
			break
		}
	}
	c.values[key] = value
}

func (c *consoleAPIKeyCache) invalidate(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.values[key]; ok && (value == "" || current == value) {
		delete(c.values, key)
	}
}

// consoleAPIKey returns the x-apikey for the credential. refresh skips both
// the persisted value and the process cache, used after the upstream rejected
// the cached key.
func (a *Adapter) consoleAPIKey(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease, refresh bool) (string, error) {
	key := consoleAPIKeyCacheKey(credential, ssoToken)
	if !refresh {
		if value := a.apiKeys.get(key); value != "" {
			return value, nil
		}
		if value := strings.TrimSpace(credential.ConsoleAPIKeyID); value != "" {
			return value, nil
		}
	}
	// Coalesce concurrent resolutions per account: two parallel misses must not
	// both reach CreateApiKey and leave a stray key on the team.
	value, err, _ := a.apiKeys.loads.Do(key, func() (any, error) {
		if !refresh {
			if cached := a.apiKeys.get(key); cached != "" {
				return cached, nil
			}
		}
		resolved, resolveErr := a.resolveConsoleAPIKey(ctx, credential, ssoToken, lease)
		if resolveErr != nil {
			return "", resolveErr
		}
		a.apiKeys.set(key, resolved)
		if resolved != strings.TrimSpace(credential.ConsoleAPIKeyID) {
			a.saveConsoleAPIKeyID(ctx, credential.ID, resolved)
		}
		return resolved, nil
	})
	if err != nil {
		return "", err
	}
	resolved, _ := value.(string)
	if resolved == "" {
		return "", errConsoleAPIKeyUnavailable
	}
	return resolved, nil
}

func (a *Adapter) invalidateConsoleAPIKey(credential account.Credential, ssoToken, value string) {
	a.apiKeys.invalidate(consoleAPIKeyCacheKey(credential, ssoToken), value)
}

func (a *Adapter) saveConsoleAPIKeyID(ctx context.Context, accountID uint64, apiKeyID string) {
	store := a.cacheStore()
	if store == nil || accountID == 0 {
		return
	}
	if err := store.UpdateConsoleAPIKeyID(context.WithoutCancel(ctx), accountID, apiKeyID); err != nil {
		a.logger().Warn("console_api_key_cache_write_failed", "account_id", accountID, "error", err)
	}
}

// resolveConsoleAPIKey is the read-then-create sequence: reuse any enabled
// team key from the Console home page and create one only when none exists.
func (a *Adapter) resolveConsoleAPIKey(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease) (string, error) {
	home, err := a.readConsoleHome(ctx, ssoToken, lease)
	if err != nil {
		return "", err
	}
	if keyID := home.apiKeyID; keyID != "" {
		return keyID, nil
	}
	if home.teamID == "" {
		return "", fmt.Errorf("%w: 缺少 teamId", errConsoleAPIKeyUnavailable)
	}
	if err := a.createConsoleAPIKey(ctx, credential, ssoToken, lease, home.teamID); err != nil {
		return "", err
	}
	// The create response carries the secret key body, not its id. Re-read the
	// home page so only the stable apiKeyId is ever used and stored.
	confirmed, err := a.readConsoleHome(ctx, ssoToken, lease)
	if err != nil {
		return "", err
	}
	if confirmed.apiKeyID == "" {
		return "", fmt.Errorf("%w: 创建后回读未找到", errConsoleAPIKeyUnavailable)
	}
	return confirmed.apiKeyID, nil
}

type consoleHome struct {
	apiKeyID string
	teamID   string
}

// readConsoleHome loads the Console landing page, which 307-redirects to
// /team/<teamId>. Only same-origin hops are followed; the team id is taken
// from the final path because it is authoritative for the SSO's team.
func (a *Adapter) readConsoleHome(ctx context.Context, ssoToken string, lease *infraegress.Lease) (consoleHome, error) {
	origin := consoleOrigin(a.config().BaseURL)
	current, err := url.Parse(origin + "/")
	if err != nil {
		return consoleHome{}, err
	}
	extraCookies := map[string]string{}
	for hop := 0; hop <= consoleHomeMaxRedirects; hop++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return consoleHome{}, err
		}
		applyBrowserHeaders(request, ssoToken, lease)
		request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		request.Header.Set("Sec-Fetch-Dest", "document")
		request.Header.Set("Sec-Fetch-Mode", "navigate")
		request.Header.Set("Sec-Fetch-Site", "none")
		request.Header.Del("Origin")
		request.Header.Del("Referer")
		if len(extraCookies) > 0 {
			parts := make([]string, 0, len(extraCookies))
			for name, value := range extraCookies {
				parts = append(parts, name+"="+value)
			}
			request.Header.Set("Cookie", joinCookieHeaders(request.Header.Get("Cookie"), strings.Join(parts, "; ")))
		}
		response, err := lease.DoDeferredForbidden(request)
		if err != nil {
			return consoleHome{}, err
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			location := response.Header.Get("Location")
			for _, cookie := range response.Cookies() {
				if cookie.Name != "sso" && cookie.Name != "sso-rw" {
					extraCookies[cookie.Name] = cookie.Value
				}
			}
			_, _, _ = provider.ReadDiagnosticBody(response.Body)
			_ = response.Body.Close()
			next, err := current.Parse(location)
			if location == "" || err != nil || !strings.EqualFold(next.Host, current.Host) {
				return consoleHome{}, fmt.Errorf("Console 首页跳转到非同源地址（status %d）", response.StatusCode)
			}
			current = next
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			data, _, _ := provider.ReadDiagnosticBody(response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusForbidden && shouldInvalidateConsoleClearance(data) {
				lease.InvalidateClearance()
			}
			if response.StatusCode == http.StatusUnauthorized {
				return consoleHome{}, fmt.Errorf("%w: Console 首页返回 401", provider.ErrUnauthorized)
			}
			return consoleHome{}, fmt.Errorf("Console 首页返回 %d", response.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, consoleHomeBodyLimit))
		_ = response.Body.Close()
		if err != nil {
			return consoleHome{}, err
		}
		home := parseConsoleHome(data)
		if match := consoleTeamPathPattern.FindStringSubmatch(current.Path); match != nil {
			home.teamID = strings.ToLower(match[1])
		}
		return home, nil
	}
	return consoleHome{}, fmt.Errorf("Console 首页跳转次数超过 %d", consoleHomeMaxRedirects)
}

// parseConsoleHome extracts the first enabled apiKeyId and the team id from
// the Next.js SSR payload. The payload embeds JSON as escaped string literals
// (\"apiKeyId\":\"…\", sometimes escaped twice); UUIDs never contain
// backslashes, so dropping them yields plain JSON fragments to scan.
func parseConsoleHome(data []byte) consoleHome {
	text := string(bytes.ReplaceAll(data, []byte(`\`), nil))
	result := consoleHome{}
	matches := consoleAPIKeyIDPattern.FindAllStringSubmatchIndex(text, -1)
	for index, match := range matches {
		end := len(text)
		if index+1 < len(matches) {
			end = matches[index+1][0]
		}
		if end-match[1] > consoleAPIKeyScanDistance {
			end = match[1] + consoleAPIKeyScanDistance
		}
		// Console serializes disabled after apiKeyId in each key object, so the
		// span up to the next key belongs to this key.
		if consoleDisabledPattern.MatchString(text[match[1]:end]) {
			continue
		}
		result.apiKeyID = strings.ToLower(text[match[2]:match[3]])
		break
	}
	if match := consoleTeamIDPattern.FindStringSubmatch(text); match != nil {
		result.teamID = strings.ToLower(match[1])
	} else if match := consoleTeamPathPattern.FindStringSubmatch(text); match != nil {
		result.teamID = strings.ToLower(match[1])
	}
	return result
}

func (a *Adapter) createConsoleAPIKey(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease, teamID string) error {
	endpoint := consoleOrigin(a.config().BaseURL) + consoleCreateAPIKeyPath
	response, err := a.doDPoPRequestWithOptions(ctx, credential, ssoToken, lease, http.MethodPost, endpoint, consoleCreateAPIKeyBody(teamID), dpopRequestOptions{
		contentType: "application/connect+proto",
		accept:      "*/*",
		referer:     consoleOrigin(a.config().BaseURL) + "/team/" + url.PathEscape(teamID) + "/chat",
		skipAPIKey:  true,
	})
	if err != nil {
		return err
	}
	data, _, _ := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusForbidden && shouldInvalidateConsoleClearance(data) {
			lease.InvalidateClearance()
		}
		return fmt.Errorf("Console CreateApiKey 返回 %d", response.StatusCode)
	}
	a.logger().Info("console_api_key_created", "account_id", credential.ID)
	return nil
}

// consoleCreateAPIKeyBody builds the Connect envelope (flag byte + big-endian
// length) around CreateApiKeyRequest{name=1, team_id=2, acls=3...}.
func consoleCreateAPIKeyBody(teamID string) []byte {
	var payload []byte
	payload = protowire.AppendTag(payload, 1, protowire.BytesType)
	payload = protowire.AppendString(payload, consoleAPIKeyName)
	payload = protowire.AppendTag(payload, 2, protowire.BytesType)
	payload = protowire.AppendString(payload, teamID)
	for _, acl := range []string{consoleAPIKeyACLEndpoint, consoleAPIKeyACLModel} {
		payload = protowire.AppendTag(payload, 3, protowire.BytesType)
		payload = protowire.AppendString(payload, acl)
	}
	envelope := make([]byte, 5, 5+len(payload))
	length := len(payload)
	envelope[1], envelope[2], envelope[3], envelope[4] = byte(length>>24), byte(length>>16), byte(length>>8), byte(length)
	return append(envelope, payload...)
}

// isConsoleMissingAPIKeyBody reports the 429 Console returns when x-apikey is
// absent or no longer belongs to the team. It is repairable, not a rate limit.
func isConsoleMissingAPIKeyBody(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "provision an api key") || strings.Contains(text, "not the owner")
}

func consoleOrigin(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return baseURL
}
