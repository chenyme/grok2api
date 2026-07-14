package gateway

import (
	"io"
	"log/slog"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// generationTiming 只记录阶段耗时和有限枚举，不保存请求体、凭据或会话键。
type generationTiming struct {
	mu             sync.Mutex
	started        time.Time
	route          string
	provider       accountdomain.Provider
	selectionWait  time.Duration
	credentialWait time.Duration
	upstreamWait   time.Duration
	firstHeaders   time.Duration
	firstBody      time.Duration
	attempts       int
	finished       bool
}

func newGenerationTiming(route string, provider accountdomain.Provider) *generationTiming {
	return &generationTiming{started: time.Now(), route: route, provider: provider}
}

func (t *generationTiming) markSelection(duration time.Duration) {
	t.mu.Lock()
	t.selectionWait += duration
	t.mu.Unlock()
}

func (t *generationTiming) markCredential(duration time.Duration) {
	t.mu.Lock()
	t.credentialWait += duration
	t.mu.Unlock()
}

func (t *generationTiming) markUpstream(duration time.Duration) {
	t.mu.Lock()
	t.attempts++
	t.upstreamWait += duration
	if t.firstHeaders == 0 {
		t.firstHeaders = time.Since(t.started)
	}
	t.mu.Unlock()
}

func (t *generationTiming) markFirstBody() {
	t.mu.Lock()
	if t.firstBody == 0 {
		t.firstBody = time.Since(t.started)
	}
	t.mu.Unlock()
}

func (t *generationTiming) finish(logger *slog.Logger, outcome string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	total := time.Since(t.started)
	retries := max(0, t.attempts-1)
	fields := []any{
		"route", t.route, "provider", t.provider, "outcome", outcome, "total_ms", total.Milliseconds(),
		"selection_wait_ms", t.selectionWait.Milliseconds(), "credential_wait_ms", t.credentialWait.Milliseconds(),
		"upstream_wait_ms", t.upstreamWait.Milliseconds(), "first_headers_ms", t.firstHeaders.Milliseconds(),
		"first_body_ms", t.firstBody.Milliseconds(), "attempts", t.attempts, "retries", retries,
	}
	t.mu.Unlock()
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("generation_timing", fields...)
}

type firstByteReadCloser struct {
	io.ReadCloser
	once sync.Once
	mark func()
}

func (r *firstByteReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	if n > 0 && r.mark != nil {
		r.once.Do(r.mark)
	}
	return n, err
}
