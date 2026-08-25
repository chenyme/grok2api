package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

func TestQualityLeaseCursorRoundTripAndValidation(t *testing.T) {
	want := accountdomain.EgressLeaseBlock{
		AccountID:     42,
		NodeID:        9,
		CooldownUntil: time.Date(2026, time.August, 19, 8, 7, 6, 123456789, time.UTC),
	}
	encoded := encodeQualityLeaseCursor(want)
	got, err := decodeQualityLeaseCursor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != want.AccountID || got.NodeID != want.NodeID || !got.CooldownUntil.Equal(want.CooldownUntil) {
		t.Fatalf("cursor = %#v, want %#v", got, want)
	}
	for _, raw := range []string{
		"not-base64",
		"eyJ0IjoxLCJhIjowLCJuIjoxfQ",
		"eyJ0IjoxLCJhIjoxLCJuIjoxfXsic2Vjb25kIjp0cnVlfQ",
	} {
		if _, err := decodeQualityLeaseCursor(raw); err == nil {
			t.Fatalf("decodeQualityLeaseCursor(%q) succeeded", raw)
		}
	}
}

type proxyRevealRepository struct {
	node        egressdomain.Node
	profile     egressdomain.ProxyProfile
	profilePage repository.PageQuery
}

func (r *proxyRevealRepository) ListEgressNodes(context.Context, egressdomain.Scope, repository.SortQuery) ([]egressdomain.Node, error) {
	return []egressdomain.Node{r.node}, nil
}
func (r *proxyRevealRepository) ListEgressNodePage(context.Context, repository.EgressNodeListQuery) ([]egressdomain.Node, int64, error) {
	return []egressdomain.Node{r.node}, 1, nil
}
func (r *proxyRevealRepository) GetEgressNode(_ context.Context, id uint64) (egressdomain.Node, error) {
	if id != r.node.ID {
		return egressdomain.Node{}, repository.ErrNotFound
	}
	return r.node, nil
}
func (r *proxyRevealRepository) CreateEgressNode(_ context.Context, value egressdomain.Node) (egressdomain.Node, error) {
	return value, nil
}
func (r *proxyRevealRepository) UpdateEgressNode(_ context.Context, value egressdomain.Node) (egressdomain.Node, error) {
	return value, nil
}
func (r *proxyRevealRepository) DeleteEgressNode(context.Context, uint64) error { return nil }
func (r *proxyRevealRepository) ListEgressProxyProfiles(_ context.Context, page repository.PageQuery) ([]egressdomain.ProxyProfile, int64, error) {
	r.profilePage = page
	return []egressdomain.ProxyProfile{r.profile}, 1, nil
}
func (r *proxyRevealRepository) GetEgressProxyProfile(_ context.Context, id uint64) (egressdomain.ProxyProfile, error) {
	if id != r.profile.ID {
		return egressdomain.ProxyProfile{}, repository.ErrNotFound
	}
	return r.profile, nil
}
func (r *proxyRevealRepository) CreateEgressProxyProfile(_ context.Context, value egressdomain.ProxyProfile) (egressdomain.ProxyProfile, error) {
	return value, nil
}
func (r *proxyRevealRepository) UpdateEgressProxyProfile(_ context.Context, value egressdomain.ProxyProfile, _ bool) (egressdomain.ProxyProfile, []uint64, error) {
	return value, nil, nil
}
func (r *proxyRevealRepository) DeleteEgressProxyProfile(context.Context, uint64) error { return nil }

func TestProxyURLRevealIsExplicitAndNeverCacheable(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := "socks5h://user:secret@proxy.example:1080"
	encrypted, err := cipher.Encrypt(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	service := egressapp.NewService(&proxyRevealRepository{node: egressdomain.Node{ID: 7, EncryptedProxyURL: encrypted}}, cipher, "")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "id", Value: "7"}}
	context.Request = httptest.NewRequest("POST", "/egress-nodes/7/proxy-url/reveal", nil)
	NewHandler(service).proxyURL(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "private, no-store" || recorder.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("reveal cache headers = %#v", recorder.Header())
	}
}

