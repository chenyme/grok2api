package provider

import (
	"encoding/json"
	"strings"
)

// IsDefinitiveAccountBlockBody accepts only explicit error code or message signals.
func IsDefinitiveAccountBlockBody(body []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return IsDefinitiveAccountBlockText(string(body))
	}
	values := []string{
		jsonStringField(payload, "code"),
		jsonStringField(payload, "message"),
		jsonStringField(payload, "error"),
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		values = append(values,
			jsonStringField(nested, "code"),
			jsonStringField(nested, "message"),
			jsonStringField(nested, "error"),
		)
	}
	return IsDefinitiveAccountBlockText(strings.Join(values, " "))
}

func IsDefinitiveAccountBlockText(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "blocked-user") || strings.Contains(value, "user is blocked")
}

// IsDPoPProofRequiredBody reports the Console protocol-level DPoP challenge.
// It must not be attributed to an account credential or physical egress node.
func IsDPoPProofRequiredBody(body []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return IsDPoPProofRequiredText(string(body))
	}
	values := []string{
		jsonStringField(payload, "code"),
		jsonStringField(payload, "message"),
		jsonStringField(payload, "error"),
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		values = append(values,
			jsonStringField(nested, "code"),
			jsonStringField(nested, "message"),
			jsonStringField(nested, "error"),
		)
	}
	return IsDPoPProofRequiredText(strings.Join(values, " "))
}

func IsDPoPProofRequiredText(value string) bool {
	normalized := strings.NewReplacer("-", "_", ":", "_", ".", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	return strings.Contains(normalized, "unauthorized_dpop_required") || strings.Contains(normalized, "dpop_proof_required")
}

func jsonStringField(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}
