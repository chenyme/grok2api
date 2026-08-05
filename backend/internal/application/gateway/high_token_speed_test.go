package gateway

import (
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

func TestAuditOutputTokensPerSecondMatchesPanel(t *testing.T) {
	first := int64(250)
	record := audit.Record{
		Streaming:    true,
		StatusCode:   200,
		FirstTokenMS: &first,
		DurationMS:   1250,
		OutputTokens: 80,
	}
	got, ok := auditOutputTokensPerSecond(record)
	if !ok || got != 80 {
		t.Fatalf("got %v ok=%v, want 80 true", got, ok)
	}
}

func TestAuditOutputTokensPerSecondRequiresStreamSuccess(t *testing.T) {
	first := int64(100)
	cases := []audit.Record{
		{Streaming: false, StatusCode: 200, FirstTokenMS: &first, DurationMS: 1100, OutputTokens: 100},
		{Streaming: true, StatusCode: 500, FirstTokenMS: &first, DurationMS: 1100, OutputTokens: 100},
		{Streaming: true, StatusCode: 200, FirstTokenMS: nil, DurationMS: 1100, OutputTokens: 100},
		{Streaming: true, StatusCode: 200, FirstTokenMS: &first, DurationMS: 100, OutputTokens: 100},
		{Streaming: true, StatusCode: 200, FirstTokenMS: &first, DurationMS: 1100, OutputTokens: 0},
		{Streaming: true, StatusCode: 200, ErrorCode: "stream_closed", FirstTokenMS: &first, DurationMS: 1100, OutputTokens: 100},
	}
	for i, record := range cases {
		if _, ok := auditOutputTokensPerSecond(record); ok {
			t.Fatalf("case %d unexpectedly measured", i)
		}
	}
}

func TestBuildHighTokenSpeedPolicyMatchesConfiguredModels(t *testing.T) {
	service := &Service{}
	service.UpdateBuildHighTokenSpeedAutoDisable(true, 1000, []string{" grok-4.20 ", "Grok-4.20", "build/ignored"})
	service.buildHighTokenSpeedMu.RLock()
	defer service.buildHighTokenSpeedMu.RUnlock()
	if !service.buildHighTokenSpeedPolicy.enabled || service.buildHighTokenSpeedPolicy.threshold != 1000 {
		t.Fatalf("policy = %#v", service.buildHighTokenSpeedPolicy)
	}
	if _, ok := service.buildHighTokenSpeedPolicy.models["grok-4.20"]; !ok {
		t.Fatalf("missing model map: %#v", service.buildHighTokenSpeedPolicy.models)
	}
	if len(service.buildHighTokenSpeedPolicy.models) != 2 {
		t.Fatalf("expected 2 unique models, got %#v", service.buildHighTokenSpeedPolicy.models)
	}
}

func TestMaybeDisableRequiresBuildProviderAndWatchedModel(t *testing.T) {
	service := &Service{}
	service.UpdateBuildHighTokenSpeedAutoDisable(true, 1000, []string{"grok-4.20"})
	first := int64(100)
	record := audit.Record{
		Streaming: true, StatusCode: 200, FirstTokenMS: &first, DurationMS: 200, OutputTokens: 500, ModelPublicID: "grok-4.20",
	}
	// No accounts service: calling with non-Build must no-op without panic.
	service.maybeDisableBuildAccountForHighTokenSpeed(nil, record, accountdomain.Credential{Provider: accountdomain.ProviderWeb, ID: 1}, "grok-4.20")
	// Below threshold must no-op.
	low := audit.Record{Streaming: true, StatusCode: 200, FirstTokenMS: &first, DurationMS: 2000, OutputTokens: 100, ModelPublicID: "grok-4.20", AccountID: uint64Ptr(1)}
	service.maybeDisableBuildAccountForHighTokenSpeed(nil, low, accountdomain.Credential{Provider: accountdomain.ProviderBuild, ID: 1}, "grok-4.20")
}

func uint64Ptr(value uint64) *uint64 { return &value }
