package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type failureAttemptRecorder struct {
	method              string
	path                string
	remainingBodyBudget int
	attempts            []audit.Attempt
}

func newFailureAttemptRecorder(method, path string) *failureAttemptRecorder {
	return &failureAttemptRecorder{method: method, path: sanitizeRequestPath(path), remainingBodyBudget: diagnosticTotalBodyLimit}
}

const (
	diagnosticBodyLimit        = 64 << 10
	diagnosticTotalBodyLimit   = 256 << 10
	diagnosticTextLimit        = 2048
	diagnosticHeadersLimit     = 4 << 10
	diagnosticHeaderValueLimit = 512
	diagnosticErrorFrameLimit  = 8
)

var (
	diagnosticAuthorizationPattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	diagnosticCookiePattern        = regexp.MustCompile(`(?i)\b(cookie|set-cookie)\b\s*[:=]\s*[^\r\n]+`)
	diagnosticSecretPattern        = regexp.MustCompile(`(?i)(["']?(?:authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token)["']?\s*[:=]\s*["']?)[^"'\s,;}]+`)
	diagnosticURLPattern           = regexp.MustCompile(`https?://[^\s"'<>]+`)
)

func (r *failureAttemptRecorder) captureCredentialFailure(credential accountdomain.Credential, startedAt time.Time, force bool, err error) {
	if err == nil {
		return
	}
	stage := "credential_validation"
	if force {
		stage = "credential_refresh"
	}
	r.append(audit.Attempt{
		Source:         audit.AttemptSourceCredential,
		Stage:          stage,
		AccountID:      auditAccountID(credential.ID),
		AccountName:    credential.Name,
		StartedAt:      startedAt.UTC(),
		DurationMS:     time.Since(startedAt).Milliseconds(),
		TransportError: sanitizeDiagnosticText(err.Error(), diagnosticTextLimit),
		ErrorChain:     errorFrames(err),
	})
}

func (r *failureAttemptRecorder) captureResponse(credential accountdomain.Credential, startedAt time.Time, response *provider.Response, requestErr error) error {
	if requestErr != nil {
		r.append(audit.Attempt{
			Source:         audit.AttemptSourceTransport,
			Stage:          transportStage(requestErr),
			AccountID:      auditAccountID(credential.ID),
			AccountName:    credential.Name,
			Method:         r.method,
			RequestPath:    r.path,
			UpstreamURL:    sanitizeUpstreamURL(errorUpstreamURL(requestErr)),
			StartedAt:      startedAt.UTC(),
			DurationMS:     time.Since(startedAt).Milliseconds(),
			TransportError: sanitizeDiagnosticText(requestErr.Error(), diagnosticTextLimit),
			ErrorChain:     errorFrames(requestErr),
		})
		return requestErr
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}

	statusCode := response.StatusCode
	status := response.Status
	headers := sanitizeDiagnosticHeaders(response.Header)
	var body []byte
	var bodyTruncated bool
	if response.Diagnostic != nil {
		statusCode = response.Diagnostic.StatusCode
		status = response.Diagnostic.Status
		headers = sanitizeDiagnosticHeaders(response.Diagnostic.Header)
		body, bodyTruncated = r.captureBody(response.Diagnostic.Body, response.Diagnostic.BodyTruncated)
	} else {
		var err error
		var replay io.ReadCloser
		body, replay, bodyTruncated, err = readResponseBody(response.Body)
		response.Body = replay
		body, bodyTruncated = r.captureBody(body, bodyTruncated)
		if err != nil {
			r.append(audit.Attempt{
				Source:                audit.AttemptSourceUpstreamHTTP,
				Stage:                 "response_body",
				AccountID:             auditAccountID(credential.ID),
				AccountName:           credential.Name,
				Method:                r.method,
				RequestPath:           r.path,
				UpstreamURL:           sanitizeUpstreamURL(response.UpstreamURL),
				StartedAt:             startedAt.UTC(),
				DurationMS:            time.Since(startedAt).Milliseconds(),
				UpstreamStatusCode:    &statusCode,
				UpstreamStatus:        status,
				ResponseHeaders:       headers,
				ResponseBody:          body,
				ResponseBodyTruncated: bodyTruncated,
				TransportError:        sanitizeDiagnosticText(err.Error(), diagnosticTextLimit),
				ErrorChain:            errorFrames(err),
			})
			return err
		}
	}
	r.append(audit.Attempt{
		Source:                audit.AttemptSourceUpstreamHTTP,
		Stage:                 "upstream_response",
		AccountID:             auditAccountID(credential.ID),
		AccountName:           credential.Name,
		Method:                r.method,
		RequestPath:           r.path,
		UpstreamURL:           sanitizeUpstreamURL(response.UpstreamURL),
		StartedAt:             startedAt.UTC(),
		DurationMS:            time.Since(startedAt).Milliseconds(),
		UpstreamStatusCode:    &statusCode,
		UpstreamStatus:        status,
		ResponseHeaders:       headers,
		ResponseBody:          body,
		ResponseBodyTruncated: bodyTruncated,
	})
	return nil
}

