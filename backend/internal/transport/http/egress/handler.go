package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service         *egressapp.Service
	guardStatePath  string
	guardConfigPath string
	guardProbe      egressapp.QualityProbeInput
	profilesMu      sync.Mutex
}

func NewHandler(service *egressapp.Service, guardStatePath ...string) *Handler {
	path := ""
	if len(guardStatePath) > 0 {
		path = strings.TrimSpace(guardStatePath[0])
	}
	configPath := ""
	if len(guardStatePath) > 1 {
		configPath = strings.TrimSpace(guardStatePath[1])
	}
	return &Handler{service: service, guardStatePath: path, guardConfigPath: configPath}
}

// WithQualityGuardProbe pins the sidecar probe to server-owned credentials and
// payload. The internal caller can select only the physical egress node.
func (h *Handler) WithQualityGuardProbe(input egressapp.QualityProbeInput) *Handler {
	h.guardProbe = input
	return h
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/egress-nodes", h.list)
	router.POST("/egress-nodes", h.create)
	router.PATCH("/egress-nodes/batch", h.updateMany)
	router.DELETE("/egress-nodes", h.deleteMany)
	router.GET("/egress-nodes/cleanup-preview", h.cleanupPreview)
	router.POST("/egress-nodes/cleanup", h.cleanup)
	router.POST("/egress-nodes/test", h.testNodes)
	router.POST("/egress-nodes/:id/test", h.testNode)
	router.POST("/egress-nodes/:id/quality-test", h.testQuality)
	router.GET("/egress-quality-guard", h.qualityGuardStatus)
	router.PUT("/egress-quality-guard/config", h.updateQualityGuardConfig)
	router.GET("/egress-quality-guard/profiles", h.listQualityGuardProfiles)
	router.POST("/egress-quality-guard/profiles", h.createQualityGuardProfile)
	router.PUT("/egress-quality-guard/profiles/:id", h.updateQualityGuardProfile)
	router.DELETE("/egress-quality-guard/profiles/:id", h.deleteQualityGuardProfile)
	router.POST("/egress-quality-guard/nodes/:id/test", h.testQualityGuardNode)
	router.POST("/egress-nodes/:id/accounts", h.assignAccounts)
	router.DELETE("/egress-nodes/accounts", h.unassignAccounts)
	router.PUT("/egress-nodes/:id", h.update)
	router.POST("/egress-nodes/:id/refresh-clearance", h.refreshClearance)
	router.DELETE("/egress-nodes/:id", h.delete)
	router.POST("/egress-imports", h.importText)
	router.GET("/egress-sources", h.listSources)
	router.POST("/egress-sources", h.createSource)
	router.POST("/egress-sources/:id/sync", h.syncSource)
	router.PUT("/egress-sources/:id", h.updateSource)
	router.DELETE("/egress-sources/:id", h.deleteSource)
	router.GET("/egress-operations", h.operationsConfig)
	router.PUT("/egress-operations", h.updateOperationsConfig)
	router.POST("/egress-operations/rebalance", h.rebalance)
}

// RegisterQualityGuard exposes the minimum egress surface required by the
// sidecar. Destructive node, source, binding, and credential operations remain
// available only through administrator authentication.
func (h *Handler) RegisterQualityGuard(router *gin.RouterGroup) {
	router.GET("/egress-nodes", h.list)
	router.PATCH("/egress-nodes/batch", h.updateMany)
	router.POST("/egress-nodes/:id/test", h.testNode)
	router.POST("/egress-nodes/:id/quality-test", h.testQualityGuardNode)
	router.GET("/egress-operations", h.operationsConfig)
}

// A fully populated 2,000-node guard state is slightly larger than 1 MiB.
// Keep a bounded limit while leaving headroom for audit cursors and events.
const maxQualityGuardStateBytes = 8 << 20

type qualityGuardState struct {
	Version           int                              `json:"version"`
	StartedAt         float64                          `json:"started_at"`
	UpdatedAt         float64                          `json:"updated_at"`
	LastActiveCycleAt float64                          `json:"last_active_cycle_at"`
	LastPassivePollAt float64                          `json:"last_passive_poll_at"`
	Guard             qualityGuardConfig               `json:"guard"`
	ProtectedNodeIDs  []string                         `json:"protected_node_ids"`
	Nodes             map[string]qualityGuardNodeState `json:"nodes"`
	RecentEvents      []qualityGuardEvent              `json:"recent_events"`
	Statistics        qualityGuardStatistics           `json:"statistics"`
}

type qualityGuardStatistics struct {
	StartedAt float64                    `json:"started_at"`
	Active    qualityGuardDetectionStats `json:"active"`
	Passive   qualityGuardDetectionStats `json:"passive"`
	Actions   qualityGuardActionStats    `json:"actions"`
}

type qualityGuardDetectionStats struct {
	Total        uint64 `json:"total"`
	Healthy      uint64 `json:"healthy"`
	Soft         uint64 `json:"soft"`
	Hard         uint64 `json:"hard"`
	Errors       uint64 `json:"errors"`
	OutputTokens uint64 `json:"output_tokens"`
}

