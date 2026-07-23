package settings

import (
	"errors"
	"net/http"
	"strings"
	"time"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *settingsapp.Service }

func NewHandler(service *settingsapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/settings", h.get)
	router.PUT("/settings", h.update)
}

type settingsConfigDTO struct {
	Server            serverConfigDTO            `json:"server"`
	ProviderBuild     providerBuildConfigDTO     `json:"providerBuild"`
	ProviderWeb       providerWebConfigDTO       `json:"providerWeb"`
	ProviderConsole   providerConsoleConfigDTO   `json:"providerConsole"`
	Batch             batchConfigDTO             `json:"batch"`
	Media             mediaConfigDTO             `json:"media"`
	Frontend          frontendConfigDTO          `json:"frontend"`
	Routing           routingConfigDTO           `json:"routing"`
	Audit             auditConfigDTO             `json:"audit"`
	ClientKeyDefaults clientKeyDefaultsConfigDTO `json:"clientKeyDefaults"`
	Accounts          *accountsConfigDTO         `json:"accounts,omitempty"`
}

type serverConfigDTO struct {
	MaxConcurrentRequests int `json:"maxConcurrentRequests"`
}

type providerConsoleConfigDTO struct {
	BaseURL     string `json:"baseURL"`
	ChatTimeout string `json:"chatTimeout"`
}

type mediaConfigDTO struct {
	MaxImageBytes           int64  `json:"maxImageBytes"`
	MaxTotalBytes           int64  `json:"maxTotalBytes"`
	CleanupThresholdPercent int    `json:"cleanupThresholdPercent"`
	CleanupInterval         string `json:"cleanupInterval"`
}

type frontendConfigDTO struct {
	PublicAPIBaseURL string `json:"publicApiBaseURL"`
}

type providerBuildConfigDTO struct {
	BaseURL               string `json:"baseURL"`
	FallbackBaseURL       string `json:"fallbackBaseURL"`
	ClientVersion         string `json:"clientVersion"`
	ClientIdentifier      string `json:"clientIdentifier"`
	TokenAuth             string `json:"tokenAuth"`
	TokenAuthConfigured   bool   `json:"tokenAuthConfigured"`
	UserAgent             string `json:"userAgent"`
	ResponseHeaderTimeout string `json:"responseHeaderTimeout"`
}

type providerWebConfigDTO struct {
	BaseURL                 string  `json:"baseURL"`
	StatsigMode             string  `json:"statsigMode"`
	StatsigManualValue      string  `json:"statsigManualValue,omitempty"`
	StatsigManualConfigured bool    `json:"statsigManualConfigured"`
	StatsigSignerURL        string  `json:"statsigSignerURL"`
	ClearanceMode           *string `json:"clearanceMode,omitempty"`
	FlareSolverrURL         *string `json:"flareSolverrURL,omitempty"`
	ClearanceTimeout        *string `json:"clearanceTimeout,omitempty"`
	ClearanceRefresh        *string `json:"clearanceRefresh,omitempty"`
	QuotaTimeout            string  `json:"quotaTimeout"`
	ChatTimeout             string  `json:"chatTimeout"`
	ImageTimeout            string  `json:"imageTimeout"`
	VideoTimeout            string  `json:"videoTimeout"`
	MediaConcurrency        int     `json:"mediaConcurrency"`
	AllowNSFW               bool    `json:"allowNSFW"`
	RecoveryBackoffBase     string  `json:"recoveryBackoffBase"`
	RecoveryBackoffMax      string  `json:"recoveryBackoffMax"`
}

type batchConfigDTO struct {
	ImportConcurrency     int    `json:"importConcurrency"`
	ConversionConcurrency int    `json:"conversionConcurrency"`
	SyncConcurrency       int    `json:"syncConcurrency"`
	RefreshConcurrency    int    `json:"refreshConcurrency"`
	RandomDelay           string `json:"randomDelay"`
}

type routingConfigDTO struct {
	StickyTTL         string                      `json:"stickyTTL"`
	CooldownBase      string                      `json:"cooldownBase"`
	CooldownMax       string                      `json:"cooldownMax"`
	CapacityWait      string                      `json:"capacityWait"`
	MaxAttempts       int                         `json:"maxAttempts"`
	PreferFreeBuild   bool                        `json:"preferFreeBuild"`
	SegmentedSelector *segmentedSelectorConfigDTO `json:"segmentedSelector,omitempty"`
}

type segmentedSelectorConfigDTO struct {
	Enabled       bool `json:"enabled"`
	MinCandidates int  `json:"minCandidates"`
	WindowSize    int  `json:"windowSize"`
}

type auditConfigDTO struct {
	BufferSize    int    `json:"bufferSize"`
	BatchSize     int    `json:"batchSize"`
	FlushInterval string `json:"flushInterval"`
	CommitDelayMS int    `json:"commitDelayMS"`
}

