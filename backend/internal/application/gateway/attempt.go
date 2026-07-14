package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"reflect"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type failureAttemptRecorder struct {
	method   string
	path     string
	attempts []audit.Attempt
}

func newFailureAttemptRecorder(method, path string) *failureAttemptRecorder {
	return &failureAttemptRecorder{method: method, path: path}
}

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
		TransportError: err.Error(),
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
			UpstreamURL:    errorUpstreamURL(requestErr),
			StartedAt:      startedAt.UTC(),
			DurationMS:     time.Since(startedAt).Milliseconds(),
			TransportError: requestErr.Error(),
			ErrorChain:     errorFrames(requestErr),
		})
		return requestErr
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}

	statusCode := response.StatusCode
	status := response.Status
	headers := response.Header.Clone()
	var body []byte
	if response.Diagnostic != nil {
		statusCode = response.Diagnostic.StatusCode
		status = response.Diagnostic.Status
		headers = response.Diagnostic.Header.Clone()
		body = bytes.Clone(response.Diagnostic.Body)
	} else {
		var err error
		body, err = readResponseBody(response.Body)
		response.Body = io.NopCloser(bytes.NewReader(body))
		if err != nil {
			r.append(audit.Attempt{
				Source:             audit.AttemptSourceUpstreamHTTP,
				Stage:              "response_body",
				AccountID:          auditAccountID(credential.ID),
				AccountName:        credential.Name,
				Method:             r.method,
				RequestPath:        r.path,
				UpstreamURL:        response.UpstreamURL,
				StartedAt:          startedAt.UTC(),
				DurationMS:         time.Since(startedAt).Milliseconds(),
				UpstreamStatusCode: &statusCode,
				UpstreamStatus:     status,
				ResponseHeaders:    headers,
				ResponseBody:       body,
				TransportError:     err.Error(),
				ErrorChain:         errorFrames(err),
			})
			return err
		}
	}
	r.append(audit.Attempt{
		Source:             audit.AttemptSourceUpstreamHTTP,
		Stage:              "upstream_response",
		AccountID:          auditAccountID(credential.ID),
		AccountName:        credential.Name,
		Method:             r.method,
		RequestPath:        r.path,
		UpstreamURL:        response.UpstreamURL,
		StartedAt:          startedAt.UTC(),
		DurationMS:         time.Since(startedAt).Milliseconds(),
		UpstreamStatusCode: &statusCode,
		UpstreamStatus:     status,
		ResponseHeaders:    headers,
		ResponseBody:       body,
	})
	return nil
}

func (r *failureAttemptRecorder) append(attempt audit.Attempt) {
	attempt.Number = len(r.attempts) + 1
	r.attempts = append(r.attempts, attempt)
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

func readResponseBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	defer body.Close()
	return io.ReadAll(body)
}

func errorFrames(err error) []audit.ErrorFrame {
	frames := make([]audit.ErrorFrame, 0, 4)
	appendErrorFrames(&frames, err)
	return frames
}

func appendErrorFrames(frames *[]audit.ErrorFrame, err error) {
	if err == nil {
		return
	}
	*frames = append(*frames, audit.ErrorFrame{Type: reflect.TypeOf(err).String(), Message: err.Error()})
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, nested := range joined.Unwrap() {
			appendErrorFrames(frames, nested)
		}
		return
	}
	appendErrorFrames(frames, errors.Unwrap(err))
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