type qualityGuardActionStats struct {
	Quarantined uint64 `json:"quarantined"`
	Restored    uint64 `json:"restored"`
	Suppressed  uint64 `json:"suppressed"`
}

type qualityGuardConfig struct {
	Mode                  string   `json:"mode"`
	Model                 string   `json:"model"`
	NodeIDs               []string `json:"node_ids"`
	ActiveIntervalSeconds int      `json:"active_interval_seconds"`
	PassivePollSeconds    int      `json:"passive_poll_seconds"`
	SoftTPS               float64  `json:"soft_tps"`
	HardTPS               float64  `json:"hard_tps"`
	ConsecutiveSoft       int      `json:"consecutive_soft"`
	ConsecutiveErrors     int      `json:"consecutive_errors"`
	QuarantineSeconds     int      `json:"quarantine_seconds"`
	MinHealthyNodes       int      `json:"min_healthy_nodes"`
	MaxOutputTokens       int      `json:"max_output_tokens"`
	FailClosed            bool     `json:"fail_closed"`
	MinGenerationMS       int      `json:"min_generation_ms"`
	Prompt                string   `json:"prompt"`
	Expected              string   `json:"expected"`
}

type qualityGuardNodeState struct {
	ActiveSoftStrikes  int     `json:"active_soft_strikes"`
	PassiveSoftStrikes int     `json:"passive_soft_strikes"`
	ErrorStrikes       int     `json:"error_strikes"`
	QuarantinedUntil   float64 `json:"quarantined_until"`
	DisabledByGuard    bool    `json:"disabled_by_guard"`
	LastReason         string  `json:"last_reason"`
	LastProbeAt        float64 `json:"last_probe_at"`
	LastObservedAt     float64 `json:"last_observed_at"`
	LastSource         string  `json:"last_source"`
	LastClassification string  `json:"last_classification"`
	LastOutputTPS      float64 `json:"last_output_tps"`
	LastOutputTokens   int     `json:"last_output_tokens"`
	LastFirstTokenMS   int     `json:"last_first_token_ms"`
	LastDurationMS     int     `json:"last_duration_ms"`
}

type qualityGuardEvent struct {
	TS             float64 `json:"ts"`
	Event          string  `json:"event"`
	NodeID         string  `json:"node_id"`
	NodeName       string  `json:"node_name"`
	Reason         string  `json:"reason"`
	Classification string  `json:"classification"`
	OutputTPS      float64 `json:"output_tps"`
}

func (h *Handler) qualityGuardStatus(c *gin.Context) {
	state, available, err := h.readQualityGuardState()
	if !available {
		response.Success(c, http.StatusOK, gin.H{"available": false})
		return
	}
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "质量守护状态暂不可用")
		return
	}
	payload := gin.H{
		"available":         true,
		"editable":          h.guardConfigPath != "",
		"startedAt":         state.StartedAt,
		"updatedAt":         state.UpdatedAt,
		"lastActiveCycleAt": state.LastActiveCycleAt,
		"lastPassivePollAt": state.LastPassivePollAt,
		"config": gin.H{
			"mode": state.Guard.Mode, "model": state.Guard.Model,
			"node_ids": state.Guard.NodeIDs, "active_interval_seconds": state.Guard.ActiveIntervalSeconds,
			"passive_poll_seconds": state.Guard.PassivePollSeconds, "soft_tps": state.Guard.SoftTPS,
			"hard_tps": state.Guard.HardTPS, "consecutive_soft": state.Guard.ConsecutiveSoft,
			"consecutive_errors": state.Guard.ConsecutiveErrors, "quarantine_seconds": state.Guard.QuarantineSeconds,
			"min_healthy_nodes": state.Guard.MinHealthyNodes, "max_output_tokens": state.Guard.MaxOutputTokens,
			"fail_closed": state.Guard.FailClosed, "min_generation_ms": state.Guard.MinGenerationMS,
		},
		"nodes": state.Nodes, "protectedNodeIds": state.ProtectedNodeIDs, "recentEvents": state.RecentEvents,
	}
	if state.Statistics.StartedAt > 0 {
		payload["statistics"] = state.Statistics
	}
	if profiles, err := loadProbeProfileFile(h.profilesPath()); err == nil {
		payload["activeProfileId"] = profiles.ActiveProfileID
		payload["profiles"] = profiles.summaries()
	}
	response.Success(c, http.StatusOK, payload)
}

type qualityGuardConfigRequest struct {
	Mode                  string  `json:"mode"`
	ActiveIntervalSeconds int     `json:"activeIntervalSeconds"`
	PassivePollSeconds    int     `json:"passivePollSeconds"`
	SoftTPS               float64 `json:"softTPS"`
	HardTPS               float64 `json:"hardTPS"`
	ConsecutiveSoft       int     `json:"consecutiveSoft"`
	ConsecutiveErrors     int     `json:"consecutiveErrors"`
	QuarantineSeconds     int     `json:"quarantineSeconds"`
	MinHealthyNodes       int     `json:"minHealthyNodes"`
}

