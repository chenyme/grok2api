package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type scriptedSSOClient struct {
	responses []*http.Response
	requests  []*http.Request
}

func (c *scriptedSSOClient) Do(request *http.Request) (*http.Response, error) {
	c.requests = append(c.requests, request)
	response := c.responses[0]
	c.responses = c.responses[1:]
	return response, nil
}

func TestSSOBuildFlowFollowsOnlyTrustedXAIHTTPSRedirects(t *testing.T) {
	client := &scriptedSSOClient{responses: []*http.Response{
		{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://auth.x.ai/next"}, "Set-Cookie": []string{"session=abc; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader(""))},
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))},
	}}
	flow := &ssoBuildFlow{client: client, userAgent: "test-agent", cookies: map[string]string{"sso": "secret"}}
	status, finalURL, body, err := flow.do(context.Background(), http.MethodGet, ssoAccountsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || finalURL != "https://auth.x.ai/next" || string(body) != "ok" {
		t.Fatalf("response = %d %s %q", status, finalURL, body)
	}
	if len(client.requests) != 2 || client.requests[1].Header.Get("User-Agent") != "test-agent" {
		t.Fatalf("requests = %#v", client.requests)
	}
	cookie := client.requests[1].Header.Get("Cookie")
	if !strings.Contains(cookie, "sso=secret") || !strings.Contains(cookie, "session=abc") {
		t.Fatalf("redirect cookies = %q", cookie)
	}

	unsafe := &scriptedSSOClient{responses: []*http.Response{{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://example.com/steal"}}, Body: io.NopCloser(strings.NewReader(""))}}}
	flow = &ssoBuildFlow{client: unsafe, userAgent: "test-agent", cookies: map[string]string{"sso": "secret"}}
	if _, _, _, err := flow.do(context.Background(), http.MethodGet, ssoAccountsURL, nil); err == nil {
		t.Fatal("unsafe redirect was accepted")
	}
}

func TestMergeCookieHeader(t *testing.T) {
	cookies := map[string]string{"sso": "secret"}
	mergeCookieHeader(cookies, "cf_clearance=abc.123; __cuid=id=with-equals; bad\x00name=x; =novalue; long="+strings.Repeat("a", 16385)+"; sso=hijack")
	if cookies["cf_clearance"] != "abc.123" {
		t.Fatalf("cf_clearance = %q", cookies["cf_clearance"])
	}
	if cookies["__cuid"] != "id=with-equals" {
		t.Fatalf("__cuid = %q", cookies["__cuid"])
	}
	if _, ok := cookies["bad\x00name"]; ok {
		t.Fatal("cookie with NUL in name was accepted")
	}
	if _, ok := cookies[""]; ok {
		t.Fatal("cookie with empty name was accepted")
	}
	if _, ok := cookies["long"]; ok {
		t.Fatal("oversized cookie value was accepted")
	}
	if cookies["sso"] != "hijack" {
		t.Fatalf("existing cookie should be overridden, got %q", cookies["sso"])
	}
}

// blockedPrecheckScript 模拟 accounts.x.ai 预检被 Cloudflare 403 拦截，
// 随后 Device Flow 启动返回 500 使流程在第 2 步终止。
func blockedPrecheckScript() *scriptedSSOClient {
	return &scriptedSSOClient{responses: []*http.Response{
		{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("challenge"))},
		{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("boom"))},
	}}
}

func TestSSOBuildFlowAttachesClearanceToFirstRequest(t *testing.T) {
	client := blockedPrecheckScript()
	flow := &ssoBuildFlow{
		client: client, userAgent: "lease-agent",
		cookies:   map[string]string{"sso": "secret"},
		clearance: func(context.Context) (string, string, error) {
			return "cf_clearance=abc.123; __cf_bm=xyz", "solver-agent", nil
		},
	}
	_, err := flow.convert(context.Background(), fakeCredential())
	if err == nil {
		t.Fatal("expected conversion to fail after scripted 500")
	}
	if len(client.requests) == 0 {
		t.Fatal("no request was issued")
	}
	first := client.requests[0]
	if ua := first.Header.Get("User-Agent"); ua != "solver-agent" {
		t.Fatalf("User-Agent = %q, want solver-agent", ua)
	}
	cookie := first.Header.Get("Cookie")
	if !strings.Contains(cookie, "sso=secret") || !strings.Contains(cookie, "cf_clearance=abc.123") || !strings.Contains(cookie, "__cf_bm=xyz") {
		t.Fatalf("Cookie = %q", cookie)
	}
	if len(client.requests) != 2 {
		t.Fatalf("blocked precheck must be degraded, requests = %d", len(client.requests))
	}
}

func TestSSOBuildFlowClearanceFailureFallsBack(t *testing.T) {
	client := blockedPrecheckScript()
	flow := &ssoBuildFlow{
		client: client, userAgent: "lease-agent",
		cookies:   map[string]string{"sso": "secret"},
		clearance: func(context.Context) (string, string, error) {
			return "", "", io.ErrUnexpectedEOF
		},
	}
	_, _ = flow.convert(context.Background(), fakeCredential())
	if len(client.requests) == 0 {
		t.Fatal("no request was issued")
	}
	first := client.requests[0]
	if ua := first.Header.Get("User-Agent"); ua != "lease-agent" {
		t.Fatalf("User-Agent = %q, want lease-agent", ua)
	}
	cookie := first.Header.Get("Cookie")
	if strings.Contains(cookie, "cf_clearance") || !strings.Contains(cookie, "sso=secret") {
		t.Fatalf("Cookie = %q", cookie)
	}
}

func TestSSOBuildFlowMapsDeadSSOToUnauthorized(t *testing.T) {
	client := &scriptedSSOClient{responses: []*http.Response{
		{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("challenge"))},
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"device_code":"dc","user_code":"uc","verification_uri_complete":"https://accounts.x.ai/oauth2/device?code=abc","interval":1,"expires_in":1800}`))},
		{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("challenge"))},
		{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://accounts.x.ai/sign-in"}}, Body: io.NopCloser(strings.NewReader(""))},
	}}
	flow := &ssoBuildFlow{client: client, userAgent: "lease-agent", cookies: map[string]string{"sso": "dead"}}
	_, err := flow.convert(context.Background(), fakeCredential())
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if len(client.requests) != 4 {
		t.Fatalf("requests = %d, want 4 (precheck, device, verify page, verify 不跟随重定向)", len(client.requests))
	}
}

func TestSSOBuildFlowVerifyDoesNotFollowRedirect(t *testing.T) {
	client := &scriptedSSOClient{responses: []*http.Response{
		{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://accounts.x.ai/oauth2/device?code=abc&consent=1"}}, Body: io.NopCloser(strings.NewReader(""))},
	}}
	flow := &ssoBuildFlow{client: client, userAgent: "lease-agent", cookies: map[string]string{"sso": "live"}}
	status, finalURL, _, err := flow.doWithFollow(context.Background(), http.MethodPost, ssoVerifyURL, url.Values{"user_code": {"uc"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusFound {
		t.Fatalf("status = %d, want 302", status)
	}
	if finalURL != "https://accounts.x.ai/oauth2/device?code=abc&consent=1" {
		t.Fatalf("finalURL = %q", finalURL)
	}
	if len(client.requests) != 1 {
		t.Fatalf("redirect must not be followed, requests = %d", len(client.requests))
	}
}

func fakeCredential() accountdomain.Credential { return accountdomain.Credential{} }

func TestSSOBuildConversionSanitizesTokenAndURLs(t *testing.T) {
	if token := normalizeSSOToken("sso=token-value; x-userid=drop"); token != "token-value" {
		t.Fatalf("token = %q", token)
	}
	for _, value := range []string{"https://accounts.x.ai/", "https://auth.x.ai/oauth2/device/code"} {
		if !safeXAIURL(value) {
			t.Fatalf("trusted URL rejected: %s", value)
		}
	}
	for _, value := range []string{"http://auth.x.ai/", "https://x.ai.example.com/", "https://user@auth.x.ai/"} {
		if safeXAIURL(value) {
			t.Fatalf("unsafe URL accepted: %s", value)
		}
	}
}
