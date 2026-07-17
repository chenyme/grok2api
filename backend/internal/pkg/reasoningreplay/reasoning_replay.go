package reasoningreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	minReplayEncryptedLen     = 20
	maxReplayCaptureBytes     = 8 << 20
	defaultReasoningReplayTTL = time.Hour
)

// Config 控制服务端推理回放缓存。
type Config struct {
	Enabled bool
	TTL     time.Duration
}

// ReasoningReplay 封装存储与 body 注入/抽取。
type ReasoningReplay struct {
	store  repository.ReasoningReplayRepository
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
}

func New(store repository.ReasoningReplayRepository, cfg Config, logger *slog.Logger) *ReasoningReplay {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultReasoningReplayTTL
	}
	return &ReasoningReplay{store: store, cfg: cfg, logger: logger, now: time.Now}
}

func (r *ReasoningReplay) UpdateConfig(cfg Config) {
	if r == nil {
		return
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultReasoningReplayTTL
	}
	r.cfg = cfg
}

func (r *ReasoningReplay) Enabled() bool {
	return r != nil && r.cfg.Enabled && r.store != nil
}

// Apply 将缓存的上一轮 output items 注入 Responses body.input。
func (r *ReasoningReplay) Apply(ctx context.Context, model, sessionKey string, body []byte) []byte {
	if !r.Enabled() || strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(model) == "" || len(body) == 0 {
		return body
	}
	if previousResponseIDPresent(body) {
		r.logger.Debug("reasoning_replay_miss", "reason", "previous_response_id", "model", model)
		return body
	}
	items, ok, err := r.store.Get(ctx, model, sessionKey, r.now().UTC())
	if err != nil {
		r.logger.Warn("reasoning_replay_get_failed", "model", model, "error", err)
		return body
	}
	if !ok || len(items) == 0 {
		r.logger.Debug("reasoning_replay_miss", "reason", "not_found", "model", model)
		return body
	}
	filtered := filterReplayItemsForInput(body, items)
	if len(filtered) == 0 {
		r.logger.Debug("reasoning_replay_miss", "reason", "filtered", "model", model)
		return body
	}
	updated, ok := insertReplayItems(body, filtered)
	if !ok {
		r.logger.Debug("reasoning_replay_miss", "reason", "insert_failed", "model", model)
		return body
	}
	r.logger.Debug("reasoning_replay_hit", "model", model, "injected", len(filtered))
	return updated
}

// StoreFromCompleted 从完整 Responses JSON 写入回放缓存。
func (r *ReasoningReplay) StoreFromCompleted(ctx context.Context, model, sessionKey string, payload []byte) {
	if !r.Enabled() || strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(model) == "" {
		return
	}
	items := extractReplayItemsFromPayload(payload)
	normalized, ok := normalizeReplayItems(items)
	if !ok {
		if err := r.store.Delete(ctx, model, sessionKey); err != nil {
			r.logger.Warn("reasoning_replay_delete_failed", "model", model, "reason", "no_anchor", "error", err)
		} else {
			r.logger.Debug("reasoning_replay_delete", "model", model, "reason", "no_anchor")
		}
		return
	}
	expiresAt := r.now().UTC().Add(r.cfg.TTL)
	if err := r.store.Set(ctx, model, sessionKey, normalized, expiresAt); err != nil {
		r.logger.Warn("reasoning_replay_store_failed", "model", model, "error", err)
		return
	}
	r.logger.Debug("reasoning_replay_store", "model", model, "items", len(normalized))
}

// Clear 删除指定会话的回放缓存（compact 成功等）。
func (r *ReasoningReplay) Clear(ctx context.Context, model, sessionKey string) {
	if !r.Enabled() || strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(model) == "" {
		return
	}
	if err := r.store.Delete(ctx, model, sessionKey); err != nil {
		r.logger.Warn("reasoning_replay_delete_failed", "model", model, "error", err)
		return
	}
	r.logger.Debug("reasoning_replay_delete", "model", model, "reason", "explicit")
}