type qualityGuardRuntimeConfigFile struct {
	Version  int                               `json:"version"`
	Settings qualityGuardRuntimeConfigSettings `json:"settings"`
}

type qualityGuardRuntimeConfigSettings struct {
	Mode                  string  `json:"mode"`
	ActiveIntervalSeconds int     `json:"active_interval_seconds"`
	PassivePollSeconds    int     `json:"passive_poll_seconds"`
	SoftTPS               float64 `json:"soft_tps"`
	HardTPS               float64 `json:"hard_tps"`
	ConsecutiveSoft       int     `json:"consecutive_soft"`
	ConsecutiveErrors     int     `json:"consecutive_errors"`
	QuarantineSeconds     int     `json:"quarantine_seconds"`
	MinHealthyNodes       int     `json:"min_healthy_nodes"`
}

func (h *Handler) updateQualityGuardConfig(c *gin.Context) {
	if h.guardConfigPath == "" {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardReadOnly", "质量守护策略当前只读")
		return
	}
	state, available, err := h.readQualityGuardState()
	if err != nil || !available {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "质量守护状态暂不可用")
		return
	}
	var request qualityGuardConfigRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	if err := request.validate(len(state.Guard.NodeIDs)); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidQualityGuardConfig", err.Error())
		return
	}
	value := qualityGuardRuntimeConfigFile{Version: 1, Settings: qualityGuardRuntimeConfigSettings{
		Mode: request.Mode, ActiveIntervalSeconds: request.ActiveIntervalSeconds,
		PassivePollSeconds: request.PassivePollSeconds, SoftTPS: request.SoftTPS, HardTPS: request.HardTPS,
		ConsecutiveSoft: request.ConsecutiveSoft, ConsecutiveErrors: request.ConsecutiveErrors,
		QuarantineSeconds: request.QuarantineSeconds, MinHealthyNodes: request.MinHealthyNodes,
	}}
	if err := saveQualityGuardRuntimeConfig(h.guardConfigPath, value); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardConfigWriteFailed", "质量守护策略保存失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"saved": true})
}

func (r qualityGuardConfigRequest) validate(nodeCount int) error {
	if r.Mode != "active" && r.Mode != "passive" && r.Mode != "hybrid" {
		return errors.New("检测模式无效")
	}
	if r.ActiveIntervalSeconds < 60 || r.ActiveIntervalSeconds > 86400 {
		return errors.New("主动检测间隔必须在 60 到 86400 秒之间")
	}
	if r.PassivePollSeconds < 1 || r.PassivePollSeconds > 300 {
		return errors.New("被动审计间隔必须在 1 到 300 秒之间")
	}
	if math.IsNaN(r.SoftTPS) || math.IsInf(r.SoftTPS, 0) || math.IsNaN(r.HardTPS) || math.IsInf(r.HardTPS, 0) || r.SoftTPS < 1 || r.HardTPS > 10000 || r.SoftTPS >= r.HardTPS {
		return errors.New("Token/s 阈值无效，软阈值必须低于硬阈值")
	}
	if r.ConsecutiveSoft < 1 || r.ConsecutiveSoft > 20 || r.ConsecutiveErrors < 1 || r.ConsecutiveErrors > 20 {
		return errors.New("连续命中次数必须在 1 到 20 之间")
	}
	if r.QuarantineSeconds < 30 || r.QuarantineSeconds > 86400 {
		return errors.New("隔离时长必须在 30 到 86400 秒之间")
	}
	if r.MinHealthyNodes < 1 || r.MinHealthyNodes > nodeCount {
		return errors.New("最少保留节点必须在受管节点数量范围内")
	}
	return nil
}

func saveQualityGuardRuntimeConfig(path string, value qualityGuardRuntimeConfigFile) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".runtime-config-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func (h *Handler) readQualityGuardState() (qualityGuardState, bool, error) {
	if h.guardStatePath == "" {
		return qualityGuardState{}, false, nil
	}
	file, err := os.Open(h.guardStatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return qualityGuardState{}, false, nil
		}
		return qualityGuardState{}, true, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxQualityGuardStateBytes+1))
	if err != nil || len(data) > maxQualityGuardStateBytes {
		return qualityGuardState{}, true, errors.New("质量守护状态不可读")
	}
	var state qualityGuardState
	if json.Unmarshal(data, &state) != nil || state.Version != 1 || state.Guard.Mode == "" || state.Nodes == nil {
		return qualityGuardState{}, true, errors.New("质量守护状态格式无效")
	}
	if state.RecentEvents == nil {
		state.RecentEvents = []qualityGuardEvent{}
	}
	if state.ProtectedNodeIDs == nil {
		state.ProtectedNodeIDs = []string{}
	}
	return state, true, nil
}

