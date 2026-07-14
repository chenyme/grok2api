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
				if request.FormValue("grant_type") != "refresh_token" || request.FormValue("refresh_token") != "refresh" {
					t.Fatalf("form = %#v", request.Form)
				}
				header := make(http.Header)
				if test.retryAfter != "" {
					header.Set("Retry-After", test.retryAfter)
				}
				return &http.Response{StatusCode: test.status, Header: header, Body: io.NopCloser(strings.NewReader(test.body)), Request: request}, nil
			})}
			client := newOAuthClient(httpClient)
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