type clientKeyDefaultsConfigDTO struct {
	RPMLimit      int `json:"rpmLimit"`
	MaxConcurrent int `json:"maxConcurrent"`
}

type accountsConfigDTO struct {
	MarkBuildForbiddenReauth  *bool     `json:"markBuildForbiddenReauth,omitempty"`
	BuildForbiddenReauthCodes *[]string `json:"buildForbiddenReauthCodes,omitempty"`
	AutoCleanReauthEnabled    bool      `json:"autoCleanReauthEnabled"`
	AutoCleanReauthInterval   string    `json:"autoCleanReauthInterval"`
	AutoCleanReauthMinAge     string    `json:"autoCleanReauthMinAge"`
	AutoCleanIncludeDisabled  bool      `json:"autoCleanIncludeDisabled"`
}

type settingsResponse struct {
	Config                   settingsConfigDTO              `json:"config"`
	RecommendedProviderBuild providerBuildRecommendationDTO `json:"recommendedProviderBuild"`
	UpdatedAt                time.Time                      `json:"updatedAt"`
	Revision                 uint64                         `json:"revision,string"`
	RestartRequired          []string                       `json:"restartRequired"`
}

type providerBuildRecommendationDTO struct {
	ClientVersion string `json:"clientVersion"`
	UserAgent     string `json:"userAgent"`
}

type updateRequest struct {
	Revision uint64            `json:"revision,string"`
	Config   settingsConfigDTO `json:"config" binding:"required"`
}

func (h *Handler) get(c *gin.Context) {
	response.Success(c, http.StatusOK, newSettingsResponse(h.service.Get()))
}

func (h *Handler) update(c *gin.Context) {
	var request updateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+err.Error())
		return
	}
	result, err := h.service.Update(c.Request.Context(), request.Revision, request.Config.toApplication())
	if err != nil {
		if errors.Is(err, settingsapp.ErrInvalidInput) {
			response.Error(c, http.StatusBadRequest, "settingsUpdateFailed", err.Error())
			return
		}
		if errors.Is(err, settingsapp.ErrConflict) {
			response.Error(c, http.StatusConflict, "settingsConflict", "设置已被其他会话更新，请刷新后重试")
			return
		}
		response.Error(c, http.StatusInternalServerError, "settingsUpdateFailed", "保存运行设置失败")
		return
	}
	response.Success(c, http.StatusOK, newSettingsResponse(result))
}

