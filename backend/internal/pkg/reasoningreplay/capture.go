package reasoningreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

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
	sawEOF    bool
	readErr   error
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
	if err == io.EOF {
		b.sawEOF = true
	} else if err != nil {
		b.readErr = err
	}
	return n, err
}

func (b *replayCaptureBody) Close() error {
	if b.done {
		return nil
	}
	b.done = true
	closeErr := b.inner.Close()
	if b.truncated || b.buf.Len() == 0 {
		return closeErr
	}
	payload := b.buf.Bytes()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if b.compact {
		if b.sawEOF && b.readErr == nil {
			b.replay.Clear(ctx, b.model, b.session)
		}
		return closeErr
	}
	if b.streaming {
		if completed, ok := extractCompletedPayloadFromSSE(payload); ok {
			b.replay.StoreFromCompleted(ctx, b.model, b.session, completed)
		}
		return closeErr
	}
	if !b.sawEOF || b.readErr != nil {
		return closeErr
	}
	b.replay.StoreFromCompleted(ctx, b.model, b.session, payload)
	return closeErr
}

func extractCompletedPayloadFromSSE(data []byte) ([]byte, bool) {
	lines := bytes.Split(data, []byte("\n"))
	var last []byte
	itemsByIndex := map[int][]byte{}
	var fallbackItems [][]byte
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
			Item     json.RawMessage `json:"item"`
			Index    *int            `json:"output_index"`
		}
		if json.Unmarshal(value, &typed) != nil {
			continue
		}
		switch strings.TrimSpace(typed.Type) {
		case "response.output_item.done":
			if len(typed.Item) == 0 || !json.Valid(typed.Item) {
				continue
			}
			item := append([]byte(nil), typed.Item...)
			if typed.Index != nil {
				itemsByIndex[*typed.Index] = item
			} else {
				fallbackItems = append(fallbackItems, item)
			}
		case "response.completed", "response.done":
			if len(typed.Response) > 0 {
				last = patchCompletedOutput(value, itemsByIndex, fallbackItems)
			} else if len(typed.Output) > 0 {
				last = append([]byte(nil), value...)
			}
		default:
			if len(typed.Output) > 0 && typed.Type == "" {
				last = append([]byte(nil), value...)
			}
		}
	}
	return last, len(last) > 0
}

func patchCompletedOutput(event []byte, itemsByIndex map[int][]byte, fallbackItems [][]byte) []byte {
	if len(itemsByIndex) == 0 && len(fallbackItems) == 0 {
		return append([]byte(nil), event...)
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(event, &root) != nil {
		return append([]byte(nil), event...)
	}
	responseRaw, ok := root["response"]
	if !ok {
		return append([]byte(nil), event...)
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(responseRaw, &response) != nil {
		return append([]byte(nil), event...)
	}
	var existing []json.RawMessage
	if raw, exists := response["output"]; exists && json.Unmarshal(raw, &existing) == nil && len(existing) > 0 {
		return append([]byte(nil), event...)
	}
	indexes := make([]int, 0, len(itemsByIndex))
	for index := range itemsByIndex {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	output := make([]json.RawMessage, 0, len(indexes)+len(fallbackItems))
	for _, index := range indexes {
		output = append(output, json.RawMessage(itemsByIndex[index]))
	}
	for _, item := range fallbackItems {
		output = append(output, json.RawMessage(item))
	}
	encodedOutput, err := json.Marshal(output)
	if err != nil {
		return append([]byte(nil), event...)
	}
	response["output"] = encodedOutput
	encodedResponse, err := json.Marshal(response)
	if err != nil {
		return append([]byte(nil), event...)
	}
	root["response"] = encodedResponse
	patched, err := json.Marshal(root)
	if err != nil {
		return append([]byte(nil), event...)
	}
	return patched
}

// StoreFromCompletedPayload 供已缓冲完整 JSON 的路径直接调用。
func (r *ReasoningReplay) StoreFromCompletedPayload(ctx context.Context, model, sessionKey string, payload []byte, compact bool) {
	if compact {
		r.Clear(ctx, model, sessionKey)
		return
	}
	r.StoreFromCompleted(ctx, model, sessionKey, payload)
}