func (r *failureAttemptRecorder) captureStreamFailure(credential accountdomain.Credential, startedAt time.Time, response *provider.Response, diagnostic StreamFailureDiagnostic) {
	if response == nil {
		return
	}
	statusCode := response.StatusCode
	body, bodyTruncated := r.captureBody(diagnostic.Body, diagnostic.BodyTruncated)
	r.append(audit.Attempt{
		Source:                audit.AttemptSourceUpstreamHTTP,
		Stage:                 "response_stream",
		AccountID:             auditAccountID(credential.ID),
		AccountName:           credential.Name,
		Method:                r.method,
		RequestPath:           r.path,
		UpstreamURL:           sanitizeUpstreamURL(response.UpstreamURL),
		StartedAt:             startedAt.UTC(),
		DurationMS:            time.Since(startedAt).Milliseconds(),
		UpstreamStatusCode:    &statusCode,
		UpstreamStatus:        response.Status,
		ResponseHeaders:       sanitizeDiagnosticHeaders(response.Header),
		ResponseBody:          body,
		ResponseBodyTruncated: bodyTruncated,
	})
}

func (r *failureAttemptRecorder) append(attempt audit.Attempt) {
	attempt.Number = len(r.attempts) + 1
	r.attempts = append(r.attempts, attempt)
}

// captureBody 在单次和单请求预算内保留可读的脱敏正文片段。
func (r *failureAttemptRecorder) captureBody(body []byte, alreadyTruncated bool) ([]byte, bool) {
	if len(body) == 0 {
		return nil, alreadyTruncated
	}
	if !utf8.Valid(body) {
		return nil, true
	}
	limit := min(diagnosticBodyLimit, r.remainingBodyBudget)
	if limit <= 0 {
		return nil, true
	}
	truncated := alreadyTruncated || len(body) > limit
	if len(body) > limit {
		body = body[:limit]
	}
	result := []byte(sanitizeDiagnosticText(string(body), limit))
	r.remainingBodyBudget -= len(result)
	return result, truncated
}

func (r *failureAttemptRecorder) snapshot() []audit.Attempt {
	return append([]audit.Attempt(nil), r.attempts...)
}

func auditAccountID(id uint64) *uint64 {
	if id == 0 {
		return nil
	}
	return &id
}

type replayReadCloser struct {
	io.Reader
	source io.Closer
}

func (r *replayReadCloser) Close() error { return r.source.Close() }

// readResponseBody 只读取诊断上限，同时把已读取前缀接回原始响应供后续错误处理。
func readResponseBody(body io.ReadCloser) ([]byte, io.ReadCloser, bool, error) {
	if body == nil {
		return nil, io.NopCloser(bytes.NewReader(nil)), false, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, diagnosticBodyLimit+1))
	truncated := len(data) > diagnosticBodyLimit
	if truncated || err != nil {
		captured := data
		if len(captured) > diagnosticBodyLimit {
			captured = captured[:diagnosticBodyLimit]
		}
		replay := &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(data), body), source: body}
		return captured, replay, truncated, err
	}
	closeErr := body.Close()
	return data, io.NopCloser(bytes.NewReader(data)), false, closeErr
}