func (value settingsConfigDTO) toApplication() settingsapp.EditableConfig {
	clearanceProvided := value.ProviderWeb.ClearanceMode != nil || value.ProviderWeb.FlareSolverrURL != nil ||
		value.ProviderWeb.ClearanceTimeout != nil || value.ProviderWeb.ClearanceRefresh != nil
	result := settingsapp.EditableConfig{
		Server: settingsapp.ServerConfig{MaxConcurrentRequests: value.Server.MaxConcurrentRequests},
		ProviderBuild: settingsapp.ProviderBuildConfig{
			BaseURL: value.ProviderBuild.BaseURL, FallbackBaseURL: value.ProviderBuild.FallbackBaseURL,
			ClientVersion: value.ProviderBuild.ClientVersion, ClientIdentifier: value.ProviderBuild.ClientIdentifier,
			TokenAuth: value.ProviderBuild.TokenAuth, UserAgent: value.ProviderBuild.UserAgent,
			ResponseHeaderTimeout: value.ProviderBuild.ResponseHeaderTimeout,
		},
		ProviderWeb: settingsapp.ProviderWebConfig{
			BaseURL: value.ProviderWeb.BaseURL, QuotaTimeout: value.ProviderWeb.QuotaTimeout,
			StatsigMode: value.ProviderWeb.StatsigMode, StatsigManualValue: value.ProviderWeb.StatsigManualValue,
			StatsigManualConfigured: value.ProviderWeb.StatsigManualConfigured, StatsigSignerURL: value.ProviderWeb.StatsigSignerURL,
			ClearanceMode: optionalString(value.ProviderWeb.ClearanceMode), FlareSolverrURL: optionalString(value.ProviderWeb.FlareSolverrURL),
			ClearanceTimeout: optionalString(value.ProviderWeb.ClearanceTimeout), ClearanceRefresh: optionalString(value.ProviderWeb.ClearanceRefresh),
			ClearanceProvided: clearanceProvided,
			ChatTimeout:       value.ProviderWeb.ChatTimeout, ImageTimeout: value.ProviderWeb.ImageTimeout,
			VideoTimeout:     value.ProviderWeb.VideoTimeout,
			MediaConcurrency: value.ProviderWeb.MediaConcurrency, AllowNSFW: value.ProviderWeb.AllowNSFW,
			RecoveryBackoffBase: value.ProviderWeb.RecoveryBackoffBase, RecoveryBackoffMax: value.ProviderWeb.RecoveryBackoffMax,
		},
		ProviderConsole: settingsapp.ProviderConsoleConfig{
			BaseURL: value.ProviderConsole.BaseURL, ChatTimeout: value.ProviderConsole.ChatTimeout,
		},
		Batch: settingsapp.BatchConfig{
			ImportConcurrency: value.Batch.ImportConcurrency, ConversionConcurrency: value.Batch.ConversionConcurrency,
			SyncConcurrency: value.Batch.SyncConcurrency, RefreshConcurrency: value.Batch.RefreshConcurrency,
			RandomDelay: value.Batch.RandomDelay,
		},
		Media: settingsapp.MediaConfig{
			MaxImageBytes: value.Media.MaxImageBytes, MaxTotalBytes: value.Media.MaxTotalBytes,
			CleanupThresholdPercent: value.Media.CleanupThresholdPercent, CleanupInterval: value.Media.CleanupInterval,
		},
		Frontend: settingsapp.FrontendConfig{
			PublicAPIBaseURL: value.Frontend.PublicAPIBaseURL,
		},
		Routing: settingsapp.RoutingConfig{
			StickyTTL: value.Routing.StickyTTL, CooldownBase: value.Routing.CooldownBase,
			CooldownMax: value.Routing.CooldownMax, CapacityWait: value.Routing.CapacityWait, MaxAttempts: value.Routing.MaxAttempts,
			PreferFreeBuild: value.Routing.PreferFreeBuild,
		},
		Audit: settingsapp.AuditConfig{
			BufferSize: value.Audit.BufferSize, BatchSize: value.Audit.BatchSize, FlushInterval: value.Audit.FlushInterval, CommitDelayMS: value.Audit.CommitDelayMS,
		},
		ClientKeyDefaults: settingsapp.ClientKeyDefaultsConfig{
			RPMLimit: value.ClientKeyDefaults.RPMLimit, MaxConcurrent: value.ClientKeyDefaults.MaxConcurrent,
		},
	}
	if value.Routing.SegmentedSelector != nil {
		result.Routing.SegmentedSelector = settingsapp.SegmentedSelectorConfig{
			Enabled: value.Routing.SegmentedSelector.Enabled, MinCandidates: value.Routing.SegmentedSelector.MinCandidates,
			WindowSize: value.Routing.SegmentedSelector.WindowSize,
		}
		result.Routing.SegmentedSelectorProvided = true
	}
	if value.Accounts != nil {
		result.Accounts = settingsapp.AccountsConfig{
			MarkBuildForbiddenReauth:          boolValue(value.Accounts.MarkBuildForbiddenReauth),
			BuildForbiddenReauthCodes:         stringSliceValue(value.Accounts.BuildForbiddenReauthCodes),
			MarkBuildForbiddenReauthProvided:  value.Accounts.MarkBuildForbiddenReauth != nil,
			BuildForbiddenReauthCodesProvided: value.Accounts.BuildForbiddenReauthCodes != nil,
			AutoCleanReauthEnabled:            value.Accounts.AutoCleanReauthEnabled,
			AutoCleanReauthInterval:           value.Accounts.AutoCleanReauthInterval,
			AutoCleanReauthMinAge:             value.Accounts.AutoCleanReauthMinAge,
			AutoCleanIncludeDisabled:          value.Accounts.AutoCleanIncludeDisabled,
		}
		result.AccountsProvided = true
	}
	return result
}

