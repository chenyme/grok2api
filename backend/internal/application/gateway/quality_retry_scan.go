package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

const (
	qualityProtocolChat        = "chat"
	qualityProtocolResponses   = "responses"
	qualityProtocolAnthropic   = "anthropic"
	qualityReasoningSSEComment = ": grok2api-reasoning-start"
	qualityHoldMaxBufferBytes  = 4 << 20
)

type qualityScanState struct {
	protocol        string
	pending         []byte
	hasThinking     bool
	visibleRunes    int
	reasoningTokens int64
	outputTokens    int64
	usage           Usage
	responseID      string
	terminal        bool
}

type qualityReadResult struct {
	data []byte
	err  error
}

// qualityReadPump is the sole reader of the upstream body. It lets the hold
// timer win while an upstream Read is blocked, then remains the continuation
// reader for the response body after the held prefix is replayed.
type qualityReadPump struct {
	source    io.ReadCloser
	results   chan qualityReadResult
	done      chan struct{}
	closeOnce sync.Once
	pending   []byte
	finalErr  error
}

func newQualityReadPump(source io.ReadCloser) *qualityReadPump {
	pump := &qualityReadPump{
		source:  source,
		results: make(chan qualityReadResult),
		done:    make(chan struct{}),
	}
	go pump.run()
	return pump
}

func (p *qualityReadPump) run() {
	defer close(p.results)
	buf := make([]byte, 4096)
	for {
		n, err := p.source.Read(buf)
		if n == 0 && err == nil {
			continue
		}
		result := qualityReadResult{err: err}
		if n > 0 {
			result.data = append([]byte(nil), buf[:n]...)
		}
		select {
		case p.results <- result:
		case <-p.done:
			return
		}
		if err != nil {
			return
		}
	}
}

func (p *qualityReadPump) Read(dst []byte) (int, error) {
	for len(p.pending) == 0 {
		if p.finalErr != nil {
			return 0, p.finalErr
		}
		result, ok := <-p.results
		if !ok {
			p.finalErr = io.EOF
			return 0, io.EOF
		}
		p.pending = result.data
		p.finalErr = result.err
		if len(p.pending) == 0 && p.finalErr != nil {
			return 0, p.finalErr
		}
	}
	n := copy(dst, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *qualityReadPump) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.done)
		err = p.source.Close()
	})
	return err
}

func qualityProtocolForOperation(operation audit.Operation) string {
	switch operation {
	case audit.OperationChat:
		return qualityProtocolChat
	case audit.OperationMessages:
		return qualityProtocolAnthropic
	default:
		return qualityProtocolResponses
	}
}

func (s *qualityScanState) signals() QualityStreamSignals {
	visible := int64((s.visibleRunes + 3) / 4)
	if s.usage.Reported {
		fromUsage := s.usage.OutputTokens - s.usage.ReasoningTokens
		if fromUsage > visible {
			visible = fromUsage
		}
	}
	output := s.outputTokens
	if s.usage.Reported && s.usage.OutputTokens > output {
		output = s.usage.OutputTokens
	}
	return QualityStreamSignals{
		HasThinking:     s.hasThinking || s.reasoningTokens > 0 || s.usage.ReasoningTokens > 0,
		VisibleTokens:   visible,
		ReasoningTokens: max(s.reasoningTokens, s.usage.ReasoningTokens),
		OutputTokens:    output,
		Terminal:        s.terminal,
	}
}

// ObserveQualityChunk feeds one SSE chunk into the hold classifier state.
// This is the shipped scanner used by peekQualityStream.
func ObserveQualityChunk(state *qualityScanState, chunk []byte) {
	if state == nil || len(chunk) == 0 {
		return
	}
	state.pending = append(state.pending, chunk...)
	for {
		index := bytes.IndexByte(state.pending, '\n')
		if index < 0 {
			if len(state.pending) > 1<<20 {
				state.pending = nil
			}
			return
		}
		line := bytes.TrimSpace(state.pending[:index])
		state.pending = state.pending[index+1:]
		if len(line) == 0 {
			continue
		}
		if bytes.Equal(line, []byte(qualityReasoningSSEComment)) {
			state.hasThinking = true
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(payload, []byte("[DONE]")) {
			state.terminal = true
			continue
		}
		observeQualityPayload(state, payload)
	}
}

func observeQualityPayload(state *qualityScanState, payload []byte) {
	switch state.protocol {
	case qualityProtocolChat:
		observeQualityChat(state, payload)
	case qualityProtocolAnthropic:
		observeQualityAnthropic(state, payload)
	default:
		observeQualityResponses(state, payload)
	}
}

func observeQualityChat(state *qualityScanState, payload []byte) {
	var event struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				ThinkingContent  string `json:"thinking_content"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
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
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	if state.responseID == "" {
		state.responseID = event.ID
	}
	if event.Usage != nil {
		state.usage.Reported = true
		state.usage.InputTokens = event.Usage.PromptTokens
		state.usage.OutputTokens = event.Usage.CompletionTokens
		state.usage.ReasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
		state.usage.TotalTokens = event.Usage.TotalTokens
		state.usage.ResponseModel = event.Model
		state.outputTokens = event.Usage.CompletionTokens
		state.reasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
		if state.reasoningTokens > 0 {
			state.hasThinking = true
		}
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		if delta.Reasoning != "" || delta.ReasoningContent != "" || delta.ThinkingContent != "" {
			state.hasThinking = true
		}
		if delta.Content != "" {
			noteVisibleContent(state, delta.Content)
		}
		if choice.FinishReason != "" {
			state.terminal = true
		}
	}
}

func observeQualityResponses(state *qualityScanState, payload []byte) {
	var event struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
		Item  struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"item"`
		Response *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				OutputTokens        int64 `json:"output_tokens"`
				InputTokens         int64 `json:"input_tokens"`
				TotalTokens         int64 `json:"total_tokens"`
				OutputTokensDetails struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "response.completed", "response.incomplete", "response.failed":
		state.terminal = true
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if event.Delta != "" {
			state.hasThinking = true
		}
	case "response.output_item.added":
		if event.Item.Type == "reasoning" && event.Item.ID != "" {
			state.hasThinking = true
		}
	case "response.output_text.delta":
		if event.Delta != "" {
			noteVisibleContent(state, event.Delta)
		}
	}
	if event.Response != nil {
		if state.responseID == "" {
			state.responseID = event.Response.ID
		}
		if event.Response.Usage != nil {
			state.usage.Reported = true
			state.usage.InputTokens = event.Response.Usage.InputTokens
			state.usage.OutputTokens = event.Response.Usage.OutputTokens
			state.usage.ReasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
			state.usage.TotalTokens = event.Response.Usage.TotalTokens
			state.usage.ResponseModel = event.Response.Model
			state.outputTokens = event.Response.Usage.OutputTokens
			state.reasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
			if state.reasoningTokens > 0 {
				state.hasThinking = true
			}
		}
	}
}

