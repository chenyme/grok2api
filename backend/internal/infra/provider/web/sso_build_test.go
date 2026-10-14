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
	status, finalURL, body, err := flow.do(context.Background(), http.MethodGet, ssoDeviceURL, nil)
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
	if _, _, _, err := flow.do(context.Background(), http.MethodGet, ssoDeviceURL, nil); err == nil {
		t.Fatal("unsafe redirect was accepted")
	}
}

func TestSSOBuildFlowMapsDeadSSOToUnauthorized(t *testing.T) {
	client := &scriptedSSOClient{responses: []*http.Response{
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"device_code":"dc","user_code":"uc","interval":1,"expires_in":1800}`))},
		{StatusCode: http.StatusSeeOther, Header: http.Header{"Location": []string{"https://accounts.x.ai/sign-in"}}, Body: io.NopCloser(strings.NewReader(""))},
	}}
	flow := &ssoBuildFlow{client: client, userAgent: "lease-agent", cookies: map[string]string{"sso": "dead"}}
	_, err := flow.convert(context.Background(), fakeCredential())
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (device, verify)", len(client.requests))
	}
}

func TestSSOBuildFlowConvertsEndToEndWithConsentToken(t *testing.T) {
	client := &scriptedSSOClient{responses: []*http.Response{
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"device_code":"dc","user_code":"uc","interval":1,"expires_in":1800}`))},
		{StatusCode: http.StatusSeeOther, Header: http.Header{"Location": []string{"https://accounts.x.ai/oauth2/device/consent"}}, Body: io.NopCloser(strings.NewReader(""))},
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`<form method="post"><input value="eyJhbGciOiJFUzI1NiJ9.eyJkYyI6ImRjIn0.sig" type="hidden" name="consent_token"></form>`))},
		{StatusCode: http.StatusSeeOther, Header: http.Header{"Location": []string{"https://accounts.x.ai/oauth2/device/done"}}, Body: io.NopCloser(strings.NewReader(""))},
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"access_token":"access","refresh_token":"refresh","expires_in":3600}`))},
	}}
	flow := &ssoBuildFlow{client: client, userAgent: "lease-agent", cookies: ssoBuildCookies("live", "cf_clearance=clearance")}
	seed, err := flow.convert(context.Background(), fakeCredential())
	if err != nil {
		t.Fatal(err)
	}
	if seed.AccessToken != "access" || seed.RefreshToken != "refresh" || len(client.requests) != 5 {
		t.Fatalf("seed=%#v requests=%d", seed, len(client.requests))
	}
	hosts := []string{"auth.x.ai", "auth.x.ai", "accounts.x.ai", "auth.x.ai", "auth.x.ai"}
	for index, request := range client.requests {
		if request.URL.Hostname() != hosts[index] {
			t.Fatalf("device flow visited unexpected host %q", request.URL.Hostname())
		}
	}
	if client.requests[3].URL.RawQuery != "" {
		t.Fatalf("approve must post a form body, got query %q", client.requests[3].URL.RawQuery)
	}
	approve := client.requests[3]
	if approve.Method != http.MethodPost {
		t.Fatalf("approve method = %q, want POST", approve.Method)
	}
	wantHeaders := map[string]string{
		"Origin":                    "https://accounts.x.ai",
		"Referer":                   "https://accounts.x.ai/oauth2/device/consent",
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "same-site",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
	}
	for name, want := range wantHeaders {
		if got := approve.Header.Get(name); got != want {
			t.Errorf("approve %s = %q, want %q", name, got, want)
		}
	}
	consentCookie := client.requests[2].Header.Get("Cookie")
	if !strings.Contains(consentCookie, "sso=live") || !strings.Contains(consentCookie, "cf_clearance=clearance") {
		t.Fatalf("consent request cookies = %q", consentCookie)
	}
	if client.requests[2].Header.Get("Sec-Fetch-Mode") != "navigate" || client.requests[2].Header.Get("Sec-Fetch-Dest") != "document" {
		t.Fatalf("consent request is missing navigation headers: %#v", client.requests[2].Header)
	}
	body, err := io.ReadAll(client.requests[3].Body)
	if err != nil {
		t.Fatal(err)
	}
	approveForm, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatal(err)
	}
	if approveForm.Get("consent_token") != "eyJhbGciOiJFUzI1NiJ9.eyJkYyI6ImRjIn0.sig" {
		t.Fatalf("approve form missing consent_token: %q", approveForm.Get("consent_token"))
	}
}

func TestSSOBuildFlowVerifyDoesNotFollowRedirect(t *testing.T) {
	client := &scriptedSSOClient{responses: []*http.Response{
		{StatusCode: http.StatusSeeOther, Header: http.Header{"Location": []string{"https://accounts.x.ai/oauth2/device/consent"}}, Body: io.NopCloser(strings.NewReader("verify response"))},
	}}
	flow := &ssoBuildFlow{client: client, userAgent: "lease-agent", cookies: map[string]string{"sso": "live"}}
	status, finalURL, body, err := flow.doWithFollow(context.Background(), http.MethodPost, ssoVerifyURL, url.Values{"user_code": {"uc"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", status)
	}
	if finalURL != "https://accounts.x.ai/oauth2/device/consent" {
		t.Fatalf("finalURL = %q", finalURL)
	}
	if len(client.requests) != 1 {
		t.Fatalf("redirect must not be followed, requests = %d", len(client.requests))
	}
	if string(body) != "verify response" {
		t.Fatalf("redirect response body = %q", body)
	}
}

func TestSSODeviceRedirectStateRequiresExactTrustedPath(t *testing.T) {
	tests := map[string]string{
		"https://accounts.x.ai/oauth2/device/consent":           "consent",
		"https://accounts.x.ai/oauth2/device/done/":             "done",
		"https://accounts.x.ai/sign-in?returnTo=%2Foauth2":      "sign-in",
		"https://accounts.x.ai/other?next=consent":              "",
		"https://accounts.x.ai/oauth2/device/consent-untrusted": "",
		"https://example.com/oauth2/device/consent":             "",
	}
	for raw, wanted := range tests {
		if actual := ssoDeviceRedirectState(raw); actual != wanted {
			t.Fatalf("ssoDeviceRedirectState(%q)=%q want=%q", raw, actual, wanted)
		}
	}
}

func fakeCredential() accountdomain.Credential { return accountdomain.Credential{} }

func TestExtractConsentToken(t *testing.T) {
	form := []byte(`<input value="eyJhbGciOiJFUzI1NiJ9.a.b" type="hidden" name="consent_token">`)
	if token := extractConsentToken(form); token != "eyJhbGciOiJFUzI1NiJ9.a.b" {
		t.Fatalf("form token = %q", token)
	}
	jsonPayload := []byte(`{"consentToken":"eyJhbGciOiJFUzI1NiJ9.c.d","other":1}`)
	if token := extractConsentToken(jsonPayload); token != "eyJhbGciOiJFUzI1NiJ9.c.d" {
		t.Fatalf("json token = %q", token)
	}
	rscPayload := []byte(`{\"consentToken\":\"eyJhbGciOiJFUzI1NiJ9.e.f\"}`)
	if token := extractConsentToken(rscPayload); token != "eyJhbGciOiJFUzI1NiJ9.e.f" {
		t.Fatalf("escaped RSC token = %q", token)
	}
	if token := extractConsentToken([]byte("<html>nothing</html>")); token != "" {
		t.Fatalf("unexpected token = %q", token)
	}
}

func TestConsentTokenMustMatchDeviceCode(t *testing.T) {
	token := "eyJhbGciOiJFUzI1NiJ9.eyJkYyI6ImRjIn0.sig"
	if !consentTokenMatchesDevice(token, "dc") {
		t.Fatal("consent token for current device code was rejected")
	}
	if consentTokenMatchesDevice(token, "other-device") {
		t.Fatal("consent token for a different device code was accepted")
	}
	if consentTokenMatchesDevice("not-a-jwt", "dc") {
		t.Fatal("malformed consent token was accepted")
	}
}

func TestSSOBuildCookiesCarrySanitizedCloudflareClearance(t *testing.T) {
	cookies := ssoBuildCookies("sso-token", "cf_clearance=clearance; __cf_bm=bm; sso=attacker")
	if cookies["sso"] != "sso-token" || cookies["sso-rw"] != "sso-token" {
		t.Fatalf("SSO cookies = %#v", cookies)
	}
	if cookies["cf_clearance"] != "clearance" || cookies["__cf_bm"] != "bm" {
		t.Fatalf("Cloudflare cookies = %#v", cookies)
	}
}

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