func errorFrames(err error) []audit.ErrorFrame {
	frames := make([]audit.ErrorFrame, 0, 4)
	appendErrorFrames(&frames, err)
	return frames
}

func appendErrorFrames(frames *[]audit.ErrorFrame, err error) {
	if err == nil || len(*frames) >= diagnosticErrorFrameLimit {
		return
	}
	*frames = append(*frames, audit.ErrorFrame{Type: truncateDiagnosticText(reflect.TypeOf(err).String(), 256), Message: sanitizeDiagnosticText(err.Error(), 512)})
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, nested := range joined.Unwrap() {
			appendErrorFrames(frames, nested)
		}
		return
	}
	appendErrorFrames(frames, errors.Unwrap(err))
}

func sanitizeDiagnosticHeaders(headers http.Header) map[string][]string {
	result := make(map[string][]string)
	remaining := diagnosticHeadersLimit
	for name, values := range headers {
		if remaining <= 0 {
			break
		}
		lowerName := strings.ToLower(name)
		if !isAllowedDiagnosticHeader(lowerName) {
			continue
		}
		cleanValues := make([]string, 0, min(len(values), 8))
		for _, value := range values {
			if len(cleanValues) == 8 || remaining <= len(name) {
				break
			}
			cleanValue := sanitizeDiagnosticText(value, min(diagnosticHeaderValueLimit, remaining-len(name)))
			cleanValues = append(cleanValues, cleanValue)
			remaining -= len(name) + len(cleanValue)
		}
		if len(cleanValues) > 0 {
			result[http.CanonicalHeaderKey(name)] = cleanValues
		}
	}
	return result
}

func isAllowedDiagnosticHeader(name string) bool {
	if strings.HasPrefix(name, "x-ratelimit-") || strings.HasPrefix(name, "ratelimit-") {
		return true
	}
	switch name {
	case "content-length", "content-type", "date", "retry-after", "server", "cf-ray", "request-id", "traceparent", "tracestate", "via", "x-correlation-id", "x-request-id", "x-grok2api-compatibility-warnings":
		return true
	default:
		return false
	}
}

func sanitizeDiagnosticText(value string, limit int) string {
	value = diagnosticCookiePattern.ReplaceAllString(value, "$1: [REDACTED]")
	value = diagnosticAuthorizationPattern.ReplaceAllString(value, "$1 [REDACTED]")
	value = diagnosticSecretPattern.ReplaceAllString(value, "$1[REDACTED]")
	value = diagnosticURLPattern.ReplaceAllStringFunc(value, sanitizeUpstreamURL)
	return truncateDiagnosticText(value, limit)
}

func truncateDiagnosticText(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}

func sanitizeRequestPath(value string) string {
	parsed, err := url.ParseRequestURI(value)
	if err != nil {
		return truncateDiagnosticText(strings.SplitN(value, "?", 2)[0], 2048)
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return truncateDiagnosticText(parsed.String(), 2048)
}

func sanitizeUpstreamURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return truncateDiagnosticText(parsed.String(), 4096)
}

func errorUpstreamURL(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.URL
	}
	return ""
}

func transportStage(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "request_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "request_timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_lookup"
	}
	var certificateError *tls.CertificateVerificationError
	if errors.As(err, &certificateError) {
		return "tls_verification"
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return "tls_verification"
	}
	var recordHeaderError tls.RecordHeaderError
	if errors.As(err, &recordHeaderError) {
		return "tls_handshake"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "network_timeout"
	}
	var operationError *net.OpError
	if errors.As(err, &operationError) && operationError.Op != "" {
		return operationError.Op
	}
	return "transport"
}
