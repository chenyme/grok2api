package egress

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/gin-gonic/gin"
)

func TestQualityGuardStatusReadsOnlyPublicState(t *testing.T) {
	path := t.TempDir() + "/state.json"
	state := `{"version":1,"started_at":10,"updated_at":20,"last_active_cycle_at":15,"last_passive_poll_at":19,"password":"must-not-leak","guard":{"mode":"hybrid","model":"grok-4.5","client_key_id":"6","node_ids":["8"],"active_interval_seconds":1800,"passive_poll_seconds":5,"soft_tps":500,"hard_tps":1000,"consecutive_soft":2,"consecutive_errors":2,"quarantine_seconds":300,"min_healthy_nodes":3,"max_output_tokens":384,"fail_closed":true,"min_generation_ms":1234,"prompt":"private-probe-prompt","expected":"private-marker"},"protected_node_ids":["9"],"nodes":{"8":{"active_soft_strikes":0,"passive_soft_strikes":0,"error_strikes":0,"quarantined_until":0,"disabled_by_guard":false,"last_reason":"","last_probe_at":15,"last_observed_at":19,"last_source":"passive","last_classification":"healthy","last_output_tps":42.5,"last_output_tokens":100,"last_first_token_ms":900,"last_duration_ms":4000}},"statistics":{"started_at":11,"active":{"total":7,"healthy":6,"soft":1,"hard":0,"errors":0,"output_tokens":1400},"passive":{"total":9,"healthy":8,"soft":0,"hard":1,"errors":0,"output_tokens":1800},"actions":{"quarantined":1,"restored":0,"suppressed":0}}}`
	if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/egress-quality-guard", nil)
	NewHandler(nil, path).qualityGuardStatus(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"available":true`) || !strings.Contains(recorder.Body.String(), `"last_output_tps":42.5`) || !strings.Contains(recorder.Body.String(), `"output_tokens":1400`) || !strings.Contains(recorder.Body.String(), `"protectedNodeIds":["9"]`) || !strings.Contains(recorder.Body.String(), `"fail_closed":true`) || !strings.Contains(recorder.Body.String(), `"min_generation_ms":1234`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "must-not-leak") || strings.Contains(recorder.Body.String(), "private-probe-prompt") || strings.Contains(recorder.Body.String(), "private-marker") || strings.Contains(recorder.Body.String(), "client_key_id") || !strings.Contains(recorder.Body.String(), `"recentEvents":[]`) {
		t.Fatalf("response leaked or omitted public defaults: %s", recorder.Body.String())
	}
}

func TestQualityGuardStatusIsOptional(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/egress-quality-guard", nil)
	NewHandler(nil).qualityGuardStatus(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"available":false`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestQualityProbeRoutesKeepAdminAndSidecarContractsSeparate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil)

	adminRouter := gin.New()
	handler.Register(adminRouter.Group(""))
	adminRecorder := httptest.NewRecorder()
	adminRouter.ServeHTTP(adminRecorder, httptest.NewRequest("POST", "/egress-nodes/1/quality-test", nil))
	if adminRecorder.Code != 400 || !strings.Contains(adminRecorder.Body.String(), `"code":"invalidRequest"`) {
		t.Fatalf("admin route status=%d body=%s", adminRecorder.Code, adminRecorder.Body.String())
	}

	internalRouter := gin.New()
	handler.RegisterQualityGuard(internalRouter.Group(""))
	internalRecorder := httptest.NewRecorder()
	internalRouter.ServeHTTP(internalRecorder, httptest.NewRequest("POST", "/egress-nodes/1/quality-test", nil))
	if internalRecorder.Code != 503 || !strings.Contains(internalRecorder.Body.String(), `"code":"qualityGuardUnavailable"`) {
		t.Fatalf("internal route status=%d body=%s", internalRecorder.Code, internalRecorder.Body.String())
	}
}

func TestQualityGuardStateAcceptsBoundedMultiMegabyteState(t *testing.T) {
	path := t.TempDir() + "/state.json"
	state := `{"version":1,"guard":{"mode":"active"},"nodes":{},"padding":"` + strings.Repeat("x", 2<<20) + `"}`
	if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	value, available, err := NewHandler(nil, path).readQualityGuardState()
	if err != nil || !available || value.Guard.Mode != "active" {
		t.Fatalf("available=%v mode=%q error=%v", available, value.Guard.Mode, err)
	}
}