func (h *Handler) testQualityGuardNode(c *gin.Context) {
	nodeID, ok := pathID(c)
	if !ok {
		return
	}
	if h.guardProbe.ClientKeyID == 0 || strings.TrimSpace(h.guardProbe.Model) == "" {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "质量守护配置暂不可用")
		return
	}
	var request struct {
		ProfileID string `json:"profileId"`
	}
	_ = c.ShouldBindJSON(&request)
	input, err := h.resolveProbeInput(strings.TrimSpace(request.ProfileID))
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", err.Error())
		return
	}
	if strings.TrimSpace(input.Prompt) == "" {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "质量守护配置暂不可用")
		return
	}
	value, err := h.service.ProbeQuality(c.Request.Context(), nodeID, input)
	if err != nil {
		h.writeQualityProbeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"requestId": value.RequestID, "nodeId": strconv.FormatUint(value.NodeID, 10), "model": value.Model,
		"statusCode": value.StatusCode, "firstTokenMs": value.FirstTokenMS, "durationMs": value.DurationMS,
		"generationMs": value.GenerationMS, "chunkCount": value.ChunkCount,
		"outputTokens": value.OutputTokens, "reasoningTokens": value.ReasoningTokens,
		"visibleTokens": value.VisibleTokens, "visibleCharacters": value.VisibleCharacters,
		"outputTokensPerSecond":  value.OutputTokensPerSecond,
		"visibleTokensPerSecond": value.OutputTokensPerSecond, "expectedMatched": value.ExpectedMatched,
		"thinkingRequired": value.ThinkingRequired,
		"responseSha256":   value.ResponseSHA256,
	})
}

func (h *Handler) cleanupPreview(c *gin.Context) {
	value, err := h.service.PreviewUnhealthyCleanup(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"nodes": value.Nodes, "boundAccounts": value.BoundAccounts, "subscriptionManaged": value.SubscriptionManaged,
	})
}

func (h *Handler) cleanup(c *gin.Context) {
	deleted, err := h.service.DeleteUnhealthy(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) refreshClearance(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.RefreshClearance(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"refreshed": true})
}

type nodeRequest struct {
	Name              string  `json:"name"`
	Scope             string  `json:"scope"`
	Enabled           bool    `json:"enabled"`
	ProxyPool         *bool   `json:"proxyPool"`
	AccountCapacity   *int    `json:"accountCapacity"`
	ProxyURL          *string `json:"proxyURL"`
	ClearProxyURL     bool    `json:"clearProxyURL"`
	UserAgent         string  `json:"userAgent"`
	CloudflareCookies *string `json:"cloudflareCookies"`
	ClearCookies      bool    `json:"clearCookies"`
}

type nodeResponse struct {
	ID                   uint64              `json:"id,string"`
	Name                 string              `json:"name"`
	Scope                string              `json:"scope"`
	Enabled              bool                `json:"enabled"`
	ProxyConfigured      bool                `json:"proxyConfigured"`
	ProxyPool            bool                `json:"proxyPool"`
	SourceID             uint64              `json:"sourceId,omitempty,string"`
	AccountCapacity      int                 `json:"accountCapacity"`
	UserAgent            string              `json:"userAgent"`
	CookieConfigured     bool                `json:"cookieConfigured"`
	AccountBoundProxy    bool                `json:"accountBoundProxy"`
	Health               float64             `json:"health"`
	FailureCount         int                 `json:"failureCount"`
	CooldownUntil        *time.Time          `json:"cooldownUntil,omitempty"`
	LastError            string              `json:"lastError,omitempty"`
	ProbeStatus          string              `json:"probeStatus"`
	LastProbedAt         *time.Time          `json:"lastProbedAt,omitempty"`
	ProbeLatencyMS       int                 `json:"probeLatencyMs"`
	ExitIP               string              `json:"exitIp,omitempty"`
	ProbeError           string              `json:"probeError,omitempty"`
	ProbeProvider        string              `json:"probeProvider,omitempty"`
	IPv4Probe            probeFamilyResponse `json:"ipv4Probe"`
	IPv6Probe            probeFamilyResponse `json:"ipv6Probe"`
	AssignedAccountCount int                 `json:"assignedAccountCount"`
}

type probeFamilyResponse struct {
	Status    string     `json:"status"`
	TestedAt  *time.Time `json:"testedAt,omitempty"`
	LatencyMS int        `json:"latencyMs"`
	ExitIP    string     `json:"exitIp,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type accountAssignmentRequest struct {
	Provider string   `json:"provider" binding:"required"`
	IDs      []string `json:"ids" binding:"required"`
	Mode     string   `json:"mode"`
}

type batchNodeDeleteRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

type batchNodeUpdateRequest struct {
	IDs     []string `json:"ids" binding:"required"`
	Enabled *bool    `json:"enabled" binding:"required"`
}

type qualityProbeRequest struct {
	ClientKeyID     string `json:"clientKeyId" binding:"required"`
	Model           string `json:"model" binding:"required"`
	Prompt          string `json:"prompt"`
	Expected        string `json:"expected"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
}

