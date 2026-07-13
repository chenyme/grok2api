package settings

import (
	"errors"
	"net/http"
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
	ProviderBuild     providerBuildConfigDTO     `json:"providerBuild"`
	ProviderWeb       providerWebConfigDTO       `json:"providerWeb"`
	Batch             batchConfigDTO             `json:"batch"`
	Media             mediaConfigDTO             `json:"media"`
	Routing           routingConfigDTO           `json:"routing"`
	Audit             auditConfigDTO             `json:"audit"`
	ClientKeyDefaults clientKeyDefaultsConfigDTO `json:"clientKeyDefaults"`
}

type mediaConfigDTO struct {
	MaxImageBytes           int64  `json:"maxImageBytes"`
	MaxTotalBytes           int64  `json:"maxTotalBytes"`
	CleanupThresholdPercent int    `json:"cleanupThresholdPercent"`
	CleanupInterval         string `json:"cleanupInterval"`
}

type providerBuildConfigDTO struct {
	BaseURL          string `json:"baseURL"`
	ClientVersion    string `json:"clientVersion"`
	ClientIdentifier string `json:"clientIdentifier"`
	TokenAuth        string `json:"tokenAuth"`
	UserAgent        string `json:"userAgent"`
}

type providerWebConfigDTO struct {
	BaseURL                 string `json:"baseURL"`
	StatsigMode             string `json:"statsigMode"`
	StatsigManualValue      string `json:"statsigManualValue,omitempty"`
	StatsigManualConfigured bool   `json:"statsigManualConfigured"`
	StatsigSignerURL        string `json:"statsigSignerURL"`
	QuotaTimeout            string `json:"quotaTimeout"`
	ChatTimeout             string `json:"chatTimeout"`
	ImageTimeout            string `json:"imageTimeout"`
	VideoTimeout            string `json:"videoTimeout"`
	MediaConcurrency        int    `json:"mediaConcurrency"`
	AllowNSFW               bool   `json:"allowNSFW"`
	RecoveryBackoffBase     string `json:"recoveryBackoffBase"`
	RecoveryBackoffMax      string `json:"recoveryBackoffMax"`
}

type batchConfigDTO struct {
	ImportConcurrency     int    `json:"importConcurrency"`
	ConversionConcurrency int    `json:"conversionConcurrency"`
	SyncConcurrency       int    `json:"syncConcurrency"`
	RefreshConcurrency    int    `json:"refreshConcurrency"`
	RandomDelay           string `json:"randomDelay"`
}

type routingConfigDTO struct {
	StickyTTL    string `json:"stickyTTL"`
	CooldownBase string `json:"cooldownBase"`
	CooldownMax  string `json:"cooldownMax"`
	MaxAttempts  int    `json:"maxAttempts"`
}

type auditConfigDTO struct {
	BufferSize    int    `json:"bufferSize"`
	BatchSize     int    `json:"batchSize"`
	FlushInterval string `json:"flushInterval"`
}

type clientKeyDefaultsConfigDTO struct {
	RPMLimit      int `json:"rpmLimit"`
	MaxConcurrent int `json:"maxConcurrent"`
}

type settingsResponse struct {
	Config          settingsConfigDTO `json:"config"`
	UpdatedAt       time.Time         `json:"updatedAt"`
	Revision        uint64            `json:"revision,string"`
	RestartRequired []string          `json:"restartRequired"`
}

type updateRequest struct {
	Revision uint64            `json:"revision,string" binding:"required"`
	Config   settingsConfigDTO `json:"config" binding:"required"`
}

func (h *Handler) get(c *gin.Context) {
	response.Success(c, http.StatusOK, newSettingsResponse(h.service.Get()))
}

