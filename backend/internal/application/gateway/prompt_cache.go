package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

const buildSessionIdentityVersion = "v3"

type buildSessionIdentity struct {
	// upstreamID is sent as prompt_cache_key and x-grok-conv-id and must remain stable across turns.
	upstreamID string
	// affinityKey controls account stickiness and is isolated by model to avoid cross-model collisions.
	affinityKey string
	// replayKey is derived only from explicit client session signals; soft anchors must not drive encrypted reasoning replay.
	replayKey string
	// soft indicates a fallback identity derived from message content when no explicit session is available.
	soft bool
}

// resolveBuildSessionIdentity derives a stable Grok Build session identity:
// 1. Prefer explicit client session signals, isolated by client key, provider, and model.
// 2. Fall back to system/instructions and the first user message when no explicit signal exists.
// 3. Return an empty identity when no signal exists; never generate a random session ID per request.
func resolveBuildSessionIdentity(clientKeyID uint64, provider accountdomain.Provider, upstreamModel, explicitKey, sessionSeed string, body []byte) buildSessionIdentity {
	// Prefer Claude Code and Codex session signals extracted by the transport layer.
	// body.prompt_cache_key is only a fallback when no stronger header or session signal exists.
	seed := strings.TrimSpace(sessionSeed)
	if seed == "" {
		seed = strings.TrimSpace(explicitKey)
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if clientKeyID == 0 || provider == "" || model == "" {
		return buildSessionIdentity{}
	}
	if seed != "" {
		upstreamSource := fmt.Sprintf("grok2api:build-session:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, seed)
		affinitySource := fmt.Sprintf("grok2api:build-affinity:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, seed)
		replaySource := fmt.Sprintf("grok2api:build-replay:%s:%d:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, seed)
		return buildSessionIdentity{
			upstreamID:  digestUUID(upstreamSource),
			affinityKey: hexDigest(affinitySource),
			replayKey:   hexDigest(replaySource),
		}
	}
	// Fall back to a message-prefix hash to keep account affinity and session IDs stable without client session signals.
	system, firstUser, _ := extractMessageAnchors(body)
	firstUser = truncateAnchor(firstUser, 200)
	system = truncateAnchor(system, 100)
	if firstUser == "" {
		return buildSessionIdentity{}
	}
	upstreamSource := fmt.Sprintf("grok2api:build-soft-session:%s:%d:%s:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, system, firstUser)
	affinitySource := fmt.Sprintf("grok2api:build-soft-affinity:%s:%d:%s:%s:%s:%s", buildSessionIdentityVersion, clientKeyID, provider, model, system, firstUser)
	return buildSessionIdentity{
		upstreamID:  digestUUID(upstreamSource),
		affinityKey: hexDigest(affinitySource),
		soft:        true,
	}
}

func digestUUID(source string) string {
	digest := sha256.Sum256([]byte(source))
	hexID := hex.EncodeToString(digest[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexID[0:8], hexID[8:12], hexID[12:16], hexID[16:20], hexID[20:32])
}

func hexDigest(source string) string {
	digest := sha256.Sum256([]byte(source))
	return hex.EncodeToString(digest[:])
}

func truncateAnchor(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" || maxRunes <= 0 {
		return value
	}
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes])
}

// extractMessageAnchors extracts stable prefix anchors from Chat, Messages, and Responses request bodies.
// It uses only system, the first user message, and an optional first assistant message to avoid hash drift across turns.
func extractMessageAnchors(body []byte) (system, firstUser, firstAssistant string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return "", "", ""
	}
	// Top-level system or instructions fields provide a stable system anchor for OpenAI Responses and Chat.
	if raw, ok := root["instructions"]; ok {
		system = flattenMessageContent(raw)
	}
	if system == "" {
		if raw, ok := root["system"]; ok {
			system = flattenMessageContent(raw)
		}
	}
	if raw, ok := root["messages"]; ok {
		msgSystem, msgUser, msgAssistant := anchorsFromRoleMessages(raw)
		if system == "" {
			system = msgSystem
		}
		firstUser, firstAssistant = msgUser, msgAssistant
		if firstUser != "" {
			return system, firstUser, firstAssistant
		}
	}
	if raw, ok := root["input"]; ok {
		inSystem, inUser, inAssistant := anchorsFromResponsesInput(raw)
		if system == "" {
			system = inSystem
		}
		if firstUser == "" {
			firstUser = inUser
		}
		if firstAssistant == "" {
			firstAssistant = inAssistant
		}
	}
	return system, firstUser, firstAssistant
}

func anchorsFromRoleMessages(raw json.RawMessage) (system, firstUser, firstAssistant string) {
	var messages []map[string]json.RawMessage
	if json.Unmarshal(raw, &messages) != nil {
		return "", "", ""
	}
	for _, msg := range messages {
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		content := flattenMessageContent(msg["content"])
		if content == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system":
			if system == "" {
				system = content
			}
		case "user":
			if firstUser == "" {
				firstUser = content
			}
		case "assistant":
			if firstAssistant == "" {
				firstAssistant = content
			}
		}
		if system != "" && firstUser != "" && firstAssistant != "" {
			break
		}
	}
	return system, firstUser, firstAssistant
}

func anchorsFromResponsesInput(raw json.RawMessage) (system, firstUser, firstAssistant string) {
	// Shorthand form: input is a direct string.
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return "", strings.TrimSpace(asString), ""
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return "", "", ""
	}
	for _, item := range items {
		var typeName, role string
		_ = json.Unmarshal(item["type"], &typeName)
		_ = json.Unmarshal(item["role"], &role)
		typeName = strings.TrimSpace(typeName)
		role = strings.ToLower(strings.TrimSpace(role))
		// Top-level instructions handle the system anchor; this branch extracts messages.
		if typeName != "" && typeName != "message" {
			continue
		}
		content := flattenMessageContent(item["content"])
		if content == "" {
			// Support content objects whose text field is a string.
			var text string
			if json.Unmarshal(item["text"], &text) == nil {
				content = strings.TrimSpace(text)
			}
		}
		if content == "" {
			continue
		}
		switch role {
		case "system", "developer":
			if system == "" {
				system = content
			}
		case "user":
			if firstUser == "" {
				firstUser = content
			}
		case "assistant":
			if firstAssistant == "" {
				firstAssistant = content
			}
		default:
			// Treat role-less plain-text input items as user input.
			if role == "" && firstUser == "" && (typeName == "" || typeName == "message") {
				firstUser = content
			}
		}
		if firstUser != "" && firstAssistant != "" {
			break
		}
	}
	// Use top-level instructions as a system fallback.
	return system, firstUser, firstAssistant
}

func flattenMessageContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return strings.TrimSpace(asString)
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		switch strings.TrimSpace(partType) {
		case "", "text", "input_text", "output_text":
			var text string
			if json.Unmarshal(part["text"], &text) == nil && strings.TrimSpace(text) != "" {
				if builder.Len() > 0 {
					builder.WriteByte('\n')
				}
				builder.WriteString(strings.TrimSpace(text))
			}
		}
	}
	return builder.String()
}