func (h *Handler) testQuality(c *gin.Context) {
	nodeID, ok := pathID(c)
	if !ok {
		return
	}
	var request qualityProbeRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	clientKeyID, err := strconv.ParseUint(request.ClientKeyID, 10, 64)
	if err != nil || clientKeyID == 0 {
		response.Error(c, http.StatusBadRequest, "invalidClientKeyId", "Client Key ID 无效")
		return
	}
	value, err := h.service.ProbeQuality(c.Request.Context(), nodeID, egressapp.QualityProbeInput{
		ClientKeyID: clientKeyID, Model: request.Model, Prompt: request.Prompt,
		Expected: request.Expected, MaxOutputTokens: request.MaxOutputTokens,
	})
	if err != nil {
		h.writeQualityProbeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"requestId": value.RequestID, "nodeId": strconv.FormatUint(value.NodeID, 10), "model": value.Model,
		"statusCode": value.StatusCode, "firstTokenMs": value.FirstTokenMS, "durationMs": value.DurationMS,
		"generationMs": value.GenerationMS, "chunkCount": value.ChunkCount,
		"outputTokens": value.OutputTokens, "reasoningTokens": value.ReasoningTokens,
		"visibleTokens": value.VisibleTokens, "visibleCharacters": value.VisibleCharacters,
		"outputTokensPerSecond":  value.OutputTokensPerSecond,
		"visibleTokensPerSecond": value.OutputTokensPerSecond, "expectedMatched": value.ExpectedMatched,
		"thinkingRequired": value.ThinkingRequired,
		"responseSha256":   value.ResponseSHA256,
	})
}

func (h *Handler) updateMany(c *gin.Context) {
	var request batchNodeUpdateRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseBoundedEgressNodeIDs(request.IDs, 5000)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "代理节点 ID 无效")
		return
	}
	updated, err := h.service.UpdateManyEnabled(c.Request.Context(), ids, *request.Enabled)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"updated": updated})
}

func (h *Handler) deleteMany(c *gin.Context) {
	var request batchNodeDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseBoundedEgressNodeIDs(request.IDs, 5000)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "代理节点 ID 无效")
		return
	}
	deleted, err := h.service.DeleteMany(c.Request.Context(), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) assignAccounts(c *gin.Context) {
	nodeID, ok := pathID(c)
	if !ok {
		return
	}
	var request accountAssignmentRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseAccountIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号 ID 无效")
		return
	}
	mode := accountdomain.EgressAssignmentMode(request.Mode)
	if mode == "" {
		mode = accountdomain.EgressAssignmentManual
	}
	result, err := h.service.AssignAccounts(c.Request.Context(), nodeID, accountdomain.Provider(request.Provider), ids, mode)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"assigned": result.Assigned})
}

func (h *Handler) unassignAccounts(c *gin.Context) {
	var request accountAssignmentRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseAccountIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号 ID 无效")
		return
	}
	result, err := h.service.UnassignAccounts(c.Request.Context(), accountdomain.Provider(request.Provider), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"assigned": result.Assigned})
}

func (value nodeRequest) input() egressapp.Input {
	return egressapp.Input{
		Name: value.Name, Scope: egressdomain.Scope(value.Scope), Enabled: value.Enabled, ProxyPool: value.ProxyPool,
		AccountCapacity: value.AccountCapacity,
		ProxyURL:        value.ProxyURL, ClearProxyURL: value.ClearProxyURL, UserAgent: value.UserAgent,
		CloudflareCookies: value.CloudflareCookies, ClearCookies: value.ClearCookies,
	}
}

func (h *Handler) list(c *gin.Context) {
	scope := egressdomain.Scope(c.Query("scope"))
	sort := repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))}
	if legacyEgressListRequest(c) {
		values, err := h.service.ListAll(c.Request.Context(), scope, sort)
		if h.writeListError(c, err) {
			return
		}
		items := make([]nodeResponse, 0, len(values))
		for _, value := range values {
			items = append(items, newNodeResponse(value))
		}
		pageSize := len(items)
		if pageSize == 0 {
			pageSize = repository.DefaultPageSize
		}
		response.Success(c, http.StatusOK, gin.H{"items": items, "page": 1, "pageSize": pageSize, "total": len(items), "defaultUserAgents": h.service.DefaultUserAgents()})
		return
	}
	page, pageSize := nodePagination(c)
	values, total, err := h.service.List(c.Request.Context(), page, pageSize, c.Query("search"), egressapp.ListFilter{
		Scope: scope, Enabled: c.Query("enabled"), ProbeStatus: c.Query("probe"), Assignment: c.Query("assignment"),
		Sort: sort,
	})
	if h.writeListError(c, err) {
		return
	}
	items := make([]nodeResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newNodeResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total, "defaultUserAgents": h.service.DefaultUserAgents()})
}

func legacyEgressListRequest(c *gin.Context) bool {
	if _, exists := c.GetQuery("page"); exists {
		return false
	}
	if _, exists := c.GetQuery("pageSize"); exists {
		return false
	}
	return c.Query("search") == "" && c.Query("enabled") == "" && c.Query("probe") == "" && c.Query("assignment") == ""
}

func (h *Handler) writeListError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, egressapp.ErrInvalidFilter):
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
	case errors.Is(err, egressapp.ErrInvalidSort):
		response.Error(c, http.StatusBadRequest, "invalidSort", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "egressNodeListFailed", "读取代理节点失败")
	}
	return true
}

func nodePagination(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	return repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
}

func (h *Handler) create(c *gin.Context) {
	var request nodeRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.Create(c.Request.Context(), request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, newNodeResponse(value))
}