func previousResponseIDPresent(body []byte) bool {
	var payload struct {
		PreviousResponseID string `json:"previous_response_id"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	return strings.TrimSpace(payload.PreviousResponseID) != ""
}

func extractReplayItemsFromPayload(payload []byte) [][]byte {
	var root map[string]json.RawMessage
	if json.Unmarshal(payload, &root) != nil {
		return nil
	}
	outputRaw := root["output"]
	if len(outputRaw) == 0 {
		if respRaw, ok := root["response"]; ok {
			var nested map[string]json.RawMessage
			if json.Unmarshal(respRaw, &nested) == nil {
				outputRaw = nested["output"]
			}
		}
	}
	if len(outputRaw) == 0 {
		return nil
	}
	var output []json.RawMessage
	if json.Unmarshal(outputRaw, &output) != nil {
		return nil
	}
	items := make([][]byte, 0, len(output))
	for _, item := range output {
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &typed) != nil {
			continue
		}
		switch strings.TrimSpace(typed.Type) {
		case "reasoning", "message", "function_call", "custom_tool_call":
			items = append(items, append([]byte(nil), item...))
		}
	}
	return items
}

func normalizeReplayItems(items [][]byte) ([][]byte, bool) {
	normalized := make([][]byte, 0, len(items))
	hasAnchor := false
	for _, item := range items {
		next, ok := normalizeReplayItem(item)
		if !ok {
			continue
		}
		normalized = append(normalized, next)
		var typed struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(next, &typed)
		switch strings.TrimSpace(typed.Type) {
		case "reasoning", "function_call", "custom_tool_call":
			hasAnchor = true
		}
	}
	return normalized, hasAnchor && len(normalized) > 0
}

func normalizeReplayItem(item []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(item, &raw) != nil {
		return nil, false
	}
	var typeName string
	_ = json.Unmarshal(raw["type"], &typeName)
	switch strings.TrimSpace(typeName) {
	case "reasoning":
		return normalizeReasoningItem(raw)
	case "message":
		return normalizeAssistantMessageItem(raw)
	case "function_call":
		return normalizeFunctionCallItem(raw)
	case "custom_tool_call":
		return normalizeCustomToolCallItem(raw)
	default:
		return nil, false
	}
}

func normalizeReasoningItem(raw map[string]json.RawMessage) ([]byte, bool) {
	var encrypted string
	if json.Unmarshal(raw["encrypted_content"], &encrypted) != nil {
		return nil, false
	}
	encrypted = strings.TrimSpace(encrypted)
	if len(encrypted) < minReplayEncryptedLen {
		return nil, false
	}
	out := map[string]any{
		"type":              "reasoning",
		"summary":           []any{},
		"content":           nil,
		"encrypted_content": encrypted,
	}
	data, err := json.Marshal(out)
	return data, err == nil
}

func normalizeAssistantMessageItem(raw map[string]json.RawMessage) ([]byte, bool) {
	var role string
	_ = json.Unmarshal(raw["role"], &role)
	if !strings.EqualFold(strings.TrimSpace(role), "assistant") {
		return nil, false
	}
	var content []map[string]json.RawMessage
	if json.Unmarshal(raw["content"], &content) != nil || len(content) == 0 {
		return nil, false
	}
	parts := make([]map[string]any, 0, len(content))
	for _, part := range content {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		switch strings.TrimSpace(partType) {
		case "output_text":
			var text string
			if json.Unmarshal(part["text"], &text) != nil {
				continue
			}
			parts = append(parts, map[string]any{"type": "output_text", "text": text})
		case "refusal":
			var refusal string
			if json.Unmarshal(part["refusal"], &refusal) != nil {
				continue
			}
			parts = append(parts, map[string]any{"type": "refusal", "refusal": refusal})
		}
	}
	if len(parts) == 0 {
		return nil, false
	}
	data, err := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": parts})
	return data, err == nil
}

func normalizeFunctionCallItem(raw map[string]json.RawMessage) ([]byte, bool) {
	var callID, name, arguments string
	_ = json.Unmarshal(raw["call_id"], &callID)
	_ = json.Unmarshal(raw["name"], &name)
	if json.Unmarshal(raw["arguments"], &arguments) != nil {
		return nil, false
	}
	callID, name = strings.TrimSpace(callID), strings.TrimSpace(name)
	if callID == "" || name == "" {
		return nil, false
	}
	data, err := json.Marshal(map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": arguments})
	return data, err == nil
}

func normalizeCustomToolCallItem(raw map[string]json.RawMessage) ([]byte, bool) {
	var callID, name string
	_ = json.Unmarshal(raw["call_id"], &callID)
	_ = json.Unmarshal(raw["name"], &name)
	callID, name = strings.TrimSpace(callID), strings.TrimSpace(name)
	if callID == "" || name == "" || len(raw["input"]) == 0 {
		return nil, false
	}
	out := map[string]any{"type": "custom_tool_call", "status": "completed", "call_id": callID, "name": name}
	var status string
	if json.Unmarshal(raw["status"], &status) == nil && strings.TrimSpace(status) != "" {
		out["status"] = status
	}
	var input any
	if json.Unmarshal(raw["input"], &input) != nil {
		return nil, false
	}
	out["input"] = input
	data, err := json.Marshal(out)
	return data, err == nil
}

func filterReplayItemsForInput(body []byte, items [][]byte) [][]byte {
	var payload struct {
		Input []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Input) == 0 {
		return nil
	}
	inputItems := make([]map[string]json.RawMessage, 0, len(payload.Input))
	for _, raw := range payload.Input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) == nil {
			inputItems = append(inputItems, item)
		}
	}
	lastAssistant, hasLastAssistant := lastAssistantMessage(inputItems)
	cachedAssistant, hasCachedAssistant := replayAssistantMessage(items)
	assistantMatches := hasLastAssistant && hasCachedAssistant && assistantContentEqual(lastAssistant, cachedAssistant)
	if hasLastAssistant && hasCachedAssistant && !assistantMatches {
		return nil
	}
	existingCalls := map[string]bool{}
	existingOutputs := map[string]bool{}
	existingEncrypted := map[string]bool{}
	for _, item := range inputItems {
		var typeName string
		_ = json.Unmarshal(item["type"], &typeName)
		typeName = strings.TrimSpace(typeName)
		switch typeName {
		case "reasoning":
			var enc string
			_ = json.Unmarshal(item["encrypted_content"], &enc)
			if enc != "" {
				existingEncrypted[enc] = true
			}
		case "function_call_output", "custom_tool_call_output":
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			if callID != "" {
				existingOutputs[callID] = true
			}
		case "function_call", "custom_tool_call":
			var callID, name string
			_ = json.Unmarshal(item["call_id"], &callID)
			_ = json.Unmarshal(item["name"], &name)
			if callID != "" {
				existingCalls[callID+"\x00"+name] = true
			}
		}
	}
	filtered := make([][]byte, 0, len(items))
	for _, item := range items {
		var typed struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
			CallID           string `json:"call_id"`
			Name             string `json:"name"`
		}
		if json.Unmarshal(item, &typed) != nil {
			continue
		}
		switch strings.TrimSpace(typed.Type) {
		case "reasoning":
			if existingEncrypted[typed.EncryptedContent] {
				continue
			}
		case "message":
			if assistantMatches {
				continue
			}
		case "function_call", "custom_tool_call":
			key := typed.CallID + "\x00" + typed.Name
			if typed.CallID == "" || existingCalls[key] {
				continue
			}
			if !existingOutputs[typed.CallID] {
				continue
			}
			existingCalls[key] = true
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func lastAssistantMessage(items []map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		var typeName, role string
		_ = json.Unmarshal(item["type"], &typeName)
		_ = json.Unmarshal(item["role"], &role)
		typeName = strings.TrimSpace(typeName)
		if (typeName != "" && typeName != "message") || !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}
		return item, true
	}
	return nil, false
}

func replayAssistantMessage(items [][]byte) (map[string]json.RawMessage, bool) {
	for _, item := range items {
		var raw map[string]json.RawMessage
		if json.Unmarshal(item, &raw) != nil {
			continue
		}
		var typeName, role string
		_ = json.Unmarshal(raw["type"], &typeName)
		_ = json.Unmarshal(raw["role"], &role)
		if strings.TrimSpace(typeName) == "message" && strings.EqualFold(strings.TrimSpace(role), "assistant") {
			return raw, true
		}
	}
	return nil, false
}

func assistantContentEqual(left, right map[string]json.RawMessage) bool {
	leftParts, leftOK := assistantParts(left["content"])
	rightParts, rightOK := assistantParts(right["content"])
	if !leftOK || !rightOK || len(leftParts) != len(rightParts) {
		return false
	}
	for i := range leftParts {
		if leftParts[i] != rightParts[i] {
			return false
		}
	}
	return true
}

type assistantPart struct {
	partType string
	value    string
}

func assistantParts(raw json.RawMessage) ([]assistantPart, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return []assistantPart{{partType: "output_text", value: asString}}, true
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, false
	}
	result := make([]assistantPart, 0, len(parts))
	for _, part := range parts {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		switch strings.TrimSpace(partType) {
		case "output_text":
			var text string
			if json.Unmarshal(part["text"], &text) != nil {
				return nil, false
			}
			result = append(result, assistantPart{partType: partType, value: text})
		case "refusal":
			var refusal string
			if json.Unmarshal(part["refusal"], &refusal) != nil {
				return nil, false
			}
			result = append(result, assistantPart{partType: partType, value: refusal})
		default:
			return nil, false
		}
	}
	return result, len(result) > 0
}

func insertReplayItems(body []byte, replayItems [][]byte) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body, false
	}
	inputRaw, ok := payload["input"]
	if !ok {
		return body, false
	}
	var input []json.RawMessage
	if json.Unmarshal(inputRaw, &input) != nil {
		return body, false
	}
	insertAt := replayInsertIndex(input, replayItems)
	next := make([]json.RawMessage, 0, len(input)+len(replayItems))
	for i, item := range input {
		if i == insertAt {
			for _, replay := range replayItems {
				next = append(next, json.RawMessage(replay))
			}
		}
		next = append(next, item)
	}
	if insertAt == len(input) {
		for _, replay := range replayItems {
			next = append(next, json.RawMessage(replay))
		}
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return body, false
	}
	payload["input"] = encoded
	updated, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return updated, true
}

func replayInsertIndex(input []json.RawMessage, replayItems [][]byte) int {
	replayCallIDs := map[string]bool{}
	for _, item := range replayItems {
		var typed struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(item, &typed) != nil {
			continue
		}
		if typed.Type == "function_call" || typed.Type == "custom_tool_call" {
			if id := strings.TrimSpace(typed.CallID); id != "" {
				replayCallIDs[id] = true
			}
		}
	}
	if len(replayCallIDs) > 0 {
		for index, raw := range input {
			var typed struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(raw, &typed) != nil {
				continue
			}
			if typed.Type != "function_call_output" && typed.Type != "custom_tool_call_output" {
				continue
			}
			callID := strings.TrimSpace(typed.CallID)
			if callID == "" || replayCallIDs[callID] {
				return index
			}
		}
	}
	for index := len(input) - 1; index >= 0; index-- {
		var typed struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(input[index], &typed) != nil {
			continue
		}
		typeName := strings.TrimSpace(typed.Type)
		if (typeName == "" || typeName == "message") && strings.EqualFold(strings.TrimSpace(typed.Role), "assistant") {
			return index + 1
		}
	}
	return len(input)
}

// CaptureBody 包装上游响应，在流结束时抽取并写入回放缓存。
func (r *ReasoningReplay) CaptureBody(body io.ReadCloser, model, sessionKey string, streaming, compact bool) io.ReadCloser {
	if !r.Enabled() || body == nil || strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(model) == "" {
		return body
	}
	return &replayCaptureBody{
		inner:     body,
		replay:    r,
		model:     model,
		session:   sessionKey,
		streaming: streaming,
		compact:   compact,
	}
}

type replayCaptureBody struct {
	inner     io.ReadCloser
	replay    *ReasoningReplay
	model     string
	session   string
	streaming bool
	compact   bool
	buf       bytes.Buffer
	truncated bool
	done      bool
}

func (b *replayCaptureBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if n > 0 && !b.truncated {
		if b.buf.Len()+n > maxReplayCaptureBytes {
			b.truncated = true
			b.buf.Reset()
		} else {
			_, _ = b.buf.Write(p[:n])
		}
	}
	return n, err
}

func (b *replayCaptureBody) Close() error {
	closeErr := b.inner.Close()
	if b.done {
		return closeErr
	}
	b.done = true
	if b.truncated || b.buf.Len() == 0 {
		return closeErr
	}
	payload := b.buf.Bytes()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if b.compact {
		b.replay.Clear(ctx, b.model, b.session)
		return closeErr
	}
	if b.streaming {
		if completed := extractCompletedPayloadFromSSE(payload); len(completed) > 0 {
			b.replay.StoreFromCompleted(ctx, b.model, b.session, completed)
		}
		return closeErr
	}
	b.replay.StoreFromCompleted(ctx, b.model, b.session, payload)
	return closeErr
}

func extractCompletedPayloadFromSSE(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	var last []byte
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(value, []byte("[DONE]")) || len(value) == 0 {
			continue
		}
		var typed struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Output   json.RawMessage `json:"output"`
		}
		if json.Unmarshal(value, &typed) != nil {
			continue
		}
		switch strings.TrimSpace(typed.Type) {
		case "response.completed", "response.done":
			if len(typed.Response) > 0 {
				last = append([]byte(nil), value...)
			} else if len(typed.Output) > 0 {
				last = append([]byte(nil), value...)
			}
		default:
			if len(typed.Output) > 0 && typed.Type == "" {
				last = append([]byte(nil), value...)
			}
		}
	}
	return last
}

// StoreFromCompletedPayload 供已缓冲完整 JSON 的路径直接调用。
func (r *ReasoningReplay) StoreFromCompletedPayload(ctx context.Context, model, sessionKey string, payload []byte, compact bool) {
	if compact {
		r.Clear(ctx, model, sessionKey)
		return
	}
	r.StoreFromCompleted(ctx, model, sessionKey, payload)
}