func TestProxyProfileURLRevealIsExplicitAndNeverCacheable(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("http://profile-user:profile-secret@proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	service := egressapp.NewService(&proxyRevealRepository{profile: egressdomain.ProxyProfile{ID: 9, EncryptedProxyURL: encrypted}}, cipher, "")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "id", Value: "9"}}
	context.Request = httptest.NewRequest("POST", "/egress-proxy-profiles/9/proxy-url/reveal", nil)
	NewHandler(service).proxyProfileURL(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "profile-secret") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "private, no-store" || recorder.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("reveal cache headers = %#v", recorder.Header())
	}
}

func TestProxyProfileListUsesBoundedPaginationAndSearch(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &proxyRevealRepository{profile: egressdomain.ProxyProfile{ID: 9, Name: "Tokyo", EncryptedProxyURL: "invalid-but-redacted"}}
	service := egressapp.NewService(repository, cipher, "")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/egress-proxy-profiles?page=2&pageSize=5&search=tokyo", nil)
	NewHandler(service).listProxyProfiles(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"page":2`) || !strings.Contains(recorder.Body.String(), `"pageSize":5`) || !strings.Contains(recorder.Body.String(), `"total":1`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if repository.profilePage.Offset != 5 || repository.profilePage.Limit != 5 || repository.profilePage.Search != "tokyo" {
		t.Fatalf("page query = %#v", repository.profilePage)
	}
}

func TestQualityGuardStatusReadsOnlyPublicState(t *testing.T) {
	path := t.TempDir() + "/state.json"
	state := `{"version":1,"started_at":10,"updated_at":20,"last_active_cycle_at":15,"last_passive_poll_at":19,"password":"must-not-leak","guard":{"mode":"hybrid","model":"grok-4.5","client_key_id":"6","node_ids":["8"],"active_interval_seconds":1800,"passive_poll_seconds":5,"soft_tps":500,"hard_tps":1000,"consecutive_soft":2,"consecutive_errors":2,"quarantine_seconds":300,"min_healthy_nodes":3,"max_output_tokens":384,"fail_closed":true,"min_generation_ms":1234,"prompt":"private-probe-prompt","expected":"private-marker"},"protected_node_ids":["9"],"nodes":{"8":{"observe_only":true,"observe_only_reason":"account_bound_proxy","active_soft_strikes":0,"passive_soft_strikes":0,"error_strikes":0,"quarantined_until":0,"disabled_by_guard":false,"last_reason":"","last_probe_at":15,"last_observed_at":19,"last_source":"passive","last_classification":"healthy","last_output_tps":42.5,"last_output_tokens":100,"last_first_token_ms":900,"last_duration_ms":4000}},"statistics":{"started_at":11,"active":{"total":7,"healthy":6,"soft":1,"hard":0,"errors":0,"output_tokens":1400},"passive":{"total":9,"healthy":8,"soft":0,"hard":1,"errors":0,"output_tokens":1800},"actions":{"quarantined":1,"restored":0,"suppressed":0}}}`
	if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/egress-quality-guard", nil)
	NewHandler(nil, path).qualityGuardStatus(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"available":true`) || !strings.Contains(recorder.Body.String(), `"observe_only":true`) || !strings.Contains(recorder.Body.String(), `"observe_only_reason":"account_bound_proxy"`) || !strings.Contains(recorder.Body.String(), `"last_output_tps":42.5`) || !strings.Contains(recorder.Body.String(), `"output_tokens":1400`) || !strings.Contains(recorder.Body.String(), `"protectedNodeIds":["9"]`) || !strings.Contains(recorder.Body.String(), `"fail_closed":true`) || !strings.Contains(recorder.Body.String(), `"min_generation_ms":1234`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "must-not-leak") || strings.Contains(recorder.Body.String(), "private-probe-prompt") || strings.Contains(recorder.Body.String(), "private-marker") || strings.Contains(recorder.Body.String(), "client_key_id") || !strings.Contains(recorder.Body.String(), `"recentEvents":[]`) {
		t.Fatalf("response leaked or omitted public defaults: %s", recorder.Body.String())
	}
}

