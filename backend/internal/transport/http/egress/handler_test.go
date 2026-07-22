package egress

import (
	"errors"
	"testing"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestOperationsConfigRequestParsesFallbacks(t *testing.T) {
	input, err := (operationsConfigRequest{
		ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		Fallbacks: map[string]operationsFallbackRequest{
			"grok_build": {Mode: "fixed", NodeID: "42"},
			"grok_web":   {Mode: "direct"},
		},
	}).input()
	if err != nil {
		t.Fatal(err)
	}
	if fallback := input.Fallbacks[egressdomain.ScopeBuild]; fallback.Mode != egressdomain.FallbackModeFixed || fallback.NodeID != 42 {
		t.Fatalf("Build fallback = %#v", fallback)
	}
	if fallback := input.Fallbacks[egressdomain.ScopeWeb]; fallback.Mode != egressdomain.FallbackModeDirect || fallback.NodeID != 0 {
		t.Fatalf("Web fallback = %#v", fallback)
	}
}

func TestOperationsConfigRequestRejectsInvalidFallbackNodeID(t *testing.T) {
	_, err := (operationsConfigRequest{
		Fallbacks: map[string]operationsFallbackRequest{"grok_build": {Mode: "fixed", NodeID: "zero"}},
	}).input()
	if !errors.Is(err, egressapp.ErrInvalidInput) {
		t.Fatalf("invalid node ID error = %v", err)
	}
}
