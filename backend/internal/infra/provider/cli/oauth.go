package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	defaultOAuthClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	defaultOAuthScope    = "openid profile email offline_access grok-cli:access api:access"
	defaultDeviceURL     = "https://auth.x.ai/oauth2/device/code"
	defaultTokenURL      = "https://auth.x.ai/oauth2/token"
)

type oauthClient struct {
	http      *http.Client
	clientID  string
	scope     string
	deviceURL string
	tokenURL  string
}

func newOAuthClient(httpClient *http.Client) *oauthClient {
	return &oauthClient{http: httpClient, clientID: defaultOAuthClientID, scope: defaultOAuthScope, deviceURL: defaultDeviceURL, tokenURL: defaultTokenURL}
}

func (c *oauthClient) startDevice(ctx context.Context) (provider.DeviceAuthorization, error) {
	form := url.Values{"client_id": {c.clientID}, "scope": {c.scope}}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := c.postForm(ctx, c.deviceURL, form, &payload); err != nil {
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
	return c.exchange(ctx, form, "")
}

func (c *oauthClient) refresh(ctx context.Context, refreshToken string) (tokenPayload, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {c.clientID}, "refresh_token": {refreshToken}}
	value, err := c.exchange(ctx, form, refreshToken)
	if errors.Is(err, provider.ErrAuthorizationDenied) {
		return tokenPayload{}, &provider.CredentialRefreshError{Code: "refresh_denied", Permanent: true, Cause: err}
	}
	return value, err
}

type tokenPayload struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	IDToken      string
}

func (c *oauthClient) exchange(ctx context.Context, form url.Values, fallbackRefresh string) (tokenPayload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenPayload{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return tokenPayload{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenPayload{}, err
	}
	var value struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		IDToken          string `json:"id_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return tokenPayload{}, fmt.Errorf("解析 xAI OAuth 响应: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch value.Error {
		case "authorization_pending":
			return tokenPayload{}, provider.ErrAuthorizationPending
		case "slow_down":
			return tokenPayload{}, provider.ErrSlowDown
		case "access_denied", "expired_token":
			return tokenPayload{}, provider.ErrAuthorizationDenied
		default:
			return tokenPayload{}, &provider.CredentialRefreshError{
				Status: resp.StatusCode, Code: firstNonEmpty(value.Error, "oauth_http_"+strconv.Itoa(resp.StatusCode)),
				Permanent:  resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized,
				RetryAfter: parseOAuthRetryAfter(resp.Header.Get("Retry-After")),
			}
		}
	}
	if value.AccessToken == "" {
		return tokenPayload{}, &provider.CredentialRefreshError{Status: resp.StatusCode, Code: "missing_access_token", Permanent: true}
	}
	if value.ExpiresIn <= 0 {
		value.ExpiresIn = 3600
	}
	return tokenPayload{AccessToken: value.AccessToken, RefreshToken: firstNonEmpty(value.RefreshToken, fallbackRefresh), ExpiresAt: time.Now().UTC().Add(time.Duration(value.ExpiresIn) * time.Second), IDToken: value.IDToken}, nil
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

func (c *oauthClient) postForm(ctx context.Context, endpoint string, form url.Values, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
