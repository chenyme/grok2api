package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const qualityProbeMaxStreamBytes = 4 << 20

type qualityProbeChatEvent struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ThinkingContent  string `json:"thinking_content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens            int64 `json:"prompt_tokens"`
		CompletionTokens        int64 `json:"completion_tokens"`
		TotalTokens             int64 `json:"total_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func (s *Service) ProbeEgressQuality(ctx context.Context, nodeID uint64, input egressapp.QualityProbeInput) (egressapp.QualityProbeResult, error) {
	key, err := s.clientKeys.Get(ctx, input.ClientKeyID)
	if err != nil {
		return egressapp.QualityProbeResult{}, fmt.Errorf("读取质量探测 Client Key: %w", err)
	}
	if !key.IsAvailable(time.Now().UTC()) {
		return egressapp.QualityProbeResult{}, fmt.Errorf("质量探测 Client Key 已禁用或过期")
	}
	requestIDPart, err := security.NewOpaqueToken(12)
	if err != nil {
		return egressapp.QualityProbeResult{}, err
	}
	requestID := "quality_" + requestIDPart
	body, err := json.Marshal(map[string]any{
		"model":          input.Model,
		"messages":       []map[string]string{{"role": "user", "content": input.Prompt}},
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": true},
		"max_tokens":     input.MaxOutputTokens,
	})
	if err != nil {
		return egressapp.QualityProbeResult{}, err
	}

	startedAt := time.Now()
	publicModel, ok := qualityProbeBuildPublicModel(input.Model)
	if !ok {
		return egressapp.QualityProbeResult{}, fmt.Errorf("%w: 质量探测模型必须属于 Grok Build", egressapp.ErrInvalidInput)
	}
	probeCtx := infraegress.WithQualityProbe(ctx)
	result, err := s.CreateChatCompletion(probeCtx, Input{
		RequestID: requestID, ClientKey: key, PublicModel: publicModel, Body: body,
		Streaming: true, Operation: audit.OperationChat, ForcedEgressNodeID: nodeID,
	})
	if err != nil {
		return egressapp.QualityProbeResult{}, normalizeQualityProbeRequestError(err)
	}
	defer result.Body.Close()

	usage := Usage{}
	responseID := ""
	errorCode := ""
	defer func() { result.Finalize(usage, responseID, errorCode) }()
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		errorCode = "quality_probe_upstream_error"
		body, _ := io.ReadAll(io.LimitReader(result.Body, 32<<10))
		return egressapp.QualityProbeResult{}, fmt.Errorf("质量探测上游返回 %d: %s", result.StatusCode, strings.TrimSpace(string(body)))
	}

	var firstGeneratedAt time.Time
	var visible strings.Builder
	chunkCount := 0
	totalBytes := 0
	terminal := false
	scanner := bufio.NewScanner(result.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		totalBytes += len(line) + 1
		if totalBytes > qualityProbeMaxStreamBytes {
			errorCode = "quality_probe_response_too_large"
			return egressapp.QualityProbeResult{}, fmt.Errorf("质量探测响应超过 %d MiB", qualityProbeMaxStreamBytes>>20)
		}
		line = []byte(strings.TrimSpace(string(line)))
		if bytes.Equal(line, []byte(": grok2api-reasoning-start")) {
			if firstGeneratedAt.IsZero() {
				firstGeneratedAt = time.Now()
				if result.MarkFirstToken != nil {
					result.MarkFirstToken()
				}
			}
			continue
		}
		if !strings.HasPrefix(string(line), "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(string(line), "data:"))
		if payload == "[DONE]" {
			terminal = true
			break
		}
		var event qualityProbeChatEvent
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		if responseID == "" {
			responseID = event.ID
		}
		if event.Usage != nil {
			usage.Reported = true
			usage.InputTokens = event.Usage.PromptTokens
			usage.OutputTokens = event.Usage.CompletionTokens
			usage.ReasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
			usage.TotalTokens = event.Usage.TotalTokens
			usage.ResponseModel = event.Model
		}
		for _, choice := range event.Choices {
			delta := choice.Delta
			generated := qualityProbeHasGeneratedDelta(delta.Content, delta.Reasoning, delta.ReasoningContent, delta.ThinkingContent)
			if generated && firstGeneratedAt.IsZero() {
				firstGeneratedAt = time.Now()
				if result.MarkFirstToken != nil {
					result.MarkFirstToken()
				}
			}
			if delta.Content != "" {
				visible.WriteString(delta.Content)
				chunkCount++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		errorCode = "quality_probe_stream_interrupted"
		return egressapp.QualityProbeResult{}, fmt.Errorf("读取质量探测流: %w", err)
	}
	if !terminal {
		errorCode = "quality_probe_stream_incomplete"
		return egressapp.QualityProbeResult{}, errors.New("质量探测流未正常结束")
	}

	completedAt := time.Now()
	text := visible.String()
	visibleCharacters := utf8.RuneCountInString(text)
	// Visible tokens are diagnostic only; TPS intentionally uses total output tokens to match the audit panel.
	visibleTokens := usage.OutputTokens - usage.ReasoningTokens
	if visibleTokens <= 0 && visibleCharacters > 0 {
		visibleTokens = int64((visibleCharacters + 3) / 4)
	}
	var firstTokenMS int64
	if !firstGeneratedAt.IsZero() {
		firstTokenMS = firstGeneratedAt.Sub(startedAt).Milliseconds()
	}
	durationMS := completedAt.Sub(startedAt).Milliseconds()
	var generationMS int64
	if !firstGeneratedAt.IsZero() {
		generationMS = audit.GenerationWindowMS(firstTokenMS, durationMS, usage.ReasoningTokens)
		if generationMS < 1 {
			generationMS = 1
		}
	}
	var outputTokensPerSecond float64
	if !firstGeneratedAt.IsZero() {
		outputTokensPerSecond = qualityProbeOutputTokensPerSecond(usage.OutputTokens, usage.ReasoningTokens, durationMS, firstTokenMS)
	}
	digest := sha256.Sum256([]byte(text))
	return egressapp.QualityProbeResult{
		RequestID: requestID, NodeID: nodeID, Model: input.Model, StatusCode: result.StatusCode,
		FirstTokenMS: firstTokenMS, DurationMS: durationMS, GenerationMS: generationMS,
		ChunkCount: chunkCount, OutputTokens: usage.OutputTokens, ReasoningTokens: usage.ReasoningTokens,
		VisibleTokens: visibleTokens, VisibleCharacters: visibleCharacters, OutputTokensPerSecond: outputTokensPerSecond,
		ExpectedMatched: egressapp.MatchExpected(text, input.Expected, input.MatchMode), ResponseSHA256: hex.EncodeToString(digest[:]),
	}, nil
}

func qualityProbeBuildPublicModel(value string) (string, bool) {
	return modeldomain.NormalizePublicID(accountdomain.ProviderBuild, value)
}

func normalizeQualityProbeRequestError(err error) error {
	if errors.Is(err, ErrNoAvailableAccount) {
		return egressapp.ErrQualityProbeNoAccount
	}
	return err
}

func qualityProbeOutputTokensPerSecond(outputTokens, reasoningTokens, durationMS, firstTokenMS int64) float64 {
	return audit.OutputTokensPerSecond(outputTokens, reasoningTokens, firstTokenMS, durationMS)
}

func qualityProbeHasGeneratedDelta(content, reasoning, reasoningContent, thinkingContent string) bool {
	return content != "" || reasoning != "" || reasoningContent != "" || thinkingContent != ""
}
