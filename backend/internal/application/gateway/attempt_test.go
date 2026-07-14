package gateway

import (
	"context"
	"errors"
	"fmt"
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

func TestFailureAttemptRecorderLimitsAndSanitizesHTTPResponse(t *testing.T) {
	body := strings.Repeat("upstream failure\n", 8192)
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	response := &provider.Response{
		StatusCode:  http.StatusBadGateway,
		Status:      "502 Bad Gateway",
		Header:      http.Header{"Content-Type": {"text/plain"}, "Set-Cookie": {"session=secret"}, "X-Request-ID": {"req-123"}},
		Body:        io.NopCloser(strings.NewReader(body)),
		UpstreamURL: "https://user:password@api.example.test/v1/responses?token=secret#debug",
	}
	if err := recorder.captureResponse(account.Credential{ID: 9, Name: "primary"}, time.Now(), response, nil); err != nil {
		t.Fatal(err)
	}
	stored := recorder.snapshot()
	if len(stored) != 1 || stored[0].Source != audit.AttemptSourceUpstreamHTTP || len(stored[0].ResponseBody) != diagnosticBodyLimit || !stored[0].ResponseBodyTruncated {
		t.Fatalf("attempt = %#v", stored)
	}
	headers := http.Header(stored[0].ResponseHeaders)
	if stored[0].UpstreamURL != "https://api.example.test/v1/responses" || headers.Get("Set-Cookie") != "" || headers.Get("X-Request-Id") != "req-123" {
		t.Fatalf("sanitized attempt = %#v", stored[0])
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
			Header:     http.Header{"Content-Type": {"text/plain"}, "Authorization": {"Bearer secret"}},
			Body:       []byte("failure access_token=secret-token"),
		},
	}
	if err := recorder.captureResponse(account.Credential{ID: 9, Name: "primary"}, time.Now(), response, nil); err != nil {
		t.Fatal(err)
	}
	stored := recorder.snapshot()[0]
	if string(stored.ResponseBody) != "failure access_token=[REDACTED]" || http.Header(stored.ResponseHeaders).Get("Authorization") != "" {
		t.Fatalf("attempt = %#v", stored)
	}
	converted, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(converted), "normalized") {
		t.Fatalf("provider response body = %q, err = %v", converted, err)
	}
}

func TestFailureAttemptRecorderClassifiesTransportErrorChain(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "api.example.test", IsNotFound: true}
	requestErr := &url.Error{Op: "Post", URL: "https://user:password@api.example.test/v1/responses?token=secret", Err: dnsErr}
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	if err := recorder.captureResponse(account.Credential{ID: 3, Name: "primary"}, time.Now(), nil, requestErr); !errors.Is(err, dnsErr) {
		t.Fatalf("capture error = %v", err)
	}
	stored := recorder.snapshot()
	if len(stored) != 1 || stored[0].Stage != "dns_lookup" || stored[0].UpstreamURL != "https://api.example.test/v1/responses" || len(stored[0].ErrorChain) != 2 {
		t.Fatalf("attempt = %#v", stored)
	}
	if transportStage(context.DeadlineExceeded) != "request_timeout" {
		t.Fatalf("deadline stage = %s", transportStage(context.DeadlineExceeded))
	}
}

func TestFailureAttemptRecorderBoundsTotalBodyAndErrorChain(t *testing.T) {
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	for index := 0; index < 5; index++ {
		response := &provider.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", diagnosticBodyLimit)))}
		if err := recorder.captureResponse(account.Credential{ID: uint64(index + 1)}, time.Now(), response, nil); err != nil {
			t.Fatal(err)
		}
	}
	stored := recorder.snapshot()
	var captured int
	for _, attempt := range stored {
		captured += len(attempt.ResponseBody)
	}
	if captured != diagnosticTotalBodyLimit || len(stored[4].ResponseBody) != 0 || !stored[4].ResponseBodyTruncated {
		t.Fatalf("captured = %d, final attempt = %#v", captured, stored[4])
	}

	var wrapped error = errors.New("root")
	for index := 0; index < 20; index++ {
		wrapped = fmt.Errorf("layer %d: %w", index, wrapped)
	}
	if frames := errorFrames(wrapped); len(frames) != diagnosticErrorFrameLimit {
		t.Fatalf("error frames = %d", len(frames))
	}
}
