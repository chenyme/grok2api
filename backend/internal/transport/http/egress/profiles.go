package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

const (
	profileThroughput    = "throughput"
	profileQualityMarker = "quality-marker"
	maxCustomProfiles    = 32
)

type probeProfile struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	BuiltIn         bool    `json:"built_in"`
	Prompt          string  `json:"prompt"`
	ExpectedText    string  `json:"expected_text,omitempty"`
	MatchMode       string  `json:"match_mode"`
	RequireThinking bool    `json:"require_thinking"`
	MaxOutputTokens int     `json:"max_output_tokens,omitempty"`
	UpdatedAt       float64 `json:"updated_at,omitempty"`
}

type probeProfileFile struct {
	Version         int                     `json:"version"`
	ActiveProfileID string                  `json:"active_profile_id"`
	NextID          int                     `json:"next_id"`
	Profiles        map[string]probeProfile `json:"profiles"`
}

func builtinProbeProfiles() []probeProfile {
	return []probeProfile{
		{
			ID:              profileQualityMarker,
			Name:            "预期标记",
			BuiltIn:         true,
			Prompt:          "先用三点总结为什么天空呈蓝色，最后一行只输出 QUALITY_OK。",
			ExpectedText:    "QUALITY_OK",
			MatchMode:       egressapp.MatchLastLine,
			RequireThinking: true,
		},
		{
			ID:        profileThroughput,
			Name:      "吞吐基线",
			BuiltIn:   true,
			Prompt:    "Write a detailed technical explanation of how TCP slow start works, at least 12 sentences, plain text only.",
			MatchMode: egressapp.MatchContains,
		},
	}
}

func (h *Handler) profilesPath() string {
	if h.guardConfigPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(h.guardConfigPath), "profiles.json")
}

func seedProbeProfileFile(data *probeProfileFile) {
	if data.Profiles == nil {
		data.Profiles = map[string]probeProfile{}
	}
	if data.Version == 0 {
		data.Version = 1
	}
	now := float64(time.Now().Unix())
	for _, builtin := range builtinProbeProfiles() {
		existing, ok := data.Profiles[builtin.ID]
		if !ok {
			builtin.UpdatedAt = now
			data.Profiles[builtin.ID] = builtin
			continue
		}
		// Reserved built-in IDs define recovery safety invariants. Never trust
		// persisted metadata for these IDs, even if an older version or a manual
		// edit incorrectly cleared built_in.
		existing.ID = builtin.ID
		existing.Name = builtin.Name
		existing.BuiltIn = true
		existing.Prompt = builtin.Prompt
		existing.ExpectedText = builtin.ExpectedText
		existing.MatchMode = builtin.MatchMode
		existing.RequireThinking = builtin.RequireThinking
		existing.MaxOutputTokens = builtin.MaxOutputTokens
		data.Profiles[builtin.ID] = existing
	}
	if data.ActiveProfileID == "" {
		data.ActiveProfileID = profileQualityMarker
	}
	if _, ok := data.Profiles[data.ActiveProfileID]; !ok {
		data.ActiveProfileID = profileQualityMarker
	}
	if data.NextID < 1 {
		data.NextID = 1
	}
}

func loadProbeProfileFile(path string) (probeProfileFile, error) {
	data := probeProfileFile{Version: 1, Profiles: map[string]probeProfile{}}
	if path == "" {
		seedProbeProfileFile(&data)
		return data, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			seedProbeProfileFile(&data)
			return data, nil
		}
		return probeProfileFile{}, err
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return probeProfileFile{}, err
	}
	seedProbeProfileFile(&data)
	return data, nil
}

