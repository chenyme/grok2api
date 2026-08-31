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

func TestFailureAttemptRecorderCapturesStreamFailure(t *testing.T) {
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	response := &provider.Response{
		StatusCode:  http.StatusOK,
		Status:      "200 OK",
		Header:      http.Header{"Content-Type": {"text/event-stream"}, "Set-Cookie": {"session=secret"}},
		UpstreamURL: "https://user:password@api.example.test/v1/responses?token=secret",
	}
	recorder.captureStreamFailure(
		account.Credential{ID: 9, Name: "primary"},
		time.Now().Add(-time.Second),
		response,
		StreamFailureDiagnostic{Body: []byte(`{"type":"response.failed","error":{"message":"access_token=secret-token"}}`)},
	)
	stored := recorder.snapshot()
	if len(stored) != 1 || stored[0].Stage != "response_stream" || stored[0].UpstreamStatusCode == nil || *stored[0].UpstreamStatusCode != http.StatusOK {
		t.Fatalf("attempt = %#v", stored)
	}
	if string(stored[0].ResponseBody) != `{"type":"response.failed","error":{"message":"access_token=[REDACTED]"}}` || stored[0].UpstreamURL != "https://api.example.test/v1/responses" || http.Header(stored[0].ResponseHeaders).Get("Set-Cookie") != "" {
		t.Fatalf("sanitized attempt = %#v", stored[0])
	}
}

func TestEnsureStreamFailureAttemptKeepsPrior429AndSkipsDuplicate(t *testing.T) {
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	first := account.Credential{ID: 11, Name: "retry-a"}
	second := account.Credential{ID: 12, Name: "retry-b"}
	exhausted := &provider.Response{StatusCode: http.StatusTooManyRequests, Status: "Too Many Requests", Body: io.NopCloser(strings.NewReader(`{"code":"subscription:free-usage-exhausted"}`))}
	if err := recorder.captureResponse(first, time.Now(), exhausted, nil); err != nil {
		t.Fatal(err)
	}
	stream := &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"text/event-stream"}}, UpstreamURL: "https://cli-chat-proxy.grok.com/v1/responses"}
	recorder.ensureStreamFailureAttempt(second, time.Now().Add(-time.Second), stream, "upstream_stream_interrupted")
	stored := recorder.snapshot()
	if len(stored) != 2 || stored[0].Stage != "upstream_response" || stored[1].Stage != "response_stream" {
		t.Fatalf("attempts = %#v", stored)
	}
	if stored[1].AccountID == nil || *stored[1].AccountID != second.ID || stored[1].TransportError != "upstream_stream_interrupted" {
		t.Fatalf("retry stream attempt = %#v", stored[1])
	}
	recorder.ensureStreamFailureAttempt(second, time.Now(), stream, "upstream_stream_interrupted")
	if got := recorder.snapshot(); len(got) != 2 {
		t.Fatalf("duplicate stream attempt = %#v", got)
	}
	inStream := newFailureAttemptRecorder(http.MethodPost, "/responses")
	inStream.captureStreamFailure(second, time.Now(), stream, StreamFailureDiagnostic{Body: []byte(`{"type":"response.failed"}`)})
	inStream.ensureStreamFailureAttempt(second, time.Now(), stream, "upstream_stream_error")
	if got := inStream.snapshot(); len(got) != 1 || got[0].Stage != "response_stream" {
		t.Fatalf("ensure after in-stream capture = %#v", got)
	}
}

func TestEnsureStreamFailureAttemptSkipsNon2xx(t *testing.T) {
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	credential := account.Credential{ID: 12, Name: "retry-b"}
	forbidden := &provider.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden"}
	recorder.ensureStreamFailureAttempt(credential, time.Now(), forbidden, "upstream_forbidden")
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("non-2xx stream attempt = %#v", got)
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

func TestFailureAttemptRecorderKeepsRecoveredAttemptsOn2xx(t *testing.T) {
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	status := http.StatusBadRequest
	response := &provider.Response{
		StatusCode:  http.StatusOK,
		Status:      "200 OK",
		UpstreamURL: "https://cli-chat-proxy.grok.com/v1/responses",
		Body:        io.NopCloser(strings.NewReader(`{"id":"ok"}`)),
		RecoveredAttempts: []provider.RecoveredAttempt{{
			Stage:  "reasoning_decode_rejected",
			Result: "recovered_encrypted_content_stripped",
			Diagnostic: provider.DiagnosticResponse{
				StatusCode: status,
				Status:     "400 Bad Request",
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       []byte(`{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`),
			},
		}},
	}
	if err := recorder.captureResponse(account.Credential{ID: 9, Name: "primary"}, time.Now(), response, nil); err != nil {
		t.Fatal(err)
	}
	stored := recorder.snapshot()
	if len(stored) != 1 || stored[0].Stage != "reasoning_decode_rejected" || stored[0].TransportError != "recovered_encrypted_content_stripped" {
		t.Fatalf("stored = %#v", stored)
	}
	if stored[0].UpstreamStatusCode == nil || *stored[0].UpstreamStatusCode != http.StatusBadRequest {
		t.Fatalf("status = %#v", stored[0].UpstreamStatusCode)
	}
	if !strings.Contains(string(stored[0].ResponseBody), "compaction blob") {
		t.Fatalf("body = %q", stored[0].ResponseBody)
	}
}

func TestFailureAttemptRecorderCapturesPinnedSelectionFailure(t *testing.T) {
	recorder := newFailureAttemptRecorder(http.MethodPost, "/responses")
	err := pinnedUnavailableError(42, "bound-account")
	recorder.captureSelectionFailure(0, "", err)
	stored := recorder.snapshot()
	if len(stored) != 1 || stored[0].Source != audit.AttemptSourceCredential || stored[0].Stage != string(SelectionPinnedUnavailable) {
		t.Fatalf("stored = %#v", stored)
	}
	if stored[0].AccountID == nil || *stored[0].AccountID != 42 || stored[0].AccountName != "bound-account" {
		t.Fatalf("account = %#v", stored[0])
	}
}
