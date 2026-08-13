package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

const (
	videoOptionParseTimeout     = 45 * time.Second
	videoOptionAttemptTimeout   = 15 * time.Second
	videoOptionMaxResponseBytes = 64 << 10
)

var defaultVideoOptionModels = []string{
	"grok-4.6",
	"grok-4.5",
	"grok-4.20-0309-non-reasoning",
	"grok-4.20-0309-reasoning",
	"grok-4.3",
	"grok-chat-expert",
	"grok-chat-auto",
	"grok-chat-heavy",
	"grok-chat-fast",
	"grok-4.20-multi-agent-0309",
	"grok-3-mini-fast",
	"grok-3-mini",
	"grok-build-0.1",
}

// VideoOptionParseInput describes only the final selected user prompt and the
// fields that the client omitted. The original prompt is never rewritten.
type VideoOptionParseInput struct {
	RequestID             string
	ClientKey             clientkey.Key
	Prompt                string
	NeedDuration          bool
	NeedAspectRatio       bool
	NeedResolution        bool
	PreferredPublicModels []string
}

type VideoOptionHints struct {
	Duration    *int    `json:"duration"`
	AspectRatio *string `json:"aspect_ratio"`
	Resolution  *string `json:"resolution"`
}

type enabledRouteLister interface {
	ListEnabled(context.Context) ([]modeldomain.Route, error)
}

type videoOptionChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens           int64 `json:"prompt_tokens"`
		CompletionTokens       int64 `json:"completion_tokens"`
		TotalTokens            int64 `json:"total_tokens"`
		CostInUSDTicks         int64 `json:"cost_in_usd_ticks"`
		NumSourcesUsed         int64 `json:"num_sources_used"`
		NumServerSideToolsUsed int64 `json:"num_server_side_tools_used"`
		PromptTokensDetails    struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		ContextDetails struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"context_details"`
	} `json:"usage"`
}

// InferVideoOptions asks enabled text routes, in quality order, to extract only
// missing structured video options. It falls through every available provider.
func (s *Service) InferVideoOptions(ctx context.Context, input VideoOptionParseInput) (VideoOptionHints, error) {
	if strings.TrimSpace(input.Prompt) == "" || (!input.NeedDuration && !input.NeedAspectRatio && !input.NeedResolution) {
		return VideoOptionHints{}, nil
	}
	candidates, err := s.videoOptionModelCandidates(ctx, input.PreferredPublicModels)
	if err != nil {
		return VideoOptionHints{}, err
	}
	if len(candidates) == 0 {
		return VideoOptionHints{}, errors.New("没有可用文本模型用于解析视频参数")
	}
	parseCtx, cancel := context.WithTimeout(ctx, videoOptionParseTimeout)
	defer cancel()
	internalKey := input.ClientKey
	internalKey.AllowedModels = nil
	internalKey.ProviderScope = clientkey.ProviderScopeAll
	internalKey.TierScope = clientkey.TierScopeAll
	internalKey.AllowModelAliases = false
	internalKey.BillingLimitUSDTicks = 0

	var lastErr error
	for index, candidate := range candidates {
		attemptCtx, attemptCancel := context.WithTimeout(parseCtx, videoOptionAttemptTimeout)
		requestBody, marshalErr := buildVideoOptionRequest(input, candidate)
		if marshalErr != nil {
			attemptCancel()
			return VideoOptionHints{}, marshalErr
		}
		result, requestErr := s.CreateChatCompletion(attemptCtx, Input{
			RequestID:   fmt.Sprintf("%s_video_options_%d", input.RequestID, index+1),
			ClientKey:   internalKey,
			PublicModel: candidate,
			Body:        requestBody,
			Streaming:   false,
		})
		if requestErr != nil {
			attemptCancel()
			lastErr = requestErr
			if parseCtx.Err() != nil {
				break
			}
			continue
		}
		hints, readErr := readVideoOptionResult(result)
		attemptCancel()
		if readErr == nil {
			return hints, nil
		}
		lastErr = readErr
		if parseCtx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("视频参数解析模型没有返回有效结果")
	}
	if s.logger != nil {
		s.logger.Warn("video_option_parser_failed", "request_id", input.RequestID, "candidate_count", len(candidates), "error", lastErr)
	}
	return VideoOptionHints{}, lastErr
}

func (s *Service) videoOptionModelCandidates(ctx context.Context, preferred []string) ([]string, error) {
	lister, ok := s.models.(enabledRouteLister)
	if !ok {
		return nil, errors.New("模型仓储不支持列出视频参数解析候选")
	}
	routes, err := lister.ListEnabled(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取视频参数解析模型: %w", err)
	}
	priority := normalizeVideoOptionModelPriority(preferred)
	priorityIndex := make(map[string]int, len(priority))
	for index, value := range priority {
		priorityIndex[strings.ToLower(value)] = index
	}
	eligible := make([]modeldomain.Route, 0, len(routes))
	for _, route := range routes {
		if route.Capability != modeldomain.CapabilityResponses && route.Capability != modeldomain.CapabilityChat {
			continue
		}
		if s.providers == nil || !s.providers.SupportsConversation(route.Provider, "chat") {
			continue
		}
		eligible = append(eligible, route)
	}
	sort.SliceStable(eligible, func(left, right int) bool {
		leftPriority := videoOptionRoutePriority(eligible[left], priorityIndex, len(priority))
		rightPriority := videoOptionRoutePriority(eligible[right], priorityIndex, len(priority))
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if eligible[left].SupportedAccounts != eligible[right].SupportedAccounts {
			return eligible[left].SupportedAccounts > eligible[right].SupportedAccounts
		}
		leftProvider := videoOptionProviderPriority(eligible[left].Provider)
		rightProvider := videoOptionProviderPriority(eligible[right].Provider)
		if leftProvider != rightProvider {
			return leftProvider < rightProvider
		}
		return eligible[left].PublicID < eligible[right].PublicID
	})
	result := make([]string, 0, len(eligible))
	seen := make(map[uint64]struct{}, len(eligible))
	for _, route := range eligible {
		if _, exists := seen[route.ID]; exists {
			continue
		}
		seen[route.ID] = struct{}{}
		result = append(result, route.PublicID)
	}
	return result, nil
}

func normalizeVideoOptionModelPriority(preferred []string) []string {
	if len(preferred) == 0 {
		preferred = defaultVideoOptionModels
	}
	result := make([]string, 0, len(preferred))
	seen := make(map[string]struct{}, len(preferred))
	for _, value := range preferred {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return append([]string(nil), defaultVideoOptionModels...)
	}
	return result
}

func videoOptionRoutePriority(route modeldomain.Route, priority map[string]int, fallback int) int {
	for _, value := range []string{route.UpstreamModel, modeldomain.ExternalPublicID(route.Provider, route.PublicID)} {
		if index, ok := priority[strings.ToLower(strings.TrimSpace(value))]; ok {
			return index
		}
	}
	return fallback
}

func videoOptionProviderPriority(value account.Provider) int {
	switch value {
	case account.ProviderBuild:
		return 0
	case account.ProviderConsole:
		return 1
	case account.ProviderWeb:
		return 2
	default:
		return 3
	}
}

func buildVideoOptionRequest(input VideoOptionParseInput, publicModel string) ([]byte, error) {
	wanted := make([]string, 0, 3)
	if input.NeedDuration {
		wanted = append(wanted, "duration")
	}
	if input.NeedAspectRatio {
		wanted = append(wanted, "aspect_ratio")
	}
	if input.NeedResolution {
		wanted = append(wanted, "resolution")
	}
	system := `Extract only explicitly requested video generation settings from the user text. Return one JSON object and nothing else. Keys: duration (integer 1-15 seconds or null), aspect_ratio (one of "1:1","16:9","9:16","4:3","3:4","3:2","2:3" or null), resolution (one of "480p","720p","1080p" or null). Do not infer unspecified values. Understand the user's language without translating or rewriting their text.`
	return json.Marshal(map[string]any{
		"model": publicModel,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": fmt.Sprintf("Fields to extract: %s\nUser text:\n%s", strings.Join(wanted, ", "), input.Prompt)},
		},
		"stream":                false,
		"temperature":           0,
		"max_completion_tokens": 96,
		"response_format":       map[string]string{"type": "json_object"},
	})
}

