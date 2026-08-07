package cli

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

type egressTransport struct {
	manager  *infraegress.Manager
	fallback http.RoundTripper
}

func (t *egressTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	affinity := infraegress.AccountFromContext(request.Context())
	if affinity == "" {
		affinity = "bootstrap"
	}
	lease, configured, err := t.manager.AcquireIfConfigured(request.Context(), domainegress.ScopeBuild, affinity)
	if err != nil {
		return nil, err
	}
	if !configured {
		// When account-isolated pools are enabled, still go through the manager's
		// direct node so different accounts do not share the process-wide fallback
		// HTTP transport / TCP connection pool. Preserve the fallback transport's
		// HTTP_PROXY/HTTPS_PROXY behavior while partitioning the pool.
		lease, configured, err = t.manager.AcquireBuildEnvironmentDirectIfIsolated(request.Context(), affinity)
		if err != nil {
			return nil, err
		}
		if !configured {
			idleRequest := t.withStreamIdleContext(request)
			response, requestErr := t.fallback.RoundTrip(idleRequest)
			infraegress.RecordDirectPhysicalCall(request.Context(), response, requestErr)
			if requestErr != nil || response == nil || response.Body == nil {
				return response, requestErr
			}
			response.Body = t.wrapStreamIdleBody(response.Body, idleRequest.Context())
			return response, requestErr
		}
	}
	if lease.UserAgent != "" {
		request.Header.Set("User-Agent", lease.UserAgent)
	}
	idleRequest := t.withStreamIdleContext(request)
	response, err := lease.Do(idleRequest)
	if err != nil {
		if shouldReportEgressFailure(request.Context(), err) {
			t.manager.FeedbackForScope(context.WithoutCancel(request.Context()), domainegress.ScopeBuild, lease.NodeID, 0, err)
		}
		lease.Release()
		return nil, err
	}
	t.manager.FeedbackForScope(context.WithoutCancel(request.Context()), domainegress.ScopeBuild, lease.NodeID, response.StatusCode, nil)
	if response.Body == nil {
		lease.Release()
		return response, nil
	}
	response.Body = &egressResponseBody{ReadCloser: t.wrapStreamIdleBody(response.Body, idleRequest.Context()), release: lease.Release}
	return response, nil
}

// withStreamIdleContext returns a shallow copy of request carrying a
// cancel-cause-aware context derived from the original. The cancel function is
// stashed on the request context so wrapStreamIdleBody can arm an idle timer
// that cancels the context (and thus the transport's body read) when the
// stream goes silent. When no idle timeout is configured the original request
// is returned unchanged.
func (t *egressTransport) withStreamIdleContext(request *http.Request) *http.Request {
	if !acceptsEventStream(request.Header.Values("Accept")) {
		return request
	}
	idle := t.manager.BuildStreamIdleTimeout()
	if idle <= 0 {
		return request
	}
	ctx, cancel := context.WithCancelCause(request.Context())
	return request.Clone(withIdleCancel(ctx, idle, cancel))
}

// acceptsEventStream keeps stream-idle enforcement scoped to requests that
// explicitly negotiate SSE. The Build HTTP client is shared by inference,
// OAuth, models, billing, and media calls, so applying the timeout solely from
// the egress scope would also abort legitimate non-streaming response bodies.
func acceptsEventStream(values []string) bool {
	for _, value := range values {
		for _, candidate := range strings.Split(value, ",") {
			mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(candidate))
			if err == nil && strings.EqualFold(mediaType, "text/event-stream") {
				return true
			}
		}
	}
	return false
}

// wrapStreamIdleBody arms an idle timer over body. The cancel function is read
// from the request context previously installed by withStreamIdleContext. When
// no cancel is present (idle disabled) the body is returned unwrapped.
func (t *egressTransport) wrapStreamIdleBody(body io.ReadCloser, ctx context.Context) io.ReadCloser {
	idle, cancel := idleCancelFrom(ctx)
	if idle <= 0 || cancel == nil {
		return body
	}
	return newIdleTimeoutReadCloser(body, idle, cancel)
}

func shouldReportEgressFailure(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, context.Canceled)
}

type egressResponseBody struct {
	io.ReadCloser
	release func()
}

func (b *egressResponseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.release != nil {
		b.release()
		b.release = nil
	}
	return err
}
