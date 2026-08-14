package model

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
)

func TestSyncStreamsTerminalFailureInsteadOfAcknowledgingSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/admin/v1/models/sync", nil)

	handler := NewHandler(modelapp.NewService(nil, nil, nil, nil))
	handler.sync(context)

	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("content type = %q", contentType)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, ": connected\n\n") || !strings.Contains(body, "event: error\ndata: {\"code\":\"modelSyncFailed\"") {
		t.Fatalf("unexpected stream body: %q", body)
	}
	if strings.Contains(body, "event: complete") || strings.Contains(body, "\"synced\":0") {
		t.Fatalf("failure was acknowledged as success: %q", body)
	}
}

func TestNewModelResponseSeparatesPublicAndUpstreamNames(t *testing.T) {
	response := newModelResponse(modeldomain.Route{
		ID: 1, PublicID: "Build/grok-4.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	})
	if response.PublicID != "grok-4.5" || response.UpstreamModel != "Build/grok-4.5" {
		t.Fatalf("model response = %#v", response)
	}
}

func TestNewModelGroupResponseKeepsAllMemberRoutes(t *testing.T) {
	response := newModelGroupResponse(modelapp.RouteGroup{
		Routes: []modeldomain.Route{
			{ID: 10, PublicID: "Console/grok-imagine-image", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: modeldomain.CapabilityImage, Origin: modeldomain.OriginCatalog},
			{ID: 11, PublicID: "Console/grok-imagine-image", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: modeldomain.CapabilityImageEdit, Origin: modeldomain.OriginCatalog},
		},
		EndpointCapabilities: []string{"image", "image_edit"},
	})
	if response.Key != "10:11" || len(response.Routes) != 2 || len(response.EndpointCapabilities) != 2 {
		t.Fatalf("model group response = %#v", response)
	}
}

func TestNewModelResponseKnowsStaticConsoleCapabilityWithoutAccountSync(t *testing.T) {
	response := newModelResponse(modeldomain.Route{
		ID: 2, PublicID: "Console/grok-imagine-image", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image",
		Capability: modeldomain.CapabilityImageEdit, Enabled: true, SupportedAccounts: 20, TotalAccounts: 20,
	})
	if !response.CapabilityKnown || !response.Available || response.BindingMode || response.SupportedAccounts != 20 {
		t.Fatalf("static Console model response = %#v", response)
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
