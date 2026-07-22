package inference

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"unicode"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
)

const codexBaseInstructions = "You are Codex, a coding agent. Follow the user's instructions and use the available tools to complete software engineering tasks. Inspect relevant files before editing, preserve unrelated changes, and verify the result."

type codexReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type codexTruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
}

type codexModelEntry struct {
	Slug                              string                `json:"slug"`
	DisplayName                       string                `json:"display_name"`
	Description                       string                `json:"description"`
	DefaultReasoningLevel             string                `json:"default_reasoning_level"`
	SupportedReasoningLevels          []codexReasoningLevel `json:"supported_reasoning_levels"`
	ShellType                         string                `json:"shell_type"`
	Visibility                        string                `json:"visibility"`
	MinimalClientVersion              string                `json:"minimal_client_version"`
	SupportedInAPI                    bool                  `json:"supported_in_api"`
	Priority                          int                   `json:"priority"`
	AdditionalSpeedTiers              []string              `json:"additional_speed_tiers"`
	ServiceTiers                      []any                 `json:"service_tiers"`
	DefaultServiceTier                *string               `json:"default_service_tier"`
	AvailabilityNUX                   any                   `json:"availability_nux"`
	Upgrade                           any                   `json:"upgrade"`
	BaseInstructions                  string                `json:"base_instructions"`
	ModelMessages                     any                   `json:"model_messages"`
	IncludeSkillsUsageInstructions    bool                  `json:"include_skills_usage_instructions"`
	SupportsReasoningSummaryParameter bool                  `json:"supports_reasoning_summary_parameter"`
	SupportsReasoningSummaries        bool                  `json:"supports_reasoning_summaries"`
	DefaultReasoningSummary           string                `json:"default_reasoning_summary"`
	SupportVerbosity                  bool                  `json:"support_verbosity"`
	DefaultVerbosity                  *string               `json:"default_verbosity"`
	ApplyPatchToolType                *string               `json:"apply_patch_tool_type"`
	WebSearchToolType                 string                `json:"web_search_tool_type"`
	TruncationPolicy                  codexTruncationPolicy `json:"truncation_policy"`
	SupportsParallelToolCalls         bool                  `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal       bool                  `json:"supports_image_detail_original"`
	ContextWindow                     int                   `json:"context_window"`
	MaxContextWindow                  int                   `json:"max_context_window"`
	AutoCompactTokenLimit             *int                  `json:"auto_compact_token_limit"`
	EffectiveContextWindowPercent     int                   `json:"effective_context_window_percent"`
	ExperimentalSupportedTools        []string              `json:"experimental_supported_tools"`
	InputModalities                   []string              `json:"input_modalities"`
	SupportsSearchTool                bool                  `json:"supports_search_tool"`
	UseResponsesLite                  bool                  `json:"use_responses_lite"`
}

type codexModelCatalog struct {
	Models []codexModelEntry `json:"models"`
}

var codexReasoningDescriptions = map[string]string{
	"none":   "No reasoning",
	"low":    "Fast responses with lighter reasoning",
	"medium": "Balances speed and reasoning depth for everyday tasks",
	"high":   "Greater reasoning depth for complex problems",
	"xhigh":  "Extra high reasoning depth for complex problems",
	"max":    "Maximum reasoning depth for the hardest problems",
}

type grokModelCapability struct {
	contextWindow   int
	reasoningLevels []string
	description     string
	imageInput      bool
}

var grokCapabilities = map[string]grokModelCapability{
	"grok-4.5":                     {500000, []string{"low", "medium", "high"}, "xAI Grok 4.5 frontier model with reasoning and vision.", true},
	"grok-4.3":                     {1000000, []string{"none", "low", "medium", "high"}, "xAI Grok 4.3 high-capacity reasoning model.", true},
	"grok-build-0.1":               {256000, []string{"none"}, "xAI Grok Build 0.1 coding model.", false},
	"grok-4.20-0309-reasoning":     {2000000, []string{"low", "medium", "high"}, "xAI Grok 4.20 reasoning model.", true},
	"grok-4.20-0309-non-reasoning": {2000000, []string{"none"}, "xAI Grok 4.20 non-reasoning model.", true},
	"grok-4.20-multi-agent-0309":   {2000000, []string{"low", "medium", "high"}, "xAI Grok 4.20 multi-agent model.", true},
	"grok-3-mini":                  {131072, []string{"low", "medium", "high"}, "xAI Grok 3 Mini model.", false},
	"grok-3-mini-fast":             {131072, []string{"low", "medium", "high"}, "xAI Grok 3 Mini Fast model.", false},
	"grok-composer-2.5-fast":       {200000, []string{"none"}, "xAI Grok Composer 2.5 model.", false},
}

var grokDefaultCapability = grokModelCapability{
	contextWindow:   128000,
	reasoningLevels: []string{"none"},
	description:     "Grok model served via grok2api.",
}

func lookupGrokCapability(slug string) grokModelCapability {
	if capability, ok := grokCapabilities[slug]; ok {
		return capability
	}
	return grokDefaultCapability
}

func defaultReasoningLevel(levels []string) string {
	for _, level := range levels {
		if level == "medium" {
			return level
		}
	}
	if len(levels) > 0 {
		return levels[0]
	}
	return "none"
}

func codexVisibilityForCapability(capability modeldomain.Capability) string {
	switch capability {
	case modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit, modeldomain.CapabilityVideo:
		return "hide"
	default:
		return "list"
	}
}

func codexReasoningLevelsFor(levels []string) []codexReasoningLevel {
	result := make([]codexReasoningLevel, 0, len(levels))
	for _, level := range levels {
		result = append(result, codexReasoningLevel{Effort: level, Description: codexReasoningDescriptions[level]})
	}
	return result
}

func codexAgentToolsSupported(item modelListItem) bool {
	return item.Provider == account.ProviderBuild && item.Capability == modeldomain.CapabilityResponses
}

func codexReasoningSupported(levels []string) bool {
	for _, level := range levels {
		if level != "none" {
			return true
		}
	}
	return false
}

func newCodexModelCatalog(items []modelListItem) codexModelCatalog {
	models := make([]codexModelEntry, 0, len(items))
	for index, item := range items {
		capability := lookupGrokCapability(item.ID)
		modalities := []string{"text"}
		if capability.imageInput {
			modalities = append(modalities, "image")
		}
		var applyPatchToolType *string
		toolsSupported := codexAgentToolsSupported(item)
		if toolsSupported {
			value := "freeform"
			applyPatchToolType = &value
		}
		reasoningSupported := codexReasoningSupported(capability.reasoningLevels)
		models = append(models, codexModelEntry{
			Slug:                              item.ID,
			DisplayName:                       codexDisplayName(item.ID),
			Description:                       capability.description,
			DefaultReasoningLevel:             defaultReasoningLevel(capability.reasoningLevels),
			SupportedReasoningLevels:          codexReasoningLevelsFor(capability.reasoningLevels),
			ShellType:                         "shell_command",
			Visibility:                        codexVisibilityForCapability(item.Capability),
			MinimalClientVersion:              "0.0.0",
			SupportedInAPI:                    true,
			Priority:                          index + 1,
			AdditionalSpeedTiers:              []string{},
			ServiceTiers:                      []any{},
			BaseInstructions:                  codexBaseInstructions,
			IncludeSkillsUsageInstructions:    false,
			SupportsReasoningSummaryParameter: reasoningSupported,
			SupportsReasoningSummaries:        reasoningSupported,
			DefaultReasoningSummary:           "auto",
			SupportVerbosity:                  false,
			ApplyPatchToolType:                applyPatchToolType,
			WebSearchToolType:                 "text",
			TruncationPolicy:                  codexTruncationPolicy{Mode: "tokens", Limit: 10000},
			SupportsParallelToolCalls:         toolsSupported,
			SupportsImageDetailOriginal:       false,
			ContextWindow:                     capability.contextWindow,
			MaxContextWindow:                  capability.contextWindow,
			EffectiveContextWindowPercent:     95,
			ExperimentalSupportedTools:        []string{},
			InputModalities:                   modalities,
			SupportsSearchTool:                false,
			UseResponsesLite:                  false,
		})
	}
	return codexModelCatalog{Models: models}
}

func writeCodexModelCatalog(c *gin.Context, catalog codexModelCatalog) {
	body, err := json.Marshal(catalog)
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "model_list_failed", "编码模型列表失败")
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	c.Header("ETag", etag)
	if strings.TrimSpace(c.GetHeader("If-None-Match")) == etag {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func codexDisplayName(slug string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(slug))
	for index, word := range words {
		runes := []rune(word)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
			words[index] = string(runes)
		}
	}
	return strings.Join(words, " ")
}
