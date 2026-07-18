package gateway

import (
	"encoding/json"
	"strings"
)

// isResponsesCompactionRequest detects Codex remote compaction v2 without
// retaining or logging the request body. The Provider adapter remains the
// authority for validating that the trigger appears exactly once and last.
func isResponsesCompactionRequest(body []byte) bool {
	var payload struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	for _, item := range payload.Input {
		if strings.EqualFold(strings.TrimSpace(item.Type), "compaction_trigger") {
			return true
		}
	}
	return false
}
