package egress

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/gin-gonic/gin"
)

func TestBatchNodeUpdateRequestRequiresEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name    string
		body    string
		wantErr bool
		want    bool
	}{
		{name: "missing", body: `{"ids":["1"]}`, wantErr: true},
		{name: "explicit false", body: `{"ids":["1"],"enabled":false}`, want: false},
		{name: "explicit true", body: `{"ids":["1"],"enabled":true}`, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Request = httptest.NewRequest("PATCH", "/egress-nodes/batch", bytes.NewBufferString(test.body))
			context.Request.Header.Set("Content-Type", "application/json")
			var request batchNodeUpdateRequest
			err := context.ShouldBindJSON(&request)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected binding error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if request.Enabled == nil || *request.Enabled != test.want {
				t.Fatalf("enabled = %v, want %v", request.Enabled, test.want)
			}
		})
	}
}

func TestUpdateManyRejectsMissingEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("PATCH", "/egress-nodes/batch", bytes.NewBufferString(`{"ids":["1"]}`))
	context.Request.Header.Set("Content-Type", "application/json")

	(&Handler{}).updateMany(context)

	if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "invalidRequest") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestParseBoundedEgressNodeIDsChecksRawInputLength(t *testing.T) {
	values := make([]string, 5001)
	for index := range values {
		values[index] = "1"
	}
	if _, err := parseBoundedEgressNodeIDs(values, 5000); err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatalf("oversized duplicate input error = %v", err)
	}
	ids, err := parseBoundedEgressNodeIDs([]string{"2", "2", "1"}, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 1 {
		t.Fatalf("ids = %v", ids)
	}
}

func TestNewNodeResponseIncludesIPv4AndIPv6ProbeDetails(t *testing.T) {
	testedAt := time.Now().UTC().Truncate(time.Second)
	response := newNodeResponse(egressdomain.PublicNode{
		ProbeStatus:   egressdomain.ProbeStatusHealthy,
		ProbeProvider: egressdomain.ProbeProviderCloudflare,
		IPv4Probe: egressdomain.ProbeFamilyResult{
			Status: egressdomain.ProbeStatusHealthy, TestedAt: testedAt, LatencyMS: 21, ExitIP: "198.51.100.2",
		},
		IPv6Probe: egressdomain.ProbeFamilyResult{
			Status: egressdomain.ProbeStatusUnhealthy, TestedAt: testedAt, LatencyMS: 48, Error: "代理连接失败",
		},
	})
	if response.ProbeProvider != "cloudflare" || response.IPv4Probe.ExitIP != "198.51.100.2" || response.IPv4Probe.TestedAt == nil || response.IPv6Probe.Status != "unhealthy" || response.IPv6Probe.Error == "" {
		t.Fatalf("node response = %#v", response)
	}
}

func TestOperationsConfigRequestParsesFallbacks(t *testing.T) {
	input, err := (operationsConfigRequest{
		ProbeProvider: "cloudflare", ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
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
	if input.ProbeProvider != egressdomain.ProbeProviderCloudflare {
		t.Fatalf("probe provider = %q", input.ProbeProvider)
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
