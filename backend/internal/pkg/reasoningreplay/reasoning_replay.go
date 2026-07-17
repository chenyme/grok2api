package reasoningreplay

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	minReplayEncryptedDecodedLen = 50
	minReplayEncryptedEntropy    = 0.85
	maxReplayEncryptedLen        = 8 << 20
	maxReplayCaptureBytes        = 8 << 20
	defaultReasoningReplayTTL    = time.Hour
)

// Config 控制服务端推理回放缓存。
type Config struct {
	Enabled bool
	TTL     time.Duration
}

// ReasoningReplay 封装存储与 body 注入/抽取。
type ReasoningReplay struct {
	store  repository.ReasoningReplayRepository
	cfg    atomic.Pointer[Config]
	logger *slog.Logger
	now    func() time.Time
}

func New(store repository.ReasoningReplayRepository, cfg Config, logger *slog.Logger) *ReasoningReplay {
	if logger == nil {
		logger = slog.Default()
	}
	replay := &ReasoningReplay{store: store, logger: logger, now: time.Now}
	replay.UpdateConfig(cfg)
	return replay
}

func (r *ReasoningReplay) UpdateConfig(cfg Config) {
	if r == nil {
		return
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultReasoningReplayTTL
	}
	r.cfg.Store(&cfg)
}

func (r *ReasoningReplay) Enabled() bool {
	if r == nil || r.store == nil {
		return false
	}
	cfg := r.cfg.Load()
	return cfg != nil && cfg.Enabled
}

// Apply 将缓存的上一轮 output items 注入 Responses body.input。
func (r *ReasoningReplay) Apply(ctx context.Context, model, sessionKey string, body []byte) []byte {
	if r == nil || r.store == nil || strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(model) == "" || len(body) == 0 {
		return body
	}
	cfg := r.cfg.Load()
	if cfg == nil || !cfg.Enabled {
		return body
	}
	if previousResponseIDPresent(body) {
		r.logger.Debug("reasoning_replay_miss", "reason", "previous_response_id", "model", model)
		return body
	}
	items, ok, err := r.store.Get(ctx, model, sessionKey, r.now().UTC(), cfg.TTL)
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
	if r == nil || r.store == nil || strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(model) == "" {
		return
	}
	cfg := r.cfg.Load()
	if cfg == nil || !cfg.Enabled {
		return
	}
	items, complete := extractReplayItemsFromPayload(payload)
	if !complete {
		r.logger.Debug("reasoning_replay_store_skipped", "model", model, "reason", "incomplete_payload")
		return
	}
	normalized, ok := normalizeReplayItems(items)
	if !ok {
		if err := r.store.Delete(ctx, model, sessionKey); err != nil {
			r.logger.Warn("reasoning_replay_delete_failed", "model", model, "reason", "no_anchor", "error", err)
		} else {
			r.logger.Debug("reasoning_replay_delete", "model", model, "reason", "no_anchor")
		}
		return
	}
	expiresAt := r.now().UTC().Add(cfg.TTL)
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
