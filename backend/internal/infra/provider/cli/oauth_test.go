package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestOAuthDeviceFlowMatchesOfficialWireContract(t *testing.T) {
	version := "0.2.111"
	requests := 0
	tokenPolls := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Header.Get("x-grok-client-version") != version {
			t.Fatalf("client version = %q, want %q", request.Header.Get("x-grok-client-version"), version)
		}
		if request.Header.Get("x-grok-client-surface") != deviceClientSurface {
			t.Fatalf("client surface = %q", request.Header.Get("x-grok-client-surface"))
		}
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch request.URL.Path {
		case "/oauth2/device/code":
			if request.Form.Get("client_id") != defaultOAuthClientID || request.Form.Get("scope") != defaultOAuthScope || request.Form.Get("referrer") != "grok-build" {
				t.Fatalf("device form = %v", request.Form)
			}
			return oauthResponse(http.StatusOK, `{"device_code":"device","user_code":"ABCD-EFGH","verification_uri":"https://auth.x.ai/activate","verification_uri_complete":"https://auth.x.ai/activate?user_code=ABCD-EFGH","interval":5,"expires_in":1800}`), nil
		case "/oauth2/token":
			if request.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || request.Form.Get("client_id") != defaultOAuthClientID || request.Form.Get("device_code") != "device" {
				t.Fatalf("token form = %v", request.Form)
			}
			tokenPolls++
			if tokenPolls == 1 {
				return oauthResponse(http.StatusBadRequest, `{"error":"authorization_pending"}`), nil
			}
			return oauthResponse(http.StatusOK, `{"access_token":"access","refresh_token":"refresh","id_token":"id","expires_in":3600}`), nil
		default:
			t.Fatalf("unexpected OAuth path %q", request.URL.Path)
			return nil, nil
		}
	})}
	client := newOAuthClient(httpClient, func() string { return version })

	authorization, err := client.startDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if authorization.DeviceCode != "device" || authorization.UserCode != "ABCD-EFGH" || authorization.Interval != 5*time.Second || authorization.ExpiresIn != 30*time.Minute {
		t.Fatalf("authorization = %#v", authorization)
	}

	version = "0.2.112"
	if _, err := client.pollDevice(context.Background(), authorization.DeviceCode); !errors.Is(err, provider.ErrAuthorizationPending) {
		t.Fatalf("poll error = %v", err)
	}
	tokens, err := client.pollDevice(context.Background(), authorization.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "access" || tokens.RefreshToken != "refresh" || tokens.IDToken != "id" || time.Until(tokens.ExpiresAt) < 59*time.Minute {
		t.Fatalf("tokens = %#v", tokens)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
}

func oauthResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestOAuthRefreshClassifiesPermanentAndTransientFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		permanent  bool
		code       string
	}{
		{name: "transient upstream", status: http.StatusServiceUnavailable, body: `{"error":"temporarily_unavailable"}`, retryAfter: "7", code: "temporarily_unavailable"},
		{name: "invalid grant", status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`, permanent: true, code: "invalid_grant"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("x-grok-client-version") != "" || request.Header.Get("x-grok-client-surface") != "" {
					t.Fatalf("refresh request unexpectedly included device headers: %v", request.Header)
				}
				if request.FormValue("grant_type") != "refresh_token" || request.FormValue("refresh_token") != "refresh" {
					t.Fatalf("form = %#v", request.Form)
				}
				header := make(http.Header)
				if test.retryAfter != "" {
					header.Set("Retry-After", test.retryAfter)
				}
				return &http.Response{StatusCode: test.status, Header: header, Body: io.NopCloser(strings.NewReader(test.body)), Request: request}, nil
			})}
			client := newOAuthClient(httpClient, func() string { return "0.2.111" })
			client.tokenURL = "https://auth.x.ai/oauth2/token"
			_, err := client.refresh(context.Background(), "refresh")
			var refreshErr *provider.CredentialRefreshError
			if !errors.As(err, &refreshErr) || refreshErr.Permanent != test.permanent || refreshErr.Code != test.code {
				t.Fatalf("error = %#v", err)
			}
			if test.retryAfter != "" && refreshErr.RetryAfter != 7*time.Second {
				t.Fatalf("retry after = %s", refreshErr.RetryAfter)
			}
		})
	}
}

func TestOAuthScopeMatchesOfficialPersonalAccountContract(t *testing.T) {
	values := strings.Fields(defaultOAuthScope)
	want := []string{
		"openid", "profile", "email", "offline_access", "grok-cli:access", "api:access",
		"conversations:read", "conversations:write", "workspaces:read", "workspaces:write",
	}
	if len(values) != len(want) {
		t.Fatalf("scope count = %d, want %d: %v", len(values), len(want), values)
	}
	for index := range want {
		if values[index] != want[index] {
			t.Fatalf("scope[%d] = %q, want %q", index, values[index], want[index])
		}
	}
}