func (h *Handler) update(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var request nodeRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.Update(c.Request.Context(), id, request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newNodeResponse(value))
}

func newNodeResponse(value egressdomain.PublicNode) nodeResponse {
	return nodeResponse{
		ID: value.ID, Name: value.Name, Scope: string(value.Scope), Enabled: value.Enabled,
		ProxyConfigured: value.ProxyConfigured, ProxyPool: value.ProxyPool, UserAgent: value.UserAgent, CookieConfigured: value.CookieConfigured,
		AccountBoundProxy: value.AccountBoundProxy,
		SourceID:          value.SourceID, AccountCapacity: value.AccountCapacity,
		Health: value.Health, FailureCount: value.FailureCount, CooldownUntil: value.CooldownUntil, LastError: value.LastError,
		ProbeStatus: string(value.ProbeStatus), LastProbedAt: value.LastProbedAt, ProbeLatencyMS: value.ProbeLatencyMS, ExitIP: value.ExitIP, ProbeError: value.ProbeError,
		ProbeProvider: string(value.ProbeProvider),
		IPv4Probe:     newProbeFamilyResponse(value.IPv4Probe), IPv6Probe: newProbeFamilyResponse(value.IPv6Probe),
		AssignedAccountCount: value.AssignedAccountCount,
	}
}

func newProbeFamilyResponse(value egressdomain.ProbeFamilyResult) probeFamilyResponse {
	status := value.Status
	if !status.IsValid() {
		status = egressdomain.ProbeStatusUnknown
	}
	var testedAt *time.Time
	if !value.TestedAt.IsZero() {
		canonical := value.TestedAt.UTC()
		testedAt = &canonical
	}
	return probeFamilyResponse{
		Status: string(status), TestedAt: testedAt, LatencyMS: value.LatencyMS, ExitIP: value.ExitIP, Error: value.Error,
	}
}

func parseAccountIDs(values []string) ([]uint64, error) {
	result := make([]uint64, 0, len(values))
	seen := make(map[uint64]struct{}, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("invalid id")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) == 0 {
		return nil, errors.New("no ids")
	}
	return result, nil
}

func parseEgressNodeIDs(values []string) ([]uint64, error) {
	result := make([]uint64, 0, len(values))
	seen := make(map[uint64]struct{}, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("invalid id")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) == 0 {
		return nil, errors.New("no ids")
	}
	return result, nil
}

func parseBoundedEgressNodeIDs(values []string, limit int) ([]uint64, error) {
	if len(values) == 0 || limit < 1 || len(values) > limit {
		return nil, errors.New("invalid id count")
	}
	return parseEgressNodeIDs(values)
}