func saveProbeProfileFile(path string, data probeProfileFile) error {
	if path == "" {
		return errors.New("profiles path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".profiles-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func (data probeProfileFile) resolve(id string) (probeProfile, bool) {
	if id == "" {
		id = data.ActiveProfileID
	}
	profile, ok := data.Profiles[id]
	return profile, ok
}

func (data probeProfileFile) summaries() []map[string]any {
	out := make([]map[string]any, 0, len(data.Profiles))
	for _, profile := range data.Profiles {
		out = append(out, map[string]any{
			"id":               profile.ID,
			"name":             profile.Name,
			"built_in":         profile.BuiltIn,
			"match_mode":       profile.MatchMode,
			"has_expected":     strings.TrimSpace(profile.ExpectedText) != "",
			"require_thinking": profile.RequireThinking,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		leftBuiltIn, _ := out[i]["built_in"].(bool)
		rightBuiltIn, _ := out[j]["built_in"].(bool)
		if leftBuiltIn != rightBuiltIn {
			return leftBuiltIn
		}
		leftID, _ := out[i]["id"].(string)
		rightID, _ := out[j]["id"].(string)
		return leftID < rightID
	})
	return out
}

func validateProbeProfile(profile *probeProfile, creating bool) error {
	profile.Name = strings.TrimSpace(profile.Name)
	profile.Prompt = strings.TrimSpace(profile.Prompt)
	profile.ExpectedText = strings.TrimSpace(profile.ExpectedText)
	profile.MatchMode = egressapp.NormalizeMatchMode(profile.MatchMode)
	if profile.Name == "" {
		return errors.New("方案名称不能为空")
	}
	if len(profile.Name) > 80 {
		return errors.New("方案名称不能超过 80 个字符")
	}
	if profile.Prompt == "" {
		return errors.New("探测 Prompt 不能为空")
	}
	if len(profile.Prompt) > egressapp.MaxQualityProbePromptBytes {
		return errors.New("探测 Prompt 过长")
	}
	if len(profile.ExpectedText) > egressapp.MaxQualityProbeExpectedBytes {
		return errors.New("预期标记过长")
	}
	if profile.MatchMode == egressapp.MatchRegex && profile.ExpectedText != "" {
		if _, err := regexp.Compile(profile.ExpectedText); err != nil {
			return fmt.Errorf("正则预期标记无效")
		}
	}
	if profile.MaxOutputTokens < 0 || profile.MaxOutputTokens > egressapp.MaxQualityProbeOutputTokens {
		return fmt.Errorf("方案最大输出需在 0 到 %d Token 之间", egressapp.MaxQualityProbeOutputTokens)
	}
	if creating && profile.BuiltIn {
		return errors.New("不能新建内置方案")
	}
	return nil
}

func applyProfile(base egressapp.QualityProbeInput, profile probeProfile) egressapp.QualityProbeInput {
	base.Prompt = profile.Prompt
	base.Expected = profile.ExpectedText
	base.MatchMode = egressapp.NormalizeMatchMode(profile.MatchMode)
	base.RequireThinking = profile.RequireThinking
	if profile.MaxOutputTokens > 0 {
		base.MaxOutputTokens = profile.MaxOutputTokens
	}
	return base
}

func (h *Handler) listQualityGuardProfiles(c *gin.Context) {
	data, err := loadProbeProfileFile(h.profilesPath())
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "探针方案暂不可用")
		return
	}
	items := make([]probeProfile, 0, len(data.Profiles))
	for _, profile := range data.Profiles {
		items = append(items, profile)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].BuiltIn != items[j].BuiltIn {
			return items[i].BuiltIn
		}
		return items[i].ID < items[j].ID
	})
	response.Success(c, http.StatusOK, gin.H{
		"activeProfileId": data.ActiveProfileID,
		"items":           items,
	})
}

