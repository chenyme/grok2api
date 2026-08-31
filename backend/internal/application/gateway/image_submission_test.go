package gateway

import "testing"

func TestImageSubmissionStateIsMonotonic(t *testing.T) {
	var state imageSubmissionState
	if state.mayHaveSubmitted || !state.canProveNotSubmitted() {
		t.Fatal("fresh state must prove no provider submission")
	}

	// An explicit pre-submission failure performs no transition: false already
	// represents both a fresh request and any number of proven-safe refusals.
	if state.mayHaveSubmitted || !state.canProveNotSubmitted() {
		t.Fatal("pre-submission failure must leave mayHaveSubmitted false")
	}

	state.markMayHaveSubmitted()
	if !state.mayHaveSubmitted || state.canProveNotSubmitted() {
		t.Fatal("provider response or ambiguous I/O must disable safe replay")
	}

	state.markMayHaveSubmitted()
	if !state.mayHaveSubmitted || state.canProveNotSubmitted() {
		t.Fatal("mayHaveSubmitted must remain true forever")
	}
}
