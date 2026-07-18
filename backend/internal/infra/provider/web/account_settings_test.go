package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestWebAccountSettingsMatchCapturedProtocol(t *testing.T) {
	t.Parallel()
	expectedNSFW, err := hex.DecodeString("00000000200a021001121a0a18616c776179735f73686f775f6e7366775f636f6e74656e74")
	if err != nil || !bytes.Equal(enableNSFWBody, expectedNSFW) {
		t.Fatalf("NSFW frame = %x", enableNSFWBody)
	}
	var accountTermsSeen, productTermsSeen, birthSeen, nsfwSeen atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		switch request.URL.Path {
		case "/auth_mgmt.AuthManagement/SetTosAcceptedVersion":
			accountTermsSeen.Store(true)
			if !bytes.Equal(body, acceptTermsBody) || request.Header.Get("Content-Type") != "application/grpc-web+proto" {
				t.Errorf("terms request body=%x content-type=%q", body, request.Header.Get("Content-Type"))
			}
			if cookie := request.Header.Get("Cookie"); cookie != "sso=test-sso; sso-rw=test-sso" {
				t.Errorf("terms cookie = %q", cookie)
			}
			if request.Header.Get("x-grpc-web") != "1" || request.Header.Get("x-user-agent") != "connect-es/2.1.1" {
				t.Errorf("terms grpc headers = %#v", request.Header)
			}
		case "/rest/auth/set-tos-accepted":
			productTermsSeen.Store(true)
			var payload map[string]int
			if json.Unmarshal(body, &payload) != nil || payload["tosVersion"] != account.CurrentWebTermsVersion {
				t.Errorf("product terms payload = %s", body)
			}
			if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("x-statsig-id") == "" {
				t.Errorf("product terms headers = %#v", request.Header)
			}
			if request.Header.Get("Cookie") != "sso=test-sso; sso-rw=test-sso; cf_clearance=clear" || request.Header.Get("Sec-Ch-Ua") == "" {
				t.Errorf("product terms browser identity = %#v", request.Header)
			}
		case "/rest/auth/set-birth-date":
			birthSeen.Store(true)
			var payload map[string]string
			if json.Unmarshal(body, &payload) != nil || payload["birthDate"] != "2000-01-02T16:00:00.000Z" {
				t.Errorf("birth payload = %s", body)
			}
			if request.Header.Get("x-statsig-id") == "" || request.Header.Get("Cookie") != "sso=test-sso; sso-rw=test-sso; cf_clearance=clear" {
				t.Errorf("birth headers = %#v", request.Header)
			}
		case "/auth_mgmt.AuthManagement/UpdateUserFeatureControls":
			nsfwSeen.Store(true)
			if !bytes.Equal(body, enableNSFWBody) || request.Header.Get("x-statsig-id") == "" || request.Header.Get("x-user-agent") != "" {
				t.Errorf("nsfw body=%x headers=%#v", body, request.Header)
			}
		default:
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, _ := cipher.Encrypt("test-sso")
	encryptedCookies, _ := cipher.Encrypt("cf_clearance=clear")
	statsig := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{'s'}, 70))
	adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: statsig}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	adapter.accountsBaseURL = server.URL
	credential := account.Credential{
		ID: 1, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO,
		EncryptedAccessToken: encryptedToken, EncryptedCloudflareCookie: encryptedCookies,
	}

	if err := adapter.AcceptTerms(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SetBirthDate(context.Background(), credential, time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := adapter.EnableNSFW(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	if !accountTermsSeen.Load() || !productTermsSeen.Load() || !birthSeen.Load() || !nsfwSeen.Load() {
		t.Fatalf("seen accountTerms=%v productTerms=%v birth=%v nsfw=%v", accountTermsSeen.Load(), productTermsSeen.Load(), birthSeen.Load(), nsfwSeen.Load())
	}
}

func TestWebAccountSettingsMapUnauthorized(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, _ := cipher.Encrypt("expired-sso")
	adapter := NewAdapter(Config{BaseURL: server.URL}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	adapter.accountsBaseURL = server.URL
	err = adapter.AcceptTerms(context.Background(), account.Credential{
		ID: 2, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, EncryptedAccessToken: encryptedToken,
	})
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestAcceptTermsRequiresBothUpstreamSteps(t *testing.T) {
	t.Parallel()
	var accountCalls, productCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/auth_mgmt.AuthManagement/SetTosAcceptedVersion":
			accountCalls.Add(1)
			writer.WriteHeader(http.StatusOK)
		case "/rest/auth/set-tos-accepted":
			productCalls.Add(1)
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = writer.Write([]byte(`{"error":"product terms unavailable"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, _ := cipher.Encrypt("test-sso")
	statsig := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{'s'}, 70))
	adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: statsig}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	adapter.accountsBaseURL = server.URL
	err = adapter.AcceptTerms(context.Background(), account.Credential{
		ID: 3, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, EncryptedAccessToken: encryptedToken,
	})
	if err == nil || accountCalls.Load() != 1 || productCalls.Load() != 1 {
		t.Fatalf("err=%v accountCalls=%d productCalls=%d", err, accountCalls.Load(), productCalls.Load())
	}
}

func TestWebAccountBirthDateMapsOnlyAlreadySetResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		alreadySet bool
	}{
		{
			name:       "birth date locked",
			body:       `{"code":8,"message":"Birth date is locked once set. Contact support if you need to update it. [WKE=account:birth-date-change-limit-reached]","details":[]}`,
			alreadySet: true,
		},
		{
			name: "ordinary rate limit",
			body: `{"code":8,"message":"Too many requests","details":[]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusTooManyRequests)
				_, _ = writer.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)
			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			encryptedToken, _ := cipher.Encrypt("test-sso")
			statsig := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{'s'}, 70))
			adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: statsig}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)

			err = adapter.SetBirthDate(context.Background(), account.Credential{
				ID: 4, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, EncryptedAccessToken: encryptedToken,
			}, time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC))
			if got := errors.Is(err, provider.ErrBirthDateAlreadySet); got != test.alreadySet {
				t.Fatalf("already set = %v, want %v; err = %v", got, test.alreadySet, err)
			}
			if !test.alreadySet {
				status, ok := provider.ErrorHTTPStatus(err)
				if !ok || status != http.StatusTooManyRequests {
					t.Fatalf("status = %d, ok = %v; err = %v", status, ok, err)
				}
			}
		})
	}
}

func TestWebAccountSettingsRejectsBodyTrailerFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(grpcWebTrailerFrame("7"))
	}))
	t.Cleanup(server.Close)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, _ := cipher.Encrypt("test-sso")
	statsig := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{'s'}, 70))
	adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: statsig}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)

	err = adapter.EnableNSFW(context.Background(), account.Credential{
		ID: 3, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, EncryptedAccessToken: encryptedToken,
	})
	if err == nil || !strings.Contains(err.Error(), "gRPC 状态 7") {
		t.Fatalf("err = %v", err)
	}
}

func grpcWebTrailerFrame(status string) []byte {
	payload := []byte("grpc-status: " + status + "\r\n")
	frame := make([]byte, 5+len(payload))
	frame[0] = 0x80
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}
