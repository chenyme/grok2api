package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

var buildGeneratedDeltaEvents = map[string]struct{}{
	"response.output_text.delta":             {},
	"response.reasoning_summary_text.delta":  {},
	"response.reasoning_text.delta":          {},
	"response.refusal.delta":                 {},
	"response.function_call_arguments.delta": {},
	"response.custom_tool_call_input.delta":  {},
}

var buildGeneratedOutputItemTypes = map[string]struct{}{
	"code_interpreter_call": {},
	"custom_tool_call":      {},
	"file_search_call":      {},
	"function_call":         {},
	"image_generation_call": {},
	"mcp_call":              {},
	"mcp_approval_request":  {},
	"mcp_approval_response": {},
	"mcp_list_tools":        {},
	"message":               {},
	"reasoning":             {},
	"shell_call":            {},
	"web_search_call":       {},
}

// semanticIdleReadCloser measures useful generated output rather than raw
// transport bytes, so SSE keepalives and Build control events cannot keep a
// stalled generation alive indefinitely.
//
// The idle clock runs only while Read is waiting on upstream. Downstream
// backpressure (for example ConvertResponseStream blocked on an unbuffered
// io.Pipe write) must not look like an upstream stall; the client write
// deadline covers that case.
type semanticIdleReadCloser struct {
	inner io.ReadCloser
	idle  time.Duration
	timer *time.Timer

	mu         sync.Mutex
	detector   buildSSEActivityDetector
	finished   bool
	timedOut   bool
	readers    int
	remaining  time.Duration
	clockStart time.Time

	closeOnce sync.Once
	closeErr  error
}

func wrapBuildSemanticIdle(body io.ReadCloser, idle time.Duration) io.ReadCloser {
	if body == nil || idle <= 0 {
		return body
	}
	return &semanticIdleReadCloser{inner: body, idle: idle, remaining: idle}
}

func (r *semanticIdleReadCloser) timeout() {
	r.mu.Lock()
	if r.finished || r.readers == 0 {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.timedOut = true
	r.clockStart = time.Time{}
	r.mu.Unlock()
	_ = r.closeInner()
}

func (r *semanticIdleReadCloser) startClockLocked() {
	if r.finished || r.readers == 0 {
		return
	}
	if r.remaining <= 0 {
		r.finished = true
		r.timedOut = true
		return
	}
	r.clockStart = time.Now()
	if r.timer == nil {
		r.timer = time.AfterFunc(r.remaining, r.timeout)
		return
	}
	r.timer.Reset(r.remaining)
}

func (r *semanticIdleReadCloser) stopClockLocked() {
	if r.timer != nil {
		r.timer.Stop()
	}
	if r.clockStart.IsZero() {
		return
	}
	elapsed := time.Since(r.clockStart)
	r.clockStart = time.Time{}
	if r.remaining > elapsed {
		r.remaining -= elapsed
	} else {
		r.remaining = 0
	}
}

func (r *semanticIdleReadCloser) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	if r.timedOut {
		r.mu.Unlock()
		return 0, neterror.ErrUpstreamStreamIdleTimeout
	}
	r.readers++
	if r.readers == 1 {
		r.startClockLocked()
		if r.timedOut {
			r.readers--
			r.mu.Unlock()
			_ = r.closeInner()
			return 0, neterror.ErrUpstreamStreamIdleTimeout
		}
	}
	r.mu.Unlock()

	n, err := r.inner.Read(buffer)

	r.mu.Lock()
	if n > 0 && !r.finished && r.detector.Observe(buffer[:n]) {
		r.remaining = r.idle
		if r.readers > 0 && !r.finished {
			r.clockStart = time.Now()
			if r.timer != nil {
				r.timer.Reset(r.remaining)
			}
		}
	}
	if err != nil && !r.finished {
		r.finished = true
		r.stopClockLocked()
	}
	if r.readers > 0 {
		r.readers--
	}
	if r.readers == 0 && !r.finished {
		r.stopClockLocked()
	}
	timedOut := r.timedOut
	r.mu.Unlock()

	if timedOut {
		return n, neterror.ErrUpstreamStreamIdleTimeout
	}
	return n, err
}

func (r *semanticIdleReadCloser) Close() error {
	r.mu.Lock()
	if !r.finished {
		r.finished = true
		r.stopClockLocked()
	}
	r.mu.Unlock()
	return r.closeInner()
}

func (r *semanticIdleReadCloser) closeInner() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.inner.Close()
	})
	return r.closeErr
}

func (r *semanticIdleReadCloser) TimedOut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timedOut
}

// buildSSEActivityDetector incrementally parses SSE framing without modifying
// the bytes returned to downstream response wrappers.
type buildSSEActivityDetector struct {
	pending    []byte
	eventName  string
	data       []byte
	eventBytes int
	overLimit  bool
	firstLine  bool
}

func (d *buildSSEActivityDetector) Observe(chunk []byte) bool {
	if len(chunk) == 0 {
		return false
	}
	d.pending = append(d.pending, chunk...)
	active := false
	for {
		newline := bytes.IndexByte(d.pending, '\n')
		if newline < 0 {
			break
		}
		line := d.pending[:newline]
		d.pending = d.pending[newline+1:]
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if !d.firstLine {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
			d.firstLine = true
		}
		if len(line) == 0 {
			active = d.finishEvent() || active
			continue
		}
		d.observeLine(line)
	}
	if len(d.pending)+d.eventBytes > maxCompatibleSSEEventBytes {
		d.pending = nil
		d.overLimit = true
	}
	return active
}

func (d *buildSSEActivityDetector) observeLine(line []byte) {
	d.eventBytes += len(line)
	if d.eventBytes > maxCompatibleSSEEventBytes {
		d.overLimit = true
		d.eventName = ""
		d.data = nil
		return
	}
	if d.overLimit || len(line) == 0 || line[0] == ':' {
		return
	}
	field, value, found := bytes.Cut(line, []byte{':'})
	if found && len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(field) {
	case "event":
		d.eventName = string(value)
	case "data":
		if len(d.data) > 0 {
			d.data = append(d.data, '\n')
		}
		d.data = append(d.data, value...)
	}
}

func (d *buildSSEActivityDetector) finishEvent() bool {
	active := false
	if !d.overLimit && len(d.data) > 0 {
		var payload struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Item  struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
		}
		if json.Unmarshal(d.data, &payload) == nil {
			kind := strings.TrimSpace(payload.Type)
			if kind == "" {
				kind = strings.TrimSpace(d.eventName)
			}
			if _, generatedDelta := buildGeneratedDeltaEvents[kind]; generatedDelta {
				active = payload.Delta != ""
			} else if kind == "response.output_item.added" || kind == "response.output_item.done" {
				itemType := strings.TrimSpace(payload.Item.Type)
				_, generatedItem := buildGeneratedOutputItemTypes[itemType]
				active = generatedItem && (strings.TrimSpace(payload.Item.ID) != "" ||
					strings.TrimSpace(payload.Item.CallID) != "" || strings.TrimSpace(payload.Item.Name) != "")
			}
		}
	}
	d.eventName = ""
	d.data = nil
	d.eventBytes = 0
	d.overLimit = false
	return active
}
