package cli

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const (
	gatewayCompactionPrefix      = "g2a_compact_v1."
	gatewayCompactionVersion     = 1
	maxGatewayCompactionSummary  = 8 << 20
	minGatewayCompactionRunes    = 500
	gatewayCompactionMaxAttempts = 3
)

// Generated from xai-org/grok-build's full_replace_summary_prompt.txt using
// build_summary_prompt(None), so the optional {user_context_section} slot is
// empty. The Grok Build 0.2.106 source confirms that this text is appended as
// the final user item for every compaction attempt.
//
//go:embed responses_compaction_prompt.txt
var gatewayCompactionPrompt string

type gatewayCompactionEnvelope struct {
	Version int    `json:"version"`
	Session string `json:"session"`
	Summary string `json:"summary"`
}

type gatewayCompactionCodec struct {
	cipher *security.Cipher
}

func newGatewayCompactionCodec(cipher *security.Cipher) *gatewayCompactionCodec {
	if cipher == nil {
		return nil
	}
	return &gatewayCompactionCodec{cipher: cipher}
}

func (c *gatewayCompactionCodec) encode(session, summary string) (string, error) {
	if c == nil || c.cipher == nil {
		return "", fmt.Errorf("compaction codec unavailable")
	}
	if summary == "" || len(summary) > maxGatewayCompactionSummary {
		return "", fmt.Errorf("compaction summary size is invalid")
	}
	data, err := json.Marshal(gatewayCompactionEnvelope{Version: gatewayCompactionVersion, Session: session, Summary: summary})
	if err != nil {
		return "", err
	}
	encrypted, err := c.cipher.Encrypt(string(data))
	if err != nil {
		return "", err
	}
	return gatewayCompactionPrefix + encrypted, nil
}

func (c *gatewayCompactionCodec) decode(session, blob string) (string, bool, error) {
	if !strings.HasPrefix(blob, gatewayCompactionPrefix) {
		return "", false, nil
	}
	if c == nil || c.cipher == nil {
		return "", true, fmt.Errorf("compaction codec unavailable")
	}
	plain, err := c.cipher.Decrypt(strings.TrimPrefix(blob, gatewayCompactionPrefix))
	if err != nil {
		return "", true, fmt.Errorf("decode gateway compaction blob: %w", err)
	}
	var envelope gatewayCompactionEnvelope
	if err := json.Unmarshal([]byte(plain), &envelope); err != nil {
		return "", true, fmt.Errorf("decode gateway compaction payload: %w", err)
	}
	if envelope.Version != gatewayCompactionVersion || envelope.Session != session || envelope.Summary == "" || len(envelope.Summary) > maxGatewayCompactionSummary {
		return "", true, fmt.Errorf("gateway compaction payload does not match this session")
	}
	return envelope.Summary, true, nil
}

// expandGatewayCompactionHistory restores gateway-owned remote-v2 state to a
// portable developer message. Foreign OpenAI/Claude/Gemini blobs are never
// forwarded to Grok Build: its decoder cannot decrypt those provider states.
func expandGatewayCompactionHistory(body []byte, codec *gatewayCompactionCodec, session string) ([]byte, int, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, 0, nil // normalizeResponsesRequest owns the public JSON error.
	}
	items, ok := payload["input"].([]any)
	if !ok {
		return body, 0, nil
	}
	foreign := 0
	changed := false
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || stringField(item, "type") != "compaction" {
			continue
		}
		blob, _ := item["encrypted_content"].(string)
		summary, owned, err := codec.decode(session, blob)
		if err != nil {
			foreign++
			items[index] = compatibilityBoundaryMessage("A prior compacted context could not be decoded by this gateway instance. Continue from the retained conversation messages.")
			changed = true
			continue
		}
		if owned {
			items[index] = gatewayCompactionSummaryMessage(summary)
		} else {
			foreign++
			items[index] = compatibilityBoundaryMessage("A compacted context created by another provider cannot be decoded by Grok Build. Continue from the retained conversation messages.")
		}
		changed = true
	}
	if !changed {
		return body, 0, nil
	}
	payload["input"] = items
	encoded, err := json.Marshal(payload)
	return encoded, foreign, err
}

