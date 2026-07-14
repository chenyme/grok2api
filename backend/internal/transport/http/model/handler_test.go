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
