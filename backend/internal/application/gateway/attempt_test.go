package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestFailureAttemptRecorderPreservesCompleteHTTPResponse(t *testing.T) {
	body := strings.Repeat("upstream failure\n", 8192)
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	response := &provider.Response{
		StatusCode:  http.StatusBadGateway,
		Status:      "502 Bad Gateway",
		Header:      http.Header{"Content-Type": {"text/plain"}, "X-Debug": {"edge-a", "edge-b"}},
		Body:        io.NopCloser(strings.NewReader(body)),
		UpstreamURL: "https://api.example.test/v1/responses",
	}
	if err := recorder.captureResponse(account.Credential{ID: 9, Name: "primary"}, time.Now(), response, nil); err != nil {
		t.Fatal(err)
	}
	stored := recorder.snapshot()
	if len(stored) != 1 || stored[0].Source != audit.AttemptSourceUpstreamHTTP || string(stored[0].ResponseBody) != body || len(stored[0].ResponseHeaders["X-Debug"]) != 2 {
		t.Fatalf("attempt = %#v", stored)
	}
	rebuilt, err := io.ReadAll(response.Body)
	if err != nil || string(rebuilt) != body {
		t.Fatalf("rebuilt body length = %d, err = %v", len(rebuilt), err)
	}
}

func TestFailureAttemptRecorderUsesProviderDiagnosticResponse(t *testing.T) {
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	response := &provider.Response{
		StatusCode:  http.StatusBadGateway,
		Status:      "502 Bad Gateway",
		Header:      http.Header{"Content-Type": {"application/json"}},
		Body:        io.NopCloser(strings.NewReader(`{"error":{"message":"normalized"}}`)),
		UpstreamURL: "https://api.example.test/v1/responses",
		Diagnostic: &provider.DiagnosticResponse{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Header:     http.Header{"Content-Type": {"text/plain"}, "X-Upstream": {"raw"}},
			Body:       []byte("complete raw upstream failure"),
		},
	}
	if err := recorder.captureResponse(account.Credential{ID: 9, Name: "primary"}, time.Now(), response, nil); err != nil {
		t.Fatal(err)
	}
	stored := recorder.snapshot()[0]
	if string(stored.ResponseBody) != "complete raw upstream failure" || len(stored.ResponseHeaders["X-Upstream"]) != 1 || stored.ResponseHeaders["X-Upstream"][0] != "raw" {
		t.Fatalf("attempt = %#v", stored)
	}
	converted, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(converted), "normalized") {
		t.Fatalf("provider response body = %q, err = %v", converted, err)
	}
}

func TestFailureAttemptRecorderClassifiesTransportErrorChain(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "api.example.test", IsNotFound: true}
	requestErr := &url.Error{Op: "Post", URL: "https://api.example.test/v1/responses", Err: dnsErr}
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	if err := recorder.captureResponse(account.Credential{ID: 3, Name: "primary"}, time.Now(), nil, requestErr); !errors.Is(err, dnsErr) {
		t.Fatalf("capture error = %v", err)
	}
	stored := recorder.snapshot()
	if len(stored) != 1 || stored[0].Stage != "dns_lookup" || stored[0].UpstreamURL != requestErr.URL || len(stored[0].ErrorChain) != 2 {
		t.Fatalf("attempt = %#v", stored)
	}
	if transportStage(context.DeadlineExceeded) != "request_timeout" {
		t.Fatalf("deadline stage = %s", transportStage(context.DeadlineExceeded))
	}
}