func (h *Handler) delete(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

type sourceRequest struct {
	Name                   string  `json:"name"`
	Scope                  string  `json:"scope"`
	Enabled                bool    `json:"enabled"`
	URL                    *string `json:"url"`
	ClearURL               bool    `json:"clearUrl"`
	ProxyURL               *string `json:"proxyURL"`
	ClearProxyURL          bool    `json:"clearProxyURL"`
	RefreshIntervalSeconds *int    `json:"refreshIntervalSeconds"`
	DefaultAccountCapacity *int    `json:"defaultAccountCapacity"`
}

type sourceResponse struct {
	ID                     uint64     `json:"id,string"`
	Name                   string     `json:"name"`
	Scope                  string     `json:"scope"`
	Enabled                bool       `json:"enabled"`
	URLConfigured          bool       `json:"urlConfigured"`
	ProxyConfigured        bool       `json:"proxyConfigured"`
	RefreshIntervalSeconds int        `json:"refreshIntervalSeconds"`
	DefaultAccountCapacity int        `json:"defaultAccountCapacity"`
	LastSyncedAt           *time.Time `json:"lastSyncedAt,omitempty"`
	NextSyncAt             *time.Time `json:"nextSyncAt,omitempty"`
	LastSyncImported       int        `json:"lastSyncImported"`
	LastSyncError          string     `json:"lastSyncError,omitempty"`
}

type importRequest struct {
	Name            string `json:"name"`
	Scope           string `json:"scope"`
	AccountCapacity int    `json:"accountCapacity"`
	Content         string `json:"content"`
}

type probeBatchRequest struct {
	IDs []string `json:"ids"`
}

type operationsConfigRequest struct {
	ProbeProvider             string                               `json:"probeProvider"`
	ProbeIntervalSeconds      int                                  `json:"probeIntervalSeconds"`
	AutoAssignEnabled         bool                                 `json:"autoAssignEnabled"`
	AutoBalanceEnabled        bool                                 `json:"autoBalanceEnabled"`
	AssignmentIntervalSeconds int                                  `json:"assignmentIntervalSeconds"`
	Fallbacks                 map[string]operationsFallbackRequest `json:"fallbacks"`
}

type operationsFallbackRequest struct {
	Mode   string `json:"mode"`
	NodeID string `json:"nodeId"`
}

type operationsConfigResponse struct {
	ProbeProvider             string                                `json:"probeProvider"`
	ProbeIntervalSeconds      int                                   `json:"probeIntervalSeconds"`
	AutoAssignEnabled         bool                                  `json:"autoAssignEnabled"`
	AutoBalanceEnabled        bool                                  `json:"autoBalanceEnabled"`
	AssignmentIntervalSeconds int                                   `json:"assignmentIntervalSeconds"`
	Fallbacks                 map[string]operationsFallbackResponse `json:"fallbacks"`
	UpdatedAt                 time.Time                             `json:"updatedAt"`
}

type operationsFallbackResponse struct {
	Mode   string `json:"mode"`
	NodeID string `json:"nodeId,omitempty"`
}

func (value operationsConfigRequest) input() (egressapp.OperationsConfigInput, error) {
	result := egressapp.OperationsConfigInput{
		ProbeProvider: egressdomain.ProbeProvider(strings.TrimSpace(value.ProbeProvider)), ProbeIntervalSeconds: value.ProbeIntervalSeconds, AutoAssignEnabled: value.AutoAssignEnabled,
		AutoBalanceEnabled: value.AutoBalanceEnabled, AssignmentIntervalSeconds: value.AssignmentIntervalSeconds,
	}
	if value.Fallbacks == nil {
		return result, nil
	}
	result.Fallbacks = make(map[egressdomain.Scope]egressapp.FallbackConfigInput, len(value.Fallbacks))
	for rawScope, fallback := range value.Fallbacks {
		nodeID := uint64(0)
		if strings.TrimSpace(fallback.NodeID) != "" {
			parsed, err := strconv.ParseUint(fallback.NodeID, 10, 64)
			if err != nil || parsed == 0 {
				return egressapp.OperationsConfigInput{}, fmt.Errorf("%w: 固定回退节点 ID 无效", egressapp.ErrInvalidInput)
			}
			nodeID = parsed
		}
		result.Fallbacks[egressdomain.Scope(rawScope)] = egressapp.FallbackConfigInput{
			Mode: egressdomain.FallbackMode(strings.TrimSpace(fallback.Mode)), NodeID: nodeID,
		}
	}
	return result, nil
}

func (value sourceRequest) input() egressapp.SubscriptionSourceInput {
	return egressapp.SubscriptionSourceInput{
		Name: value.Name, Scope: egressdomain.Scope(value.Scope), Enabled: value.Enabled, URL: value.URL, ClearURL: value.ClearURL,
		ProxyURL: value.ProxyURL, ClearProxyURL: value.ClearProxyURL,
		RefreshIntervalSeconds: value.RefreshIntervalSeconds, DefaultAccountCapacity: value.DefaultAccountCapacity,
	}
}

func newSourceResponse(value egressdomain.PublicSubscriptionSource) sourceResponse {
	return sourceResponse{
		ID: value.ID, Name: value.Name, Scope: string(value.Scope), Enabled: value.Enabled, URLConfigured: value.URLConfigured,
		ProxyConfigured:        value.ProxyConfigured,
		RefreshIntervalSeconds: value.RefreshIntervalSeconds, DefaultAccountCapacity: value.DefaultAccountCapacity,
		LastSyncedAt: value.LastSyncedAt, NextSyncAt: value.NextSyncAt, LastSyncImported: value.LastSyncImported, LastSyncError: value.LastSyncError,
	}
}

func newOperationsConfigResponse(value egressdomain.OperationsConfig) operationsConfigResponse {
	fallbacks := make(map[string]operationsFallbackResponse, 5)
	for _, scope := range []egressdomain.Scope{egressdomain.ScopeBuild, egressdomain.ScopeWeb, egressdomain.ScopeConsole, egressdomain.ScopeWebAsset, egressdomain.ScopeConsoleAsset} {
		fallback := value.FallbackFor(scope)
		item := operationsFallbackResponse{Mode: string(fallback.Mode)}
		if fallback.NodeID != 0 {
			item.NodeID = strconv.FormatUint(fallback.NodeID, 10)
		}
		fallbacks[string(scope)] = item
	}
	return operationsConfigResponse{
		ProbeProvider: string(value.ProbeProvider.Normalized()), ProbeIntervalSeconds: value.ProbeIntervalSeconds, AutoAssignEnabled: value.AutoAssignEnabled,
		AutoBalanceEnabled: value.AutoBalanceEnabled, AssignmentIntervalSeconds: value.AssignmentIntervalSeconds,
		Fallbacks: fallbacks, UpdatedAt: value.UpdatedAt,
	}
}

func (h *Handler) listSources(c *gin.Context) {
	if !legacyEgressSourceListRequest(c) {
		page, pageSize := nodePagination(c)
		values, total, err := h.service.ListSourcePage(c.Request.Context(), page, pageSize, c.Query("search"), egressapp.SourceListFilter{
			Scope: egressdomain.Scope(c.Query("scope")),
		})
		if h.writeSourceListError(c, err) {
			return
		}
		items := make([]sourceResponse, 0, len(values))
		for _, value := range values {
			items = append(items, newSourceResponse(value))
		}
		response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
		return
	}
	values, err := h.service.ListSources(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]sourceResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newSourceResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}

func legacyEgressSourceListRequest(c *gin.Context) bool {
	if _, exists := c.GetQuery("page"); exists {
		return false
	}
	if _, exists := c.GetQuery("pageSize"); exists {
		return false
	}
	return c.Query("search") == "" && c.Query("scope") == ""
}

func (h *Handler) writeSourceListError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, egressapp.ErrInvalidFilter):
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "egressSourceListFailed", "读取代理订阅来源失败")
	}
	return true
}