// prepareGatewayCompactionSample mirrors Grok Build 0.2.106 full-replace
// sampling: normal /responses SSE, instructions=null, tools retained with
// tool_choice=auto, concise reasoning summary, and the canonical final user prompt.
func prepareGatewayCompactionSample(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	items, ok := payload["input"].([]any)
	if !ok {
		return nil, &responsesRequestError{Message: "compaction 请求的 input 必须是数组", Param: "input", Code: "invalid_parameter"}
	}
	items = append(items, map[string]any{
		"type": "message", "role": "user", "content": gatewayCompactionPrompt,
	})
	payload["input"] = items
	payload["instructions"] = nil
	payload["stream"] = true
	payload["store"] = false
	payload["temperature"] = 1.0
	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		// 0.2.105 起默认使用 auto；部分部署会拒绝 tools + tool_choice=none。
		payload["tool_choice"] = "auto"
	} else {
		delete(payload, "tool_choice")
	}
	payload["reasoning"] = map[string]any{"summary": "concise"}
	for _, field := range []string{
		"previous_response_id", "text", "response_format", "max_output_tokens", "max_completion_tokens",
	} {
		delete(payload, field)
	}
	return json.Marshal(payload)
}

func cleanGatewayCompactionSummary(raw string) string {
	result := strings.TrimSpace(raw)
	for {
		start := strings.Index(result, "<analysis>")
		if start < 0 {
			break
		}
		summaryStart := strings.Index(result, "<summary>")
		leading := summaryStart < 0 && strings.TrimSpace(result[:start]) == ""
		if summaryStart >= 0 {
			leading = start < summaryStart || strings.TrimSpace(result[summaryStart+len("<summary>"):start]) == ""
		}
		if !leading {
			break
		}
		endRel := strings.Index(result[start:], "</analysis>")
		if endRel < 0 {
			if nextSummary := strings.Index(result[start:], "<summary>"); nextSummary >= 0 {
				result = result[:start] + result[start+nextSummary:]
			} else {
				result = result[:start]
			}
			break
		}
		end := start + endRel + len("</analysis>")
		result = result[:start] + result[end:]
	}
	if start := strings.Index(result, "<summary>"); start >= 0 {
		if end := strings.LastIndex(result, "</summary>"); end > start {
			before := result[:start]
			inner := stripLeadingGatewayCompactionScratchpad(result[start+len("<summary>") : end])
			after := result[end+len("</summary>"):]
			result = before + "Summary:\n" + inner + after
		}
	}
	result = neutralizeGatewayCompactionTags(result)
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(result)
}

// stripLeadingGatewayCompactionScratchpad mirrors grok-build's summary
// cleaner for the common malformed shape where the model emits an untagged
// markdown analysis followed by an orphan </analysis> inside <summary>.
// Numbered summaries are left intact even when they quote that token later.
func stripLeadingGatewayCompactionScratchpad(inner string) string {
	result := strings.TrimSpace(inner)
	lead := strings.TrimLeft(result, "#*-> \t")
	startsWithNumber := len(lead) > 0 && lead[0] >= '0' && lead[0] <= '9'
	if !startsWithNumber {
		if end := strings.LastIndex(result, "</analysis>"); end >= 0 {
			result = strings.TrimSpace(result[end+len("</analysis>"):])
		}
	}
	if strings.HasPrefix(result, "<summary>") {
		result = strings.TrimSpace(strings.TrimPrefix(result, "<summary>"))
	}
	return result
}

func neutralizeGatewayCompactionTags(text string) string {
	for _, tag := range []string{"</summary>", "<summary>", "</analysis>", "<analysis>", "</summary_request>", "<summary_request>"} {
		text = strings.ReplaceAll(text, tag, "<\u200b"+strings.TrimPrefix(tag, "<"))
	}
	return text
}

func isDegenerateGatewayCompactionSummary(summary string) bool {
	cleaned := cleanGatewayCompactionSummary(summary)
	return cleaned == "" || utf8.RuneCountInString(cleaned) < minGatewayCompactionRunes
}

func gatewayCompactionContinuation(raw string) string {
	return "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\n" + cleanGatewayCompactionSummary(raw)
}

// Grok Build rebuilds full-replace history with a synthetic user_meta item.
// Responses does not expose SyntheticReason, so a normal user input item is
// the closest wire-level representation of that carrier.
func gatewayCompactionSummaryMessage(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}