func readVideoOptionResult(result *Result) (hints VideoOptionHints, err error) {
	if result == nil || result.Body == nil {
		return VideoOptionHints{}, errors.New("视频参数解析模型返回空响应")
	}
	usage := Usage{}
	responseID := ""
	errorCode := "video_option_parser_invalid_response"
	defer result.Body.Close()
	if result.Finalize != nil {
		defer func() { result.Finalize(usage, responseID, errorCode) }()
	}
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		errorCode = "video_option_parser_upstream_error"
		_, _ = io.Copy(io.Discard, io.LimitReader(result.Body, videoOptionMaxResponseBytes))
		return VideoOptionHints{}, fmt.Errorf("视频参数解析模型返回 HTTP %d", result.StatusCode)
	}
	body, readErr := io.ReadAll(io.LimitReader(result.Body, videoOptionMaxResponseBytes+1))
	if readErr != nil {
		return VideoOptionHints{}, fmt.Errorf("读取视频参数解析响应: %w", readErr)
	}
	if len(body) > videoOptionMaxResponseBytes {
		return VideoOptionHints{}, errors.New("视频参数解析响应过大")
	}
	var response videoOptionChatResponse
	if json.Unmarshal(body, &response) != nil || len(response.Choices) == 0 {
		return VideoOptionHints{}, errors.New("视频参数解析响应格式无效")
	}
	responseID = response.ID
	usage = Usage{
		InputTokens: response.Usage.PromptTokens, CachedInputTokens: response.Usage.PromptTokensDetails.CachedTokens,
		OutputTokens: response.Usage.CompletionTokens, ReasoningTokens: response.Usage.CompletionTokensDetails.ReasoningTokens,
		TotalTokens: response.Usage.TotalTokens, CostInUSDTicks: response.Usage.CostInUSDTicks,
		NumSourcesUsed: response.Usage.NumSourcesUsed, NumServerSideToolsUsed: response.Usage.NumServerSideToolsUsed,
		ContextInputTokens: response.Usage.ContextDetails.InputTokens, ContextOutputTokens: response.Usage.ContextDetails.OutputTokens,
		ResponseModel: response.Model,
	}
	hints, err = decodeVideoOptionHints(response.Choices[0].Message.Content)
	if err != nil {
		return VideoOptionHints{}, err
	}
	errorCode = ""
	return hints, nil
}

func decodeVideoOptionHints(value string) (VideoOptionHints, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "```") && strings.HasSuffix(value, "```") {
		lines := strings.Split(value, "\n")
		if len(lines) >= 3 {
			value = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
		}
	}
	var hints VideoOptionHints
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&hints); err != nil {
		return VideoOptionHints{}, fmt.Errorf("解析视频参数 JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return VideoOptionHints{}, errors.New("视频参数 JSON 包含额外内容")
	}
	if hints.Duration != nil && (*hints.Duration < 1 || *hints.Duration > 15) {
		return VideoOptionHints{}, errors.New("视频参数 duration 超出 1 到 15")
	}
	if hints.AspectRatio != nil {
		value := strings.TrimSpace(*hints.AspectRatio)
		if !validInferredVideoAspectRatio(value) {
			return VideoOptionHints{}, errors.New("视频参数 aspect_ratio 无效")
		}
		*hints.AspectRatio = value
	}
	if hints.Resolution != nil {
		value := strings.ToLower(strings.TrimSpace(*hints.Resolution))
		if value != "480p" && value != "720p" && value != "1080p" {
			return VideoOptionHints{}, errors.New("视频参数 resolution 无效")
		}
		*hints.Resolution = value
	}
	return hints, nil
}

func validInferredVideoAspectRatio(value string) bool {
	switch value {
	case "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3":
		return true
	default:
		return false
	}
}