func TestQualityGuardStatusFiltersNodeDetailsAndKeepsGlobalSummary(t *testing.T) {
	path := t.TempDir() + "/state.json"
	state := `{"version":1,"started_at":10,"updated_at":20,"guard":{"mode":"hybrid","model":"grok-4.5","node_ids":["8","9"],"active_interval_seconds":1800,"passive_poll_seconds":5,"soft_tps":500,"hard_tps":1000,"consecutive_soft":2,"consecutive_errors":2,"quarantine_seconds":300,"min_healthy_nodes":1,"max_output_tokens":384},"nodes":{"8":{"active_soft_strikes":0,"passive_soft_strikes":0,"error_strikes":0,"quarantined_until":0,"disabled_by_guard":false,"last_reason":"","last_probe_at":15,"last_observed_at":19,"last_source":"passive","last_classification":"healthy","last_output_tps":42.5,"last_output_tokens":100,"last_first_token_ms":900,"last_duration_ms":4000},"9":{"quarantined_lease_count":2,"active_soft_strikes":0,"passive_soft_strikes":0,"error_strikes":0,"quarantined_until":30,"disabled_by_guard":true,"last_reason":"hard_tps","last_probe_at":16,"last_observed_at":20,"last_source":"active","last_classification":"hard","last_output_tps":5,"last_output_tokens":20,"last_first_token_ms":1200,"last_duration_ms":5000}}}`
	if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/egress-quality-guard?nodeId=8", nil)
	NewHandler(nil, path).qualityGuardStatus(context)
	if recorder.Code != 200 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	type statusPayload struct {
		Data struct {
			Nodes       map[string]qualityGuardNodeState `json:"nodes"`
			NodeSummary struct {
				Total             int `json:"total"`
				Quarantined       int `json:"quarantined"`
				QuarantinedLeases int `json:"quarantinedLeases"`
			} `json:"nodeSummary"`
		} `json:"data"`
	}
	var payload statusPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.Nodes) != 1 || payload.Data.Nodes["8"].LastClassification != "healthy" {
		t.Fatalf("filtered nodes = %#v", payload.Data.Nodes)
	}
	if payload.Data.NodeSummary.Total != 2 || payload.Data.NodeSummary.Quarantined != 1 || payload.Data.NodeSummary.QuarantinedLeases != 2 {
		t.Fatalf("node summary = %#v", payload.Data.NodeSummary)
	}

	emptyRecorder := httptest.NewRecorder()
	emptyContext, _ := gin.CreateTestContext(emptyRecorder)
	emptyContext.Request = httptest.NewRequest("GET", "/egress-quality-guard?nodeId=", nil)
	NewHandler(nil, path).qualityGuardStatus(emptyContext)
	if emptyRecorder.Code != 200 {
		t.Fatalf("empty page status=%d body=%s", emptyRecorder.Code, emptyRecorder.Body.String())
	}
	var emptyPayload statusPayload
	if err := json.Unmarshal(emptyRecorder.Body.Bytes(), &emptyPayload); err != nil {
		t.Fatal(err)
	}
	if len(emptyPayload.Data.Nodes) != 0 || emptyPayload.Data.NodeSummary.Total != 2 {
		t.Fatalf("empty page nodes=%#v summary=%#v", emptyPayload.Data.Nodes, emptyPayload.Data.NodeSummary)
	}
}