func (h *Handler) createSource(c *gin.Context) {
	var request sourceRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.CreateSource(c.Request.Context(), request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, newSourceResponse(value))
}

func (h *Handler) updateSource(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var request sourceRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.UpdateSource(c.Request.Context(), id, request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newSourceResponse(value))
}

func (h *Handler) deleteSource(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.DeleteSource(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) syncSource(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.SyncSource(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"imported": value.Imported, "skipped": value.Skipped})
}

func (h *Handler) importText(c *gin.Context) {
	var request importRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.ImportText(c.Request.Context(), egressapp.ImportInput{
		Name: request.Name, Scope: egressdomain.Scope(request.Scope), AccountCapacity: request.AccountCapacity, Content: request.Content,
	})
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, gin.H{"imported": value.Imported, "skipped": value.Skipped})
}

func (h *Handler) testNode(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.TestNode(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"status": value.Status, "testedAt": value.TestedAt, "latencyMs": value.LatencyMS, "exitIp": value.ExitIP, "error": value.Error,
		"probeProvider": value.Provider,
		"ipv4":          newProbeFamilyResponse(value.IPv4), "ipv6": newProbeFamilyResponse(value.IPv6),
	})
}

func (h *Handler) testNodes(c *gin.Context) {
	var request probeBatchRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseOptionalAccountIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号 ID 无效")
		return
	}
	value, err := h.service.TestNodes(c.Request.Context(), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"requested": value.Requested, "healthy": value.Healthy, "unhealthy": value.Unhealthy})
}

func (h *Handler) operationsConfig(c *gin.Context) {
	value, err := h.service.OperationsConfig(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newOperationsConfigResponse(value))
}

func (h *Handler) updateOperationsConfig(c *gin.Context) {
	var request operationsConfigRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	input, err := request.input()
	if err != nil {
		h.writeError(c, err)
		return
	}
	value, err := h.service.UpdateOperationsConfig(c.Request.Context(), input)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newOperationsConfigResponse(value))
}

func (h *Handler) rebalance(c *gin.Context) {
	config, err := h.service.OperationsConfig(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	value, err := h.service.RebalanceAccounts(c.Request.Context(), true, true, time.Duration(config.ProbeIntervalSeconds)*time.Second)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"assigned": value.Assigned, "rebalanced": value.Rebalanced, "unplaced": value.Unplaced})
}

func parseOptionalAccountIDs(values []string) ([]uint64, error) {
	if len(values) == 0 {
		return nil, nil
	}
	return parseAccountIDs(values)
}

func (h *Handler) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, egressapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, "invalidEgressNode", err.Error())
	case errors.Is(err, egressapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "egressNodeNotFound", err.Error())
	case errors.Is(err, egressapp.ErrProbeStale):
		response.Error(c, http.StatusConflict, "egressProbeStale", err.Error())
	case errors.Is(err, repository.ErrConflict):
		response.Error(c, http.StatusConflict, "egressConflict", "名称已存在")
	case errors.Is(err, egressapp.ErrOperationsUnavailable):
		response.Error(c, http.StatusServiceUnavailable, "egressOperationsUnavailable", "代理运营功能暂不可用")
	case errors.Is(err, egressapp.ErrSubscriptionSync):
		response.Error(c, http.StatusBadGateway, "egressSubscriptionSyncFailed", "代理订阅同步失败")
	case errors.Is(err, egressapp.ErrClearanceUnavailable):
		response.Error(c, http.StatusConflict, "clearanceRefreshUnavailable", err.Error())
	case errors.Is(err, egressapp.ErrQualityProbeUnavailable):
		response.Error(c, http.StatusServiceUnavailable, "egressQualityProbeUnavailable", err.Error())
	case strings.Contains(err.Error(), "FlareSolverr") || strings.Contains(err.Error(), "Clearance"):
		response.Error(c, http.StatusBadGateway, "clearanceRefreshFailed", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "egressNodeOperationFailed", "代理节点操作失败")
	}
}

func (h *Handler) writeQualityProbeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, egressapp.ErrQualityProbeNoAccount):
		response.Error(c, http.StatusServiceUnavailable, "egressQualityProbeNoAccount", "质量检测暂无可调度账号，请稍后重试")
	case errors.Is(err, egressapp.ErrInvalidInput),
		errors.Is(err, egressapp.ErrNotFound),
		errors.Is(err, egressapp.ErrQualityProbeUnavailable):
		h.writeError(c, err)
	default:
		response.Error(c, http.StatusBadGateway, "egressQualityProbeFailed", "质量检测暂不可用，请稍后重试")
	}
}

func pathID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return 0, false
	}
	return id, true
}
