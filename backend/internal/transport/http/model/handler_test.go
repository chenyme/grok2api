package model

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

func TestNewModelResponseSeparatesPublicAndUpstreamNames(t *testing.T) {
	response := newModelResponse(modeldomain.Route{
		ID: 1, PublicID: "Build/grok-4.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	})
	if response.PublicID != "grok-4.5" || response.UpstreamModel != "Build/grok-4.5" {
		t.Fatalf("model response = %#v", response)
	}
}

func TestParseOptionalBoolRejectsAmbiguousValues(t *testing.T) {
	for _, test := range []struct {
		input string
		value bool
		valid bool
	}{
		{input: "", valid: true},
		{input: "false", valid: true},
		{input: "true", value: true, valid: true},
		{input: "1", valid: false},
		{input: "yes", valid: false},
	} {
		value, valid := parseOptionalBool(test.input)
		if value != test.value || valid != test.valid {
			t.Fatalf("parseOptionalBool(%q) = (%v, %v), want (%v, %v)", test.input, value, valid, test.value, test.valid)
		}
	}
}
