package cli

import (
	"context"
	"encoding/base64"
	"errors"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareImportedCredentialRefreshesRTOnlySeed(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-1","email":"user@example.com","team_id":"team-1"}`)) + ".signature"
	requests := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		wantRefresh := "original-rt"
		response := `{"access_token":"fresh-access","refresh_token":"rotated-rt","id_token":"` + idToken + `","expires_in":3600}`
		if requests == 2 {
			wantRefresh = "rotated-rt"
			response = `{"access_token":"renewed-access","refresh_token":"rotated-again","expires_in":3600}`
		}
		if request.FormValue("grant_type") != "refresh_token" || request.FormValue("refresh_token") != wantRefresh || request.FormValue("client_id") != "custom-client" {
			t.Fatalf("form = %#v", request.Form)
		}
		return oauthResponse(http.StatusOK, response), nil
	})}
	adapter := &Adapter{oauth: newOAuthClient(httpClient, nil), cipher: cipher}
	prepared, err := adapter.PrepareImportedCredential(context.Background(), provider.CredentialSeed{OIDCClientID: "custom-client", RefreshToken: "original-rt"})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.AccessToken != "fresh-access" || prepared.RefreshToken != "rotated-rt" || prepared.UserID != "user-1" || prepared.Email != "user@example.com" || prepared.TeamID != "team-1" || prepared.OIDCClientID != "custom-client" || prepared.SourceKey == "" {
		t.Fatalf("prepared seed = %#v", prepared)
	}
	lead := time.Until(prepared.ExpiresAt)
	if lead < 59*time.Minute || lead > 61*time.Minute {
		t.Fatalf("expires in = %s", lead)
	}
	encryptedRefresh, err := cipher.Encrypt(prepared.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := adapter.RefreshCredential(context.Background(), accountdomain.Credential{OIDCClientID: prepared.OIDCClientID, EncryptedRefreshToken: encryptedRefresh})
	if err != nil {
		t.Fatal(err)
	}
	renewedRefresh, err := cipher.Decrypt(renewed.EncryptedRefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if renewedRefresh != "rotated-again" || requests != 2 {
		t.Fatalf("renewed refresh = %q, requests = %d", renewedRefresh, requests)
	}
}

func TestCredentialRefreshCallsOAuthAndPersistsRotationEndToEnd(t *testing.T) {
	ctx := context.Background()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if err := request.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "original-rt" || request.Form.Get("client_id") != "custom-client" {
			t.Errorf("form = %#v", request.Form)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"fresh-access","refresh_token":"rotated-rt","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)

	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "oauth-refresh-e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accessEncrypted, err := cipher.Encrypt("old-access")
	if err != nil {
		t.Fatal(err)
	}
	refreshEncrypted, err := cipher.Encrypt("original-rt")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{}, cipher)
	adapter.oauth.http = server.Client()
	adapter.oauth.tokenURL = server.URL
	repository := relational.NewAccountRepository(database)
	credential, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "oauth-e2e", SourceKey: "oauth-e2e", OIDCClientID: "custom-client",
		EncryptedAccessToken: accessEncrypted, EncryptedRefreshToken: refreshEncrypted, ExpiresAt: time.Now().UTC().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := accountapp.NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)
	refreshed, err := service.EnsureCredential(ctx, credential, true)
	if err != nil {
		t.Fatal(err)
	}
	storedAccess, err := cipher.Decrypt(refreshed.EncryptedAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	storedRefresh, err := cipher.Decrypt(refreshed.EncryptedRefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || storedAccess != "fresh-access" || storedRefresh != "rotated-rt" || refreshed.LastRefreshAt == nil {
		t.Fatalf("requests=%d access=%q refresh=%q credential=%#v", requests, storedAccess, storedRefresh, refreshed)
	}
}

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
		message    string
		response   string
	}{
		{name: "transient upstream", status: http.StatusServiceUnavailable, body: `{"error":"temporarily_unavailable"}`, retryAfter: "7", code: "temporarily_unavailable"},
		{name: "bad request is not inherently permanent", status: http.StatusBadRequest, body: `{"error":"temporarily_unavailable","error_description":"Try another egress"}`, code: "temporarily_unavailable", message: "Try another egress"},
		{name: "unauthorized client is configuration scoped", status: http.StatusUnauthorized, body: `{"error":"invalid_client","error_description":"Client authentication failed"}`, code: "invalid_client", message: "Client authentication failed"},
		{name: "invalid grant", status: http.StatusBadRequest, body: `{"error":"invalid_grant","error_description":"Refresh token has expired","message":"Access denied","request_id":"req-123","refresh_token":"must-not-leak"}`, permanent: true, code: "invalid_grant", message: "Refresh token has expired · Access denied", response: `"refresh_token":"[REDACTED]"`},
		{name: "nested error", status: http.StatusBadRequest, body: `{"error":{"code":"invalid_client","message":"Client rejected","detail":"Application disabled"}}`, code: "invalid_client", message: "Client rejected · Application disabled", response: `"detail":"Application disabled"`},
		{name: "explicit revoked refresh token", status: http.StatusUnauthorized, body: `{"error":"refresh_token_revoked"}`, permanent: true, code: "refresh_token_revoked"},
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
			if !errors.As(err, &refreshErr) || refreshErr.Status != test.status || refreshErr.Permanent != test.permanent || refreshErr.Code != test.code || refreshErr.Message != test.message {
				t.Fatalf("error = %#v", err)
			}
			if test.response != "" && !strings.Contains(refreshErr.Response, test.response) {
				t.Fatalf("response = %q, want substring %q", refreshErr.Response, test.response)
			}
			if strings.Contains(refreshErr.Response, "must-not-leak") {
				t.Fatalf("response leaked refresh token: %q", refreshErr.Response)
			}
			if test.retryAfter != "" && refreshErr.RetryAfter != 7*time.Second {
				t.Fatalf("retry after = %s", refreshErr.RetryAfter)
			}
		})
	}
}

func TestOAuthRefreshTreatsMalformedSuccessAsRetryable(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return oauthResponse(http.StatusOK, `{"expires_in":3600}`), nil
	})}
	client := newOAuthClient(httpClient, nil)
	_, err := client.refresh(context.Background(), "refresh")
	var refreshErr *provider.CredentialRefreshError
	if !errors.As(err, &refreshErr) || refreshErr.Code != "missing_access_token" || refreshErr.Permanent {
		t.Fatalf("error = %#v", err)
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