type probeProfileWriteRequest struct {
	Name            string `json:"name"`
	Prompt          string `json:"prompt"`
	ExpectedText    string `json:"expectedText"`
	MatchMode       string `json:"matchMode"`
	RequireThinking bool   `json:"requireThinking"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
	Active          bool   `json:"active"`
}

func (h *Handler) createQualityGuardProfile(c *gin.Context) {
	path := h.profilesPath()
	if path == "" {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardReadOnly", "探针方案当前只读")
		return
	}
	var request probeProfileWriteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	h.profilesMu.Lock()
	defer h.profilesMu.Unlock()
	data, err := loadProbeProfileFile(path)
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "探针方案暂不可用")
		return
	}
	custom := 0
	for _, profile := range data.Profiles {
		if !profile.BuiltIn {
			custom++
		}
	}
	if custom >= maxCustomProfiles {
		response.Error(c, http.StatusBadRequest, "invalidRequest", fmt.Sprintf("自定义方案最多 %d 个", maxCustomProfiles))
		return
	}
	profile := probeProfile{
		Name: request.Name, Prompt: request.Prompt, ExpectedText: request.ExpectedText,
		MatchMode: request.MatchMode, RequireThinking: request.RequireThinking, MaxOutputTokens: request.MaxOutputTokens,
	}
	if err := validateProbeProfile(&profile, true); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", err.Error())
		return
	}
	data.NextID++
	profile.ID = fmt.Sprintf("p-%d", data.NextID)
	profile.UpdatedAt = float64(time.Now().Unix())
	data.Profiles[profile.ID] = profile
	if request.Active {
		data.ActiveProfileID = profile.ID
	}
	if err := saveProbeProfileFile(path, data); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardConfigWriteFailed", "探针方案保存失败")
		return
	}
	response.Success(c, http.StatusOK, profile)
}

func (h *Handler) updateQualityGuardProfile(c *gin.Context) {
	path := h.profilesPath()
	if path == "" {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardReadOnly", "探针方案当前只读")
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	var request probeProfileWriteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	h.profilesMu.Lock()
	defer h.profilesMu.Unlock()
	data, err := loadProbeProfileFile(path)
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "探针方案暂不可用")
		return
	}
	existing, ok := data.Profiles[id]
	if !ok {
		response.Error(c, http.StatusNotFound, "notFound", "方案不存在")
		return
	}
	if existing.BuiltIn {
		if request.Active {
			data.ActiveProfileID = existing.ID
			if err := saveProbeProfileFile(path, data); err != nil {
				response.Error(c, http.StatusServiceUnavailable, "qualityGuardConfigWriteFailed", "探针方案保存失败")
				return
			}
			response.Success(c, http.StatusOK, existing)
			return
		}
		response.Error(c, http.StatusBadRequest, "invalidRequest", "内置方案不能修改，请复制后另存")
		return
	}
	existing.Name = request.Name
	existing.Prompt = request.Prompt
	existing.ExpectedText = request.ExpectedText
	existing.MatchMode = request.MatchMode
	existing.RequireThinking = request.RequireThinking
	existing.MaxOutputTokens = request.MaxOutputTokens
	if err := validateProbeProfile(&existing, false); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", err.Error())
		return
	}
	existing.UpdatedAt = float64(time.Now().Unix())
	data.Profiles[id] = existing
	if request.Active {
		data.ActiveProfileID = id
	}
	if err := saveProbeProfileFile(path, data); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardConfigWriteFailed", "探针方案保存失败")
		return
	}
	response.Success(c, http.StatusOK, existing)
}

func (h *Handler) deleteQualityGuardProfile(c *gin.Context) {
	path := h.profilesPath()
	if path == "" {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardReadOnly", "探针方案当前只读")
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	h.profilesMu.Lock()
	defer h.profilesMu.Unlock()
	data, err := loadProbeProfileFile(path)
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "探针方案暂不可用")
		return
	}
	existing, ok := data.Profiles[id]
	if !ok {
		response.Error(c, http.StatusNotFound, "notFound", "方案不存在")
		return
	}
	if existing.BuiltIn {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "内置方案不能删除")
		return
	}
	delete(data.Profiles, id)
	if data.ActiveProfileID == id {
		data.ActiveProfileID = profileQualityMarker
	}
	if err := saveProbeProfileFile(path, data); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardConfigWriteFailed", "探针方案保存失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) resolveProbeInput(profileID string) (egressapp.QualityProbeInput, error) {
	input := h.guardProbe
	data, err := loadProbeProfileFile(h.profilesPath())
	if err != nil {
		return input, err
	}
	profile, ok := data.resolve(profileID)
	if !ok {
		if profileID != "" {
			return input, fmt.Errorf("方案不存在")
		}
		return input, nil
	}
	return applyProfile(input, profile), nil
}