func (h *Handler) update(c *gin.Context) {
	var request updateRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
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
	return settingsapp.EditableConfig{
		ProviderBuild: settingsapp.ProviderBuildConfig{
			BaseURL: value.ProviderBuild.BaseURL, ClientVersion: value.ProviderBuild.ClientVersion,
			ClientIdentifier: value.ProviderBuild.ClientIdentifier, TokenAuth: value.ProviderBuild.TokenAuth,
			UserAgent: value.ProviderBuild.UserAgent,
		},
		ProviderWeb: settingsapp.ProviderWebConfig{
			BaseURL: value.ProviderWeb.BaseURL, QuotaTimeout: value.ProviderWeb.QuotaTimeout,
			StatsigMode: value.ProviderWeb.StatsigMode, StatsigManualValue: value.ProviderWeb.StatsigManualValue,
			StatsigManualConfigured: value.ProviderWeb.StatsigManualConfigured, StatsigSignerURL: value.ProviderWeb.StatsigSignerURL,
			ChatTimeout: value.ProviderWeb.ChatTimeout, ImageTimeout: value.ProviderWeb.ImageTimeout,
			VideoTimeout:     value.ProviderWeb.VideoTimeout,
			MediaConcurrency: value.ProviderWeb.MediaConcurrency, AllowNSFW: value.ProviderWeb.AllowNSFW,
			RecoveryBackoffBase: value.ProviderWeb.RecoveryBackoffBase, RecoveryBackoffMax: value.ProviderWeb.RecoveryBackoffMax,
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
		Routing: settingsapp.RoutingConfig{
			StickyTTL: value.Routing.StickyTTL, CooldownBase: value.Routing.CooldownBase,
			CooldownMax: value.Routing.CooldownMax, MaxAttempts: value.Routing.MaxAttempts,
		},
		Audit: settingsapp.AuditConfig{
			BufferSize: value.Audit.BufferSize, BatchSize: value.Audit.BatchSize, FlushInterval: value.Audit.FlushInterval,
		},
		ClientKeyDefaults: settingsapp.ClientKeyDefaultsConfig{
			RPMLimit: value.ClientKeyDefaults.RPMLimit, MaxConcurrent: value.ClientKeyDefaults.MaxConcurrent,
		},
	}
}

func newSettingsResponse(value settingsapp.Snapshot) settingsResponse {
	config := value.Config
	return settingsResponse{
		Config: settingsConfigDTO{
			ProviderBuild: providerBuildConfigDTO{
				BaseURL: config.ProviderBuild.BaseURL, ClientVersion: config.ProviderBuild.ClientVersion,
				ClientIdentifier: config.ProviderBuild.ClientIdentifier, TokenAuth: config.ProviderBuild.TokenAuth,
				UserAgent: config.ProviderBuild.UserAgent,
			},
			ProviderWeb: providerWebConfigDTO{
				BaseURL: config.ProviderWeb.BaseURL, QuotaTimeout: config.ProviderWeb.QuotaTimeout,
				StatsigMode: config.ProviderWeb.StatsigMode, StatsigManualConfigured: config.ProviderWeb.StatsigManualConfigured,
				StatsigSignerURL: config.ProviderWeb.StatsigSignerURL,
				ChatTimeout:      config.ProviderWeb.ChatTimeout, ImageTimeout: config.ProviderWeb.ImageTimeout,
				VideoTimeout:     config.ProviderWeb.VideoTimeout,
				MediaConcurrency: config.ProviderWeb.MediaConcurrency, AllowNSFW: config.ProviderWeb.AllowNSFW,
				RecoveryBackoffBase: config.ProviderWeb.RecoveryBackoffBase, RecoveryBackoffMax: config.ProviderWeb.RecoveryBackoffMax,
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
			Routing: routingConfigDTO{
				StickyTTL: config.Routing.StickyTTL, CooldownBase: config.Routing.CooldownBase,
				CooldownMax: config.Routing.CooldownMax, MaxAttempts: config.Routing.MaxAttempts,
			},
			Audit: auditConfigDTO{
				BufferSize: config.Audit.BufferSize, BatchSize: config.Audit.BatchSize, FlushInterval: config.Audit.FlushInterval,
			},
			ClientKeyDefaults: clientKeyDefaultsConfigDTO{
				RPMLimit: config.ClientKeyDefaults.RPMLimit, MaxConcurrent: config.ClientKeyDefaults.MaxConcurrent,
			},
		},
		UpdatedAt: value.UpdatedAt, Revision: value.Revision, RestartRequired: value.RestartRequired,
	}
}