func TestSelectedQualityGuardNodesRejectsInvalidOrOversizedFilters(t *testing.T) {
	values := map[string]qualityGuardNodeState{"8": {LastClassification: "healthy"}}
	if _, err := selectedQualityGuardNodes(values, []string{"not-a-number"}); err == nil {
		t.Fatal("expected invalid node ID to be rejected")
	}
	oversized := make([]string, 201)
	for index := range oversized {
		oversized[index] = "8"
	}
	if _, err := selectedQualityGuardNodes(values, oversized); err == nil {
		t.Fatal("expected oversized node filter to be rejected")
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

func TestQualityGuardStateCachesUnchangedFileAndRefreshesAfterUpdate(t *testing.T) {
	path := t.TempDir() + "/state.json"
	firstJSON := `{"version":1,"updated_at":20,"guard":{"mode":"active"},"nodes":{"8":{"disabled_by_guard":true,"quarantined_lease_count":2}},"recent_events":[{"ts":20,"event":"node_quarantined","node_id":"8"}]}`
	if err := os.WriteFile(path, []byte(firstJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(nil, path)
	first, available, err := handler.readQualityGuardState()
	if err != nil || !available || len(first.RecentEvents) != 1 {
		t.Fatalf("first read available=%v state=%#v error=%v", available, first, err)
	}
	second, available, err := handler.readQualityGuardState()
	if err != nil || !available || len(second.RecentEvents) != 1 {
		t.Fatalf("second read available=%v state=%#v error=%v", available, second, err)
	}
	if &first.RecentEvents[0] != &second.RecentEvents[0] {
		t.Fatal("unchanged state was decoded again instead of using the cache")
	}
	if second.NodeSummary.Total != 1 || second.NodeSummary.Quarantined != 1 || second.NodeSummary.QuarantinedLeases != 2 {
		t.Fatalf("cached node summary = %#v", second.NodeSummary)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON := `{"version":1,"updated_at":30,"guard":{"mode":"active"},"nodes":{"9":{"disabled_by_guard":false}},"recent_events":[{"ts":30,"event":"node_restored","node_id":"9"}]}`
	if err := os.WriteFile(path, []byte(secondJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	updatedTime := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, updatedTime, updatedTime); err != nil {
		t.Fatal(err)
	}
	updated, available, err := handler.readQualityGuardState()
	if err != nil || !available || updated.UpdatedAt != 30 {
		t.Fatalf("updated read available=%v state=%#v error=%v", available, updated, err)
	}
	if updated.NodeSummary.Total != 1 || updated.NodeSummary.Quarantined != 0 || updated.NodeSummary.QuarantinedLeases != 0 {
		t.Fatalf("updated node summary = %#v", updated.NodeSummary)
	}

	updatedInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacementJSON := `{"version":1,"updated_at":40,"guard":{"mode":"active"},"nodes":{"7":{"disabled_by_guard":false}},"recent_events":[{"ts":40,"event":"node_restored","node_id":"7"}]}`
	if len(replacementJSON) != len(secondJSON) {
		t.Fatalf("replacement fixture size = %d, want %d", len(replacementJSON), len(secondJSON))
	}
	replacementPath := path + ".replacement"
	if err := os.WriteFile(replacementPath, []byte(replacementJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacementPath, updatedInfo.ModTime(), updatedInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, path); err != nil {
		t.Fatal(err)
	}
	replaced, available, err := handler.readQualityGuardState()
	if err != nil || !available || replaced.UpdatedAt != 40 {
		t.Fatalf("replaced read available=%v state=%#v error=%v", available, replaced, err)
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

func TestQualityGuardProfilesCRUDAndStatusOmitsPrompt(t *testing.T) {
	directory := t.TempDir()
	statePath := directory + "/state.json"
	configPath := directory + "/runtime-config.json"
	if err := os.WriteFile(statePath, []byte(`{"version":1,"guard":{"mode":"hybrid","model":"grok-4.5","node_ids":["8"]},"nodes":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(nil, statePath, configPath)

	createRecorder := httptest.NewRecorder()
	createContext, _ := gin.CreateTestContext(createRecorder)
	createContext.Request = httptest.NewRequest("POST", "/egress-quality-guard/profiles", bytes.NewBufferString(`{"name":"自定义标记","prompt":"只输出 FLAG_OK","expectedText":"FLAG_OK","matchMode":"last_line","requireThinking":true,"active":true}`))
	createContext.Request.Header.Set("Content-Type", "application/json")
	handler.createQualityGuardProfile(createContext)
	if createRecorder.Code != 200 || !strings.Contains(createRecorder.Body.String(), `"FLAG_OK"`) || !strings.Contains(createRecorder.Body.String(), `"require_thinking":true`) {
		t.Fatalf("create status=%d body=%s", createRecorder.Code, createRecorder.Body.String())
	}

	statusRecorder := httptest.NewRecorder()
	statusContext, _ := gin.CreateTestContext(statusRecorder)
	statusContext.Request = httptest.NewRequest("GET", "/egress-quality-guard", nil)
	handler.qualityGuardStatus(statusContext)
	body := statusRecorder.Body.String()
	if statusRecorder.Code != 200 || !strings.Contains(body, `"activeProfileId":"p-2"`) || !strings.Contains(body, `"has_expected":true`) || !strings.Contains(body, `"require_thinking":true`) {
		t.Fatalf("status=%d body=%s", statusRecorder.Code, body)
	}
	if strings.Contains(body, "只输出 FLAG_OK") || strings.Contains(body, "FLAG_OK") {
		t.Fatalf("status leaked probe prompt or marker: %s", body)
	}
}

func TestQualityGuardProfileWritesAreSerialized(t *testing.T) {
	directory := t.TempDir()
	handler := NewHandler(nil, "", directory+"/runtime-config.json")
	const count = 32
	var wait sync.WaitGroup
	errorsFound := make(chan string, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			body := fmt.Sprintf(`{"name":"profile-%d","prompt":"probe-%d","matchMode":"contains"}`, index, index)
			context.Request = httptest.NewRequest("POST", "/egress-quality-guard/profiles", bytes.NewBufferString(body))
			context.Request.Header.Set("Content-Type", "application/json")
			handler.createQualityGuardProfile(context)
			if recorder.Code != 200 {
				errorsFound <- recorder.Body.String()
			}
		}(index)
	}
	wait.Wait()
	close(errorsFound)
	for message := range errorsFound {
		t.Fatalf("concurrent profile create failed: %s", message)
	}
	data, err := loadProbeProfileFile(directory + "/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	custom := 0
	for _, profile := range data.Profiles {
		if !profile.BuiltIn {
			custom++
		}
	}
	if custom != count {
		t.Fatalf("custom profiles = %d, want %d", custom, count)
	}
}

func TestQualityGuardReservedProfilesAreCanonicalized(t *testing.T) {
	directory := t.TempDir()
	path := directory + "/profiles.json"
	forged := `{"version":1,"active_profile_id":"quality-marker","profiles":{"quality-marker":{"id":"quality-marker","name":"forged","built_in":false,"prompt":"skip checks","expected_text":"","match_mode":"contains","require_thinking":false},"throughput":{"id":"throughput","name":"forged","built_in":false,"prompt":"skip checks","expected_text":"PASS","match_mode":"regex","require_thinking":true}}}`
	if err := os.WriteFile(path, []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := loadProbeProfileFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := data.Profiles[profileQualityMarker]
	if !marker.BuiltIn || marker.ExpectedText != "QUALITY_OK" || marker.MatchMode != egressapp.MatchLastLine || !marker.RequireThinking {
		t.Fatalf("quality marker was not canonicalized: %#v", marker)
	}
	throughput := data.Profiles[profileThroughput]
	if !throughput.BuiltIn || throughput.ExpectedText != "" || throughput.MatchMode != egressapp.MatchContains || throughput.RequireThinking {
		t.Fatalf("throughput profile was not canonicalized: %#v", throughput)
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