func newSettingsResponse(value settingsapp.Snapshot) settingsResponse {
	config := value.Config
	return settingsResponse{
		Config: settingsConfigDTO{
			Server: serverConfigDTO{MaxConcurrentRequests: config.Server.MaxConcurrentRequests},
			ProviderBuild: providerBuildConfigDTO{
				BaseURL: config.ProviderBuild.BaseURL, FallbackBaseURL: config.ProviderBuild.FallbackBaseURL,
				ClientVersion: config.ProviderBuild.ClientVersion, ClientIdentifier: config.ProviderBuild.ClientIdentifier,
				TokenAuth:           config.ProviderBuild.TokenAuth,
				TokenAuthConfigured: strings.TrimSpace(config.ProviderBuild.TokenAuth) != "", UserAgent: config.ProviderBuild.UserAgent,
				ResponseHeaderTimeout: config.ProviderBuild.ResponseHeaderTimeout,
			},
			ProviderWeb: providerWebConfigDTO{
				BaseURL: config.ProviderWeb.BaseURL, QuotaTimeout: config.ProviderWeb.QuotaTimeout,
				StatsigMode: config.ProviderWeb.StatsigMode, StatsigManualConfigured: config.ProviderWeb.StatsigManualConfigured,
				StatsigSignerURL: config.ProviderWeb.StatsigSignerURL,
				ClearanceMode:    stringPointer(config.ProviderWeb.ClearanceMode), FlareSolverrURL: stringPointer(config.ProviderWeb.FlareSolverrURL),
				ClearanceTimeout: stringPointer(config.ProviderWeb.ClearanceTimeout), ClearanceRefresh: stringPointer(config.ProviderWeb.ClearanceRefresh),
				ChatTimeout: config.ProviderWeb.ChatTimeout, ImageTimeout: config.ProviderWeb.ImageTimeout,
				VideoTimeout:     config.ProviderWeb.VideoTimeout,
				MediaConcurrency: config.ProviderWeb.MediaConcurrency, AllowNSFW: config.ProviderWeb.AllowNSFW,
				RecoveryBackoffBase: config.ProviderWeb.RecoveryBackoffBase, RecoveryBackoffMax: config.ProviderWeb.RecoveryBackoffMax,
			},
			ProviderConsole: providerConsoleConfigDTO{
				BaseURL: config.ProviderConsole.BaseURL, ChatTimeout: config.ProviderConsole.ChatTimeout,
			},
			Batch: batchConfigDTO{
				ImportConcurrency: config.Batch.ImportConcurrency, ConversionConcurrency: config.Batch.ConversionConcurrency,
				SyncConcurrency: config.Batch.SyncConcurrency, RefreshConcurrency: config.Batch.RefreshConcurrency,
				RandomDelay: config.Batch.RandomDelay,
			},
			Media: mediaConfigDTO{
				MaxImageBytes: config.Media.MaxImageBytes, MaxTotalBytes: config.Media.MaxTotalBytes,
				CleanupThresholdPercent: config.Media.CleanupThresholdPercent, CleanupInterval: config.Media.CleanupInterval,
			},
			Frontend: frontendConfigDTO{
				PublicAPIBaseURL: config.Frontend.PublicAPIBaseURL,
			},
			Routing: routingConfigDTO{
				StickyTTL: config.Routing.StickyTTL, CooldownBase: config.Routing.CooldownBase,
				CooldownMax: config.Routing.CooldownMax, CapacityWait: config.Routing.CapacityWait, MaxAttempts: config.Routing.MaxAttempts,
				PreferFreeBuild: config.Routing.PreferFreeBuild,
				SegmentedSelector: &segmentedSelectorConfigDTO{
					Enabled: config.Routing.SegmentedSelector.Enabled, MinCandidates: config.Routing.SegmentedSelector.MinCandidates,
					WindowSize: config.Routing.SegmentedSelector.WindowSize,
				},
			},
			Audit: auditConfigDTO{
				BufferSize: config.Audit.BufferSize, BatchSize: config.Audit.BatchSize, FlushInterval: config.Audit.FlushInterval, CommitDelayMS: config.Audit.CommitDelayMS,
			},
			ClientKeyDefaults: clientKeyDefaultsConfigDTO{
				RPMLimit: config.ClientKeyDefaults.RPMLimit, MaxConcurrent: config.ClientKeyDefaults.MaxConcurrent,
			},
			Accounts: &accountsConfigDTO{
				MarkBuildForbiddenReauth:  boolPointer(config.Accounts.MarkBuildForbiddenReauth),
				BuildForbiddenReauthCodes: stringSlicePointer(config.Accounts.BuildForbiddenReauthCodes),
				AutoCleanReauthEnabled:    config.Accounts.AutoCleanReauthEnabled,
				AutoCleanReauthInterval:   config.Accounts.AutoCleanReauthInterval,
				AutoCleanReauthMinAge:     config.Accounts.AutoCleanReauthMinAge,
				AutoCleanIncludeDisabled:  config.Accounts.AutoCleanIncludeDisabled,
			},
		},
		RecommendedProviderBuild: providerBuildRecommendationDTO{
			ClientVersion: value.RecommendedProviderBuild.ClientVersion,
			UserAgent:     value.RecommendedProviderBuild.UserAgent,
		},
		UpdatedAt: value.UpdatedAt, Revision: value.Revision, RestartRequired: value.RestartRequired,
	}
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func stringPointer(value string) *string { return &value }

func boolPointer(value bool) *bool { return &value }

func boolValue(value *bool) bool {
	if value == nil {
		return false
	}
	return *value
}

func stringSliceValue(value *[]string) []string {
	if value == nil {
		return nil
	}
	return append([]string(nil), (*value)...)
}

func stringSlicePointer(value []string) *[]string {
	cloned := append([]string(nil), value...)
	return &cloned
}
