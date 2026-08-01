package gateway

import "testing"

func TestQualityProbeOutputTokensPerSecondMatchesAuditPanel(t *testing.T) {
	got := qualityProbeOutputTokensPerSecond(1335, 17320, 17100)
	want := float64(1335) * 1000 / 220
	if got != want {
		t.Fatalf("output TPS = %v, want %v", got, want)
	}
}

func TestQualityProbeOutputTokensPerSecondIncludesReasoningTokens(t *testing.T) {
	// The panel reports completion/output tokens, including reasoning tokens.
	got := qualityProbeOutputTokensPerSecond(1050, 1100, 1000)
	if got != 10500 {
		t.Fatalf("output TPS = %v, want 10500", got)
	}
}
