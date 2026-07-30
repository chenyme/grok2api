package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	defaultOAuthClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	defaultOAuthScope    = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write workspaces:read workspaces:write"
	defaultDeviceURL     = "https://auth.x.ai/oauth2/device/code"
	defaultTokenURL      = "https://auth.x.ai/oauth2/token"
	deviceClientSurface  = "ui"
)

var (
	oauthBearerPattern        = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)
	oauthJWTPattern           = regexp.MustCompile(`[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)
	oauthSensitivePairPattern = regexp.MustCompile(`(?i)(access_token|refresh_token|id_token|token|authorization|cookie|secret|password|credential)=([^&\s"'<>]+)`)
)

type oauthClient struct {
	http      *http.Client
	clientID  string
	scope     string
	deviceURL string
	tokenURL  string
	version   func() string
}

func newOAuthClient(httpClient *http.Client, version func() string) *oauthClient {
	return &oauthClient{http: httpClient, clientID: defaultOAuthClientID, scope: defaultOAuthScope, deviceURL: defaultDeviceURL, tokenURL: defaultTokenURL, version: version}
}

func (c *oauthClient) startDevice(ctx context.Context) (provider.DeviceAuthorization, error) {
	form := url.Values{"client_id": {c.clientID}, "scope": {c.scope}, "referrer": {"grok-build"}}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := c.postForm(ctx, c.deviceURL, form, &payload, true); err != nil {
		return provider.DeviceAuthorization{}, err
	}
	if payload.DeviceCode == "" || payload.UserCode == "" || payload.VerificationURI == "" {
		return provider.DeviceAuthorization{}, fmt.Errorf("xAI Device OAuth 返回字段不完整")
	}
	if payload.Interval <= 0 {
		payload.Interval = 5
	}
	if payload.ExpiresIn <= 0 {
		payload.ExpiresIn = 1800
	}
	return provider.DeviceAuthorization{DeviceCode: payload.DeviceCode, UserCode: payload.UserCode, VerificationURI: payload.VerificationURI, VerificationURIComplete: payload.VerificationURIComplete, Interval: time.Duration(payload.Interval) * time.Second, ExpiresIn: time.Duration(payload.ExpiresIn) * time.Second}, nil
}

func (c *oauthClient) pollDevice(ctx context.Context, deviceCode string) (tokenPayload, error) {
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {c.clientID}, "device_code": {deviceCode}}
	return c.exchange(ctx, form, "", true)
}

func (c *oauthClient) refresh(ctx context.Context, refreshToken string) (tokenPayload, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {c.clientID}, "refresh_token": {refreshToken}}
	value, err := c.exchange(ctx, form, refreshToken, false)
	if errors.Is(err, provider.ErrAuthorizationDenied) {
		return tokenPayload{}, &provider.CredentialRefreshError{Code: "refresh_denied", Message: "OAuth authorization was denied", Permanent: true, Cause: err}
	}
	return value, err
}

type tokenPayload struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	IDToken      string
}

