package web

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	application "github.com/chenyme/grok2api/backend/internal/application/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

var forbiddenBrowserIdentityFields = []string{
	"grok_device_id", "x-anonuserid", "x-userid", "x-challenge", "x-signature",
}

func TestCloudflareCookieWhitelistDropsBrowserIdentityFields(t *testing.T) {
	raw := "cf_clearance=clear; __cf_bm=bm; _cfuvid=uv; cf_chl_2=challenge; grok_device_id=device; x-anonuserid=anon; x-userid=user; x-challenge=c; x-signature=s; unrelated=value"
	sanitized := application.SanitizeCloudflareCookies(raw)
	for _, expected := range []string{"cf_clearance=clear", "__cf_bm=bm", "_cfuvid=uv", "cf_chl_2=challenge"} {
		if !strings.Contains(sanitized, expected) {
			t.Fatalf("sanitized cookies missing %q: %s", expected, sanitized)
		}
	}
	assertForbiddenFieldsAbsent(t, sanitized)
}

func TestWebHeadersOnlyUseSSOAndCloudflareCookies(t *testing.T) {
	lease := &infraegress.Lease{UserAgent: "test-agent", CFCookies: "cf_clearance=clear; x-userid=drop"}
	headers := buildHeaders("sso=token-value; x-userid=drop; x-signature=drop", lease, "application/json")
	cookie := headers.Get("Cookie")
	if !strings.Contains(cookie, "sso=token-value") || !strings.Contains(cookie, "sso-rw=token-value") || !strings.Contains(cookie, "cf_clearance=clear") {
		t.Fatalf("cookie header = %q", cookie)
	}
	assertForbiddenFieldsAbsent(t, cookie)
	for name := range headers {
		assertForbiddenFieldsAbsent(t, name)
	}
	for _, name := range []string{"Accept", "Accept-Language", "x-xai-request-id"} {
		if headers.Get(name) == "" {
			t.Fatalf("missing header %s", name)
		}
	}
	if value := headers.Get("x-statsig-id"); value != "" {
		t.Fatalf("unsigned base headers must not contain x-statsig-id: %q", value)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(headers.Get("x-xai-request-id")) {
		t.Fatalf("x-xai-request-id = %q", headers.Get("x-xai-request-id"))
	}
	for name := range headers {
		if strings.HasPrefix(strings.ToLower(name), "sec-ch-ua") {
			t.Fatalf("synthetic client hint must not be sent: %s", name)
		}
	}
}

func TestAppHeadersMatchStableBrowserFetchSignals(t *testing.T) {
	headers := http.Header{}
	applyAppHeaders(headers, "https://grok.com", "https://grok.com/")
	for name, expected := range map[string]string{
		"Origin": "https://grok.com", "Referer": "https://grok.com/", "Cache-Control": "no-cache",
		"Pragma": "no-cache", "Priority": "u=1, i", "Sec-Fetch-Dest": "empty",
		"Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin",
	} {
		if headers.Get(name) != expected {
			t.Fatalf("%s = %q", name, headers.Get(name))
		}
	}
}

func assertForbiddenFieldsAbsent(t *testing.T, value string) {
	t.Helper()
	lower := strings.ToLower(value)
	for _, forbidden := range forbiddenBrowserIdentityFields {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("forbidden browser identity field %q found in %q", forbidden, value)
		}
	}
}
