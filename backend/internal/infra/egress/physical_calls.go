package egress

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

type physicalCallTrace struct {
	provider  string
	operation string
	ordinal   atomic.Uint64
}

type physicalCallContext struct {
	trace *physicalCallTrace
	plane string
	stage string
}

type physicalCallContextKey struct{}

// WithPhysicalCallTrace starts observe-only physical-call accounting for one
// downstream request. It does not impose a retry budget or alter transport.
func WithPhysicalCallTrace(ctx context.Context, provider, operation string) context.Context {
	if ctx == nil {
		return ctx
	}
	if current := physicalCallFromContext(ctx); current.trace != nil {
		return ctx
	}
	provider = normalizePhysicalProvider(provider)
	value := physicalCallContext{
		trace: &physicalCallTrace{provider: provider, operation: normalizePhysicalOperation(operation)},
		plane: defaultPhysicalPlane(provider),
		stage: "primary",
	}
	return context.WithValue(ctx, physicalCallContextKey{}, value)
}

// WithPhysicalCallPlane annotates a bounded upstream plane while preserving
// the request-wide ordinal and current stage.
func WithPhysicalCallPlane(ctx context.Context, plane string) context.Context {
	value := physicalCallFromContext(ctx)
	if value.trace == nil {
		return ctx
	}
	value.plane = normalizePhysicalPlane(plane)
	return context.WithValue(ctx, physicalCallContextKey{}, value)
}

// WithPhysicalCallStage annotates an internal retry or preparation stage while
// preserving the request-wide ordinal and upstream plane.
func WithPhysicalCallStage(ctx context.Context, stage string) context.Context {
	value := physicalCallFromContext(ctx)
	if value.trace == nil {
		return ctx
	}
	value.stage = normalizePhysicalStage(stage)
	return context.WithValue(ctx, physicalCallContextKey{}, value)
}

func recordPhysicalCall(ctx context.Context, response *http.Response, err error) {
	value := physicalCallFromContext(ctx)
	if value.trace == nil {
		return
	}
	ordinal := value.trace.ordinal.Add(1)
	perfmetrics.Default.Inc("upstream_physical_call_total", perfmetrics.Labels{
		Subsystem: "upstream",
		Operation: value.trace.operation,
		Provider:  value.trace.provider,
		Plane:     value.plane,
		Stage:     value.stage,
		Ordinal:   physicalOrdinalBucket(ordinal),
		Outcome:   physicalCallOutcome(response, err),
	})
}

// RecordDirectPhysicalCall records a transport call that intentionally bypasses
// the managed egress lease because no Build node is configured.
func RecordDirectPhysicalCall(ctx context.Context, response *http.Response, err error) {
	recordPhysicalCall(ctx, response, err)
}

func physicalCallFromContext(ctx context.Context) physicalCallContext {
	if ctx == nil {
		return physicalCallContext{}
	}
	value, _ := ctx.Value(physicalCallContextKey{}).(physicalCallContext)
	return value
}

func normalizePhysicalProvider(value string) string {
	switch strings.TrimSpace(value) {
	case "grok_build", "grok_web", "grok_console":
		return strings.TrimSpace(value)
	default:
		return "unknown"
	}
}

func normalizePhysicalOperation(value string) string {
	switch strings.TrimSpace(value) {
	case "responses", "chat", "messages", "compaction", "response_get", "response_delete":
		return strings.TrimSpace(value)
	case "responses_compact":
		return "compaction"
	default:
		return "other"
	}
}

func defaultPhysicalPlane(provider string) string {
	switch provider {
	case "grok_build":
		return "build"
	case "grok_web":
		return "web"
	case "grok_console":
		return "console"
	default:
		return "unknown"
	}
}

func normalizePhysicalPlane(value string) string {
	switch strings.TrimSpace(value) {
	case "build", "xai", "web", "console":
		return strings.TrimSpace(value)
	default:
		return "unknown"
	}
}

func normalizePhysicalStage(value string) string {
	switch strings.TrimSpace(value) {
	case "primary", "plane_fallback", "reasoning_replay", "reasoning_session_reset", "compaction", "compaction_retry", "anti_bot_retry", "statsig_meta":
		return strings.TrimSpace(value)
	default:
		return "other"
	}
}

func physicalOrdinalBucket(value uint64) string {
	if value >= 5 {
		return "5_plus"
	}
	return strconv.FormatUint(value, 10)
}

func physicalCallOutcome(response *http.Response, err error) string {
	if err != nil {
		return "transport_error"
	}
	if response == nil {
		return "empty_response"
	}
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return "success"
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return "redirect"
	case response.StatusCode >= 400 && response.StatusCode < 500:
		return "client_error"
	case response.StatusCode >= 500 && response.StatusCode < 600:
		return "server_error"
	default:
		return "other"
	}
}
