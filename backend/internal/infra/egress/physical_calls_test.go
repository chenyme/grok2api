package egress

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

func TestPhysicalCallTraceRecordsPlaneStageOrdinalAndOutcome(t *testing.T) {
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	t.Cleanup(func() { perfmetrics.Default = previous })

	ctx := WithPhysicalCallTrace(context.Background(), "grok_build", "responses")
	recordPhysicalCall(ctx, &http.Response{StatusCode: http.StatusForbidden}, nil)
	fallbackCtx := WithPhysicalCallPlane(WithPhysicalCallStage(ctx, "plane_fallback"), "xai")
	recordPhysicalCall(fallbackCtx, &http.Response{StatusCode: http.StatusOK}, nil)
	recordPhysicalCall(WithPhysicalCallStage(ctx, "reasoning_replay"), nil, errors.New("connection reset"))

	samples := registry.CollectAndReset()
	assertPhysicalCallMetric(t, samples, "grok_build", "build", "primary", "1", "client_error")
	assertPhysicalCallMetric(t, samples, "grok_build", "xai", "plane_fallback", "2", "success")
	assertPhysicalCallMetric(t, samples, "grok_build", "build", "reasoning_replay", "3", "transport_error")
}

func TestPhysicalCallTraceNormalizesCompactionOperation(t *testing.T) {
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	t.Cleanup(func() { perfmetrics.Default = previous })

	ctx := WithPhysicalCallTrace(context.Background(), "grok_build", "compaction")
	recordPhysicalCall(ctx, &http.Response{StatusCode: http.StatusOK}, nil)
	legacyCtx := WithPhysicalCallTrace(context.Background(), "grok_build", "responses_compact")
	recordPhysicalCall(legacyCtx, &http.Response{StatusCode: http.StatusOK}, nil)

	samples := registry.CollectAndReset()
	var count uint64
	for _, sample := range samples {
		if sample.Name == "upstream_physical_call_total" && sample.Labels.Operation == "compaction" {
			count += sample.Count
		}
		if sample.Name == "upstream_physical_call_total" && sample.Labels.Operation == "other" {
			t.Fatalf("compaction was classified as other: %#v", sample)
		}
	}
	if count != 2 {
		t.Fatalf("normalized compaction count = %d, want 2: %#v", count, samples)
	}
}

func TestProxyPoolConnectionRetriesCountAsOneProviderCall(t *testing.T) {
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	t.Cleanup(func() { perfmetrics.Default = previous })

	client := &scriptedRequestClient{do: func(call int, _ *http.Request) (*http.Response, error) {
		if call == 1 {
			return nil, errors.New("proxyconnect tcp: connection refused")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}}
	lease := &Lease{client: client, proxyPool: true}
	ctx := WithPhysicalCallTrace(context.Background(), "grok_web", "messages")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://grok.com/rest/app-chat", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := lease.Do(request)
	if err != nil || response.StatusCode != http.StatusOK || client.calls != 2 {
		t.Fatalf("response=%#v calls=%d err=%v", response, client.calls, err)
	}
	samples := registry.CollectAndReset()
	assertPhysicalCallMetric(t, samples, "grok_web", "web", "primary", "1", "success")
	var total uint64
	for _, sample := range samples {
		if sample.Name == "upstream_physical_call_total" {
			total += sample.Count
		}
	}
	if total != 1 {
		t.Fatalf("physical call count = %d, want 1: %#v", total, samples)
	}
}

func assertPhysicalCallMetric(t *testing.T, samples []perfmetrics.Sample, provider, plane, stage, ordinal, outcome string) {
	t.Helper()
	for _, sample := range samples {
		labels := sample.Labels
		if sample.Name == "upstream_physical_call_total" && labels.Provider == provider && labels.Plane == plane && labels.Stage == stage && labels.Ordinal == ordinal && labels.Outcome == outcome {
			if sample.Count != 1 || sample.Total != 1 {
				t.Fatalf("physical call sample = %#v", sample)
			}
			return
		}
	}
	t.Fatalf("physical call metric not found: %#v", samples)
}
