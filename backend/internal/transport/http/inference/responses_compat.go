package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// responsesCompatState fills fields Grok CLI serde treats as required.
type responsesCompatState struct {
	responseID string
	createdAt  int64
	itemSeq    int
	pending    []byte
}

func rewriteResponsesStreamChunk(chunk []byte, state *responsesCompatState) []byte {
	if state == nil {
		return chunk
	}
	state.pending = append(state.pending, chunk...)
	var out []byte
	for {
		index := bytes.IndexByte(state.pending, '\n')
		if index < 0 {
			break
		}
		line := state.pending[:index+1]
		state.pending = state.pending[index+1:]
		out = append(out, rewriteResponsesDataLine(line, state)...)
	}
	return out
}

func (s *responsesCompatState) ensureID() string {
	if s.responseID == "" {
		s.responseID = "resp_abort"
	}
	if s.createdAt == 0 {
		s.createdAt = time.Now().Unix()
	}
	return s.responseID
}

func (s *responsesCompatState) rememberFromMeta(meta responseMetadata) {
	if id := strings.TrimSpace(meta.ResponseID); id != "" {
		s.responseID = id
	}
	s.ensureID()
}

func rewriteResponsesDataLine(line []byte, state *responsesCompatState) []byte {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return line
	}
	changed := sanitizeResponsesEvent(event, state)
	if !changed {
		return line
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return line
	}
	newline := ""
	if bytes.HasSuffix(line, []byte("\n")) {
		newline = "\n"
	}
	return []byte("data: " + string(encoded) + newline)
}

func sanitizeResponsesEvent(event map[string]any, state *responsesCompatState) bool {
	changed := false
	if resp, ok := event["response"].(map[string]any); ok {
		if id := strings.TrimSpace(stringAny(resp["id"])); id != "" {
			state.responseID = id
		} else {
			resp["id"] = state.ensureID()
			changed = true
		}
		if resp["created_at"] == nil {
			_ = state.ensureID()
			resp["created_at"] = state.createdAt
			changed = true
		} else if ts, ok := asInt64(resp["created_at"]); ok && ts > 0 {
			state.createdAt = ts
		}
		if resp["object"] == nil {
			resp["object"] = "response"
			changed = true
		}
		if resp["output"] == nil {
			resp["output"] = []any{}
			changed = true
		}
		if errObj, ok := resp["error"].(map[string]any); ok && strings.TrimSpace(stringAny(errObj["id"])) == "" {
			errObj["id"] = "err_" + strings.TrimPrefix(state.ensureID(), "resp_")
			changed = true
		}
		event["response"] = resp
	}
	if item, ok := event["item"].(map[string]any); ok {
		if strings.TrimSpace(stringAny(item["id"])) == "" {
			state.itemSeq++
			item["id"] = fmt.Sprintf("item_%d", state.itemSeq)
			event["item"] = item
			changed = true
		}
	}
	typ := stringAny(event["type"])
	if strings.HasPrefix(typ, "response.") && (event["id"] == nil || strings.TrimSpace(stringAny(event["id"])) == "") {
		switch typ {
		case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed", "error":
			event["id"] = state.ensureID()
			changed = true
		}
	}
	return changed
}

func stringAny(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func asInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		n, err := typed.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}
