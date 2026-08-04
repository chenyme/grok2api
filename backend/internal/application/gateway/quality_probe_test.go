package gateway

import (
	"errors"
	"testing"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
)

func TestQualityProbeSelectionFailureCanBeIdentifiedByCaller(t *testing.T) {
	err := normalizeQualityProbeRequestError(errors.Join(ErrNoAvailableAccount, &SelectionUnavailableError{Reason: SelectionCooling}))
	if !errors.Is(err, egressapp.ErrQualityProbeNoAccount) {
		t.Fatalf("error = %v", err)
	}
}

func TestQualityProbeModelIsPinnedToBuildNamespace(t *testing.T) {
	got, ok := qualityProbeBuildPublicModel("grok-shared")
	if !ok || got != "Build/grok-shared" {
		t.Fatalf("Build probe model = %q, valid=%v", got, ok)
	}
	if _, ok := qualityProbeBuildPublicModel("Console/grok-shared"); ok {
		t.Fatal("quality probe must reject an explicitly non-Build model")
	}
}

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