func observeQualityAnthropic(state *qualityScanState, payload []byte) {
	var event struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
		Usage *struct {
			OutputTokens        int64 `json:"output_tokens"`
			OutputTokensDetails struct {
				ThinkingTokens int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "message_stop":
		state.terminal = true
	case "content_block_start":
		if event.ContentBlock.Type == "thinking" {
			state.hasThinking = true
		}
	case "content_block_delta":
		if event.Delta.Type == "thinking_delta" && event.Delta.Thinking != "" {
			state.hasThinking = true
		}
		if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			noteVisibleContent(state, event.Delta.Text)
		}
	}
	if event.Usage != nil {
		state.usage.Reported = true
		state.usage.OutputTokens = event.Usage.OutputTokens
		state.usage.ReasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
		state.outputTokens = event.Usage.OutputTokens
		state.reasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
		if state.reasoningTokens > 0 {
			state.hasThinking = true
		}
	}
}

func noteVisibleContent(state *qualityScanState, text string) {
	if text == "" {
		return
	}
	state.visibleRunes += utf8.RuneCountInString(text)
}

func peekQualityStream(ctx context.Context, body io.ReadCloser, protocol string, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, string, error) {
	cfg = normalizeQualityRetry(cfg)
	if body == nil {
		return io.NopCloser(bytes.NewReader(nil)), QualityDeliver, Usage{}, "", nil
	}
	pump := newQualityReadPump(body)
	state := qualityScanState{protocol: protocol}
	var held bytes.Buffer
	holdTimer := time.NewTimer(cfg.HoldTimeout)
	defer holdTimer.Stop()
	for {
		sig := state.signals()
		if verdict := ClassifyQualityHold(sig, cfg.MinOutputTokens); verdict != QualityWait {
			return newPrefixReplay(&held, pump), verdict, state.usage, state.responseID, nil
		}

		select {
		case <-ctx.Done():
			_ = pump.Close()
			return io.NopCloser(bytes.NewReader(held.Bytes())), QualityDeliver, state.usage, state.responseID, ctx.Err()
		case <-holdTimer.C:
			sig.HoldExpired = true
			return newPrefixReplay(&held, pump), ClassifyQualityHold(sig, cfg.MinOutputTokens), state.usage, state.responseID, nil
		case result, ok := <-pump.results:
			if !ok {
				state.terminal = true
				return newPrefixReplay(&held, pump), ClassifyQualityHold(state.signals(), cfg.MinOutputTokens), state.usage, state.responseID, nil
			}
			if len(result.data) > 0 {
				if held.Len()+len(result.data) > qualityHoldMaxBufferBytes {
					_, _ = held.Write(result.data)
					return newPrefixReplay(&held, pump), QualityDeliver, state.usage, state.responseID, nil
				}
				_, _ = held.Write(result.data)
				ObserveQualityChunk(&state, result.data)
			}
			if result.err == io.EOF {
				state.terminal = true
				return newPrefixReplay(&held, pump), ClassifyQualityHold(state.signals(), cfg.MinOutputTokens), state.usage, state.responseID, nil
			}
			if result.err != nil {
				_ = pump.Close()
				return io.NopCloser(bytes.NewReader(held.Bytes())), QualityDeliver, state.usage, state.responseID, result.err
			}
		}
	}
}

func newPrefixReplay(held *bytes.Buffer, rest io.ReadCloser) io.ReadCloser {
	if rest == nil {
		rest = io.NopCloser(bytes.NewReader(nil))
	}
	if held == nil || held.Len() == 0 {
		return rest
	}
	return &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(held.Bytes()), rest), source: rest}
}