func (c *oauthClient) exchange(ctx context.Context, form url.Values, fallbackRefresh string, deviceFlow bool) (tokenPayload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenPayload{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if deviceFlow {
		c.applyDeviceHeaders(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return tokenPayload{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenPayload{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		oauthError := parseOAuthErrorResponse(body, resp.StatusCode)
		switch oauthError.Code {
		case "authorization_pending":
			if deviceFlow {
				return tokenPayload{}, provider.ErrAuthorizationPending
			}
		case "slow_down":
			if deviceFlow {
				return tokenPayload{}, provider.ErrSlowDown
			}
		case "access_denied", "expired_token":
			if deviceFlow {
				return tokenPayload{}, provider.ErrAuthorizationDenied
			}
		}
		return tokenPayload{}, &provider.CredentialRefreshError{
			Status: resp.StatusCode, Code: oauthError.Code, Message: oauthError.Message, Response: oauthError.Response,
			Permanent:  resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized,
			RetryAfter: parseOAuthRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	var value struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return tokenPayload{}, fmt.Errorf("解析 xAI OAuth 响应: %w", err)
	}
	if value.AccessToken == "" {
		return tokenPayload{}, &provider.CredentialRefreshError{Status: resp.StatusCode, Code: "missing_access_token", Message: "OAuth response did not contain access_token", Permanent: true}
	}
	if value.ExpiresIn <= 0 {
		value.ExpiresIn = 3600
	}
	return tokenPayload{AccessToken: value.AccessToken, RefreshToken: firstNonEmpty(value.RefreshToken, fallbackRefresh), ExpiresAt: time.Now().UTC().Add(time.Duration(value.ExpiresIn) * time.Second), IDToken: value.IDToken}, nil
}

type oauthErrorDetails struct {
	Code     string
	Message  string
	Response string
}

func parseOAuthErrorResponse(body []byte, status int) oauthErrorDetails {
	result := oauthErrorDetails{Code: "oauth_http_" + strconv.Itoa(status)}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		redacted := redactOAuthDiagnosticText(string(body))
		result.Message = normalizeOAuthErrorMessage(redacted, 512)
		result.Response = normalizeOAuthErrorMessage(redacted, 4096)
		return result
	}

	result.Code = firstNonEmpty(
		jsonStringAt(payload, "error"),
		jsonStringAt(payload, "error", "code"),
		jsonStringAt(payload, "error_code"),
		jsonStringAt(payload, "code"),
		result.Code,
	)
	rootMessage, _ := payload.(string)
	messages := []string{
		rootMessage,
		jsonStringAt(payload, "error_description"),
		jsonStringAt(payload, "message"),
		jsonStringAt(payload, "detail"),
		jsonStringAt(payload, "description"),
		jsonStringAt(payload, "title"),
		jsonStringAt(payload, "error", "message"),
		jsonStringAt(payload, "error", "error_description"),
		jsonStringAt(payload, "error", "detail"),
		jsonStringAt(payload, "error", "description"),
	}
	seen := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		message = normalizeOAuthErrorMessage(message, 512)
		if message == "" {
			continue
		}
		if _, exists := seen[message]; exists {
			continue
		}
		seen[message] = struct{}{}
		if result.Message != "" {
			result.Message += " · "
		}
		result.Message += message
	}
	redacted, err := json.Marshal(redactOAuthDiagnosticValue("", payload))
	if err == nil {
		result.Response = truncateOAuthDiagnostic(string(redacted), 4096)
	}
	return result
}

func jsonStringAt(value any, path ...string) string {
	current := value
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = object[key]
		if !ok {
			return ""
		}
	}
	text, _ := current.(string)
	return text
}

func redactOAuthDiagnosticValue(key string, value any) any {
	if isSensitiveOAuthDiagnosticKey(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for childKey, child := range typed {
			result[childKey] = redactOAuthDiagnosticValue(childKey, child)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = redactOAuthDiagnosticValue(key, child)
		}
		return result
	case string:
		return redactOAuthDiagnosticText(typed)
	default:
		return value
	}
}

func isSensitiveOAuthDiagnosticKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	for _, part := range []string{"token", "authorization", "cookie", "secret", "password", "credential", "assertion", "code_verifier", "device_code"} {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	return false
}

func redactOAuthDiagnosticText(value string) string {
	value = oauthBearerPattern.ReplaceAllString(value, "Bearer [REDACTED]")
	value = oauthJWTPattern.ReplaceAllString(value, "[REDACTED]")
	value = oauthSensitivePairPattern.ReplaceAllString(value, "$1=[REDACTED]")
	fields := strings.Fields(value)
	for index := range fields {
		trimmed := strings.Trim(fields[index], `"'(),;`)
		if len(trimmed) >= 80 && !strings.ContainsAny(trimmed, "{}[]<>") {
			fields[index] = strings.Replace(fields[index], trimmed, "[REDACTED]", 1)
		}
	}
	return strings.Join(fields, " ")
}

func normalizeOAuthErrorMessage(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	return truncateOAuthDiagnostic(value, limit)
}

func truncateOAuthDiagnostic(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		if limit <= 1 {
			return "…"
		}
		return string(runes[:limit-1]) + "…"
	}
	return value
}

func parseOAuthRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(time.Now()) {
		return time.Until(parsed)
	}
	return 0
}

func (c *oauthClient) postForm(ctx context.Context, endpoint string, form url.Values, output any, deviceFlow bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if deviceFlow {
		c.applyDeviceHeaders(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("xAI OAuth 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, output)
}

func (c *oauthClient) applyDeviceHeaders(req *http.Request) {
	if c.version != nil {
		if version := strings.TrimSpace(c.version()); version != "" {
			req.Header.Set("x-grok-client-version", version)
		}
	}
	// The management UI presents the verification URL and code to a human.
	req.Header.Set("x-grok-client-surface", deviceClientSurface)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