func TestWriteQualityProbeErrorUsesSpecificSafeMessage(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	NewHandler(nil).writeQualityProbeError(context, errors.New("sensitive upstream failure"))
	if recorder.Code != 502 || !strings.Contains(recorder.Body.String(), `"code":"egressQualityProbeFailed"`) || !strings.Contains(recorder.Body.String(), "质量检测暂不可用") || strings.Contains(recorder.Body.String(), "sensitive upstream failure") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestWriteQualityProbeErrorIdentifiesMissingProbeAccount(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	NewHandler(nil).writeQualityProbeError(context, egressapp.ErrQualityProbeNoAccount)
	if recorder.Code != 503 || !strings.Contains(recorder.Body.String(), `"code":"egressQualityProbeNoAccount"`) || !strings.Contains(recorder.Body.String(), "暂无可调度账号") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestUpdateQualityGuardConfigWritesPrivateAtomicFile(t *testing.T) {
	directory := t.TempDir()
	statePath := directory + "/state.json"
	configPath := directory + "/runtime-config.json"
	state := `{"version":1,"guard":{"mode":"hybrid","model":"grok-4.5","client_key_id":"6","node_ids":["8","9","10","11","12"],"active_interval_seconds":1800,"passive_poll_seconds":5,"soft_tps":500,"hard_tps":1000,"consecutive_soft":2,"consecutive_errors":2,"quarantine_seconds":300,"min_healthy_nodes":3,"max_output_tokens":384,"prompt":"probe","expected":"QUALITY_OK"},"nodes":{}}`
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("PUT", "/egress-quality-guard/config", bytes.NewBufferString(`{"mode":"passive","activeIntervalSeconds":3600,"passivePollSeconds":10,"softTPS":400,"hardTPS":900,"consecutiveSoft":3,"consecutiveErrors":4,"quarantineSeconds":600,"minHealthyNodes":2}`))
	context.Request.Header.Set("Content-Type", "application/json")
	NewHandler(nil, statePath, configPath).updateQualityGuardConfig(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"saved":true`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"passive_poll_seconds":10`) || strings.Contains(string(data), "prompt") {
		t.Fatalf("runtime config = %s", data)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX permission bits: the os.WriteFile mode argument
	// is ignored and files always report 0666-style permissions.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("runtime config mode = %o", info.Mode().Perm())
	}
}

func TestUpdateQualityGuardConfigRejectsInvalidAndUnknownFields(t *testing.T) {
	directory := t.TempDir()
	statePath := directory + "/state.json"
	state := `{"version":1,"guard":{"mode":"hybrid","node_ids":["8","9"]},"nodes":{}}`
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"mode":"hybrid","activeIntervalSeconds":60,"passivePollSeconds":5,"softTPS":1000,"hardTPS":500,"consecutiveSoft":2,"consecutiveErrors":2,"quarantineSeconds":300,"minHealthyNodes":1}`,
		`{"mode":"hybrid","activeIntervalSeconds":60,"passivePollSeconds":5,"softTPS":500,"hardTPS":1000,"consecutiveSoft":2,"consecutiveErrors":2,"quarantineSeconds":300,"minHealthyNodes":1,"proxy":"forbidden"}`,
	} {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest("PUT", "/egress-quality-guard/config", bytes.NewBufferString(body))
		NewHandler(nil, statePath, directory+"/runtime-config.json").updateQualityGuardConfig(context)
		if recorder.Code != 400 {
			t.Fatalf("body=%s status=%d response=%s", body, recorder.Code, recorder.Body.String())
		}
	}
}

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

func TestLegacyEgressSourceListRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		path string
		want bool
	}{
		{path: "/egress-sources", want: true},
		{path: "/egress-sources?page=1", want: false},
		{path: "/egress-sources?pageSize=100", want: false},
		{path: "/egress-sources?search=alpha", want: false},
		{path: "/egress-sources?scope=grok_build", want: false},
	} {
		context, _ := gin.CreateTestContext(httptest.NewRecorder())
		context.Request = httptest.NewRequest("GET", test.path, nil)
		if got := legacyEgressSourceListRequest(context); got != test.want {
			t.Fatalf("legacyEgressSourceListRequest(%q) = %v, want %v", test.path, got, test.want)
		}
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
