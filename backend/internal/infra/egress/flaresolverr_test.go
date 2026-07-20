package egress

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFlareSolverrSolveUsesNodeProxyAndFiltersCookies(t *testing.T) {
	var requestPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1" || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&requestPayload); err != nil {
			t.Fatal(err)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"status":"ok","solution":{"userAgent":"Mozilla/5.0 Chrome/146.0.0.0 Safari/537.36","cookies":[{"name":"cf_clearance","value":"clear"},{"name":"sso","value":"secret"},{"name":"__cf_bm","value":"bm"}]}}`))
	}))
	defer server.Close()

	solution, err := (flaresolverrSolver{}).Solve(context.Background(), ClearanceConfig{
		FlareSolverrURL: server.URL, TargetURL: "https://grok.com", Timeout: time.Second,
	}, "socks5h://proxy:1080")
	if err != nil {
		t.Fatal(err)
	}
	if solution.Cookies != "cf_clearance=clear; __cf_bm=bm" || solution.UserAgent == "" {
		t.Fatalf("solution = %#v", solution)
	}
	if requestPayload["cmd"] != "request.get" || requestPayload["url"] != "https://grok.com" {
		t.Fatalf("payload = %#v", requestPayload)
	}
	proxy, ok := requestPayload["proxy"].(map[string]any)
	if !ok || proxy["url"] != "socks5h://proxy:1080" {
		t.Fatalf("proxy payload = %#v", requestPayload["proxy"])
	}
}

func TestFlareSolverrEndpointAcceptsBaseAndV1Path(t *testing.T) {
	for input, expected := range map[string]string{
		"http://flaresolverr:8191":   "http://flaresolverr:8191/v1",
		"http://flaresolverr:8191/":  "http://flaresolverr:8191/v1",
		"https://solver.example/api": "https://solver.example/api/v1",
	} {
		actual, err := flaresolverrEndpoint(input)
		if err != nil || actual != expected {
			t.Fatalf("endpoint(%q) = %q, %v", input, actual, err)
		}
	}
}

func TestSanitizeFlareSolverrMessageRedactsCredentials(t *testing.T) {
	message := sanitizeFlareSolverrMessage("proxy socks5h://user:secret@resin:2260 failed; token=abc123 Authorization: Bearer.SECRET cookie=sso-value")
	for _, secret := range []string{"user", "secret", "abc123", "Bearer.SECRET", "sso-value"} {
		if strings.Contains(message, secret) {
			t.Fatalf("sanitized message leaked %q: %q", secret, message)
		}
	}
	if !strings.Contains(message, "socks5h://***:***@") {
		t.Fatalf("proxy scheme was not retained safely: %q", message)
	}
}
