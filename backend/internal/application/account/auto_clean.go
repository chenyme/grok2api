package account

import (
	"context"
	"time"
)

// AutoCleanConfig 是账号自动清理策略；由 app 层从运行设置映射，不依赖 infra/config。
type AutoCleanConfig struct {
	Enabled         bool
	Interval        time.Duration
	MinAge          time.Duration
	IncludeDisabled bool
}

const autoCleanReauthBatchSize = 100

// UpdateAutoCleanConfig 热更新账号自动清理策略。
func (s *Service) UpdateAutoCleanConfig(value AutoCleanConfig) {
	s.autoCleanMu.Lock()
	defer s.autoCleanMu.Unlock()
	if value.Interval < time.Minute {
		value.Interval = time.Minute
	}
	if value.Interval > time.Hour {
		value.Interval = time.Hour
	}
	if value.MinAge < time.Minute {
		value.MinAge = time.Minute
	}
	if value.MinAge > 30*24*time.Hour {
		value.MinAge = 30 * 24 * time.Hour
	}
	s.autoClean = value
	select {
	case s.autoCleanWake <- struct{}{}:
	default:
	}
}

func (s *Service) autoCleanConfig() AutoCleanConfig {
	s.autoCleanMu.RLock()
	defer s.autoCleanMu.RUnlock()
	return s.autoClean
}

// RunAccountAutoClean 在启用时周期性删除过期的 reauthRequired 账号；默认关闭。
// 首次执行等待一个 interval，避免进程启动立即清库。
func (s *Service) RunAccountAutoClean(ctx context.Context) {
	cfg := s.autoCleanConfig()
	initial := cfg.Interval
	if !cfg.Enabled || initial < time.Minute {
		initial = time.Hour
	}
	timer := time.NewTimer(initial)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.autoCleanWake:
		case <-timer.C:
		}
		cfg = s.autoCleanConfig()
		if !cfg.Enabled {
			resetCredentialRefreshTimer(timer, time.Hour)
			continue
		}
		interval := cfg.Interval
		if interval < time.Minute {
			interval = time.Minute
		}
		if err := s.runAutoCleanReauth(ctx, cfg); err != nil && ctx.Err() == nil {
			s.logger.Warn("account_auto_clean_failed", "error", err)
			if interval < 30*time.Second {
				interval = 30 * time.Second
			}
		}
		resetCredentialRefreshTimer(timer, interval)
	}
}

func (s *Service) runAutoCleanReauth(ctx context.Context, cfg AutoCleanConfig) error {
	if !cfg.Enabled {
		return nil
	}
	minAge := cfg.MinAge
	if minAge < time.Minute {
		minAge = time.Minute
	}
	markedBefore := s.now().Add(-minAge)
	var afterID uint64
	scanned := 0
	deleted := 0
	skipped := 0
	for {
		ids, candidates, nextAfter, err := s.accounts.DeleteAutoCleanReauthBatch(ctx, markedBefore, cfg.IncludeDisabled, afterID, autoCleanReauthBatchSize)
		if err != nil {
			if scanned > 0 || deleted > 0 || skipped > 0 {
				s.logger.Warn("auto_clean_reauth_partial", "deleted", deleted, "scanned", scanned, "skipped", skipped, "error", err)
			}
			return err
		}
		if candidates == 0 {
			break
		}
		scanned += candidates
		deleted += len(ids)
		skipped += candidates - len(ids)
		for _, id := range ids {
			if s.sticky != nil {
				_ = s.sticky.DeleteByAccount(ctx, id)
			}
			s.clearRefreshState(id)
		}
		if len(ids) > 0 {
			s.invalidateBuildBotFlagCache()
		}
		if nextAfter == 0 || candidates < autoCleanReauthBatchSize {
			break
		}
		afterID = nextAfter
	}
	if scanned > 0 || deleted > 0 || skipped > 0 {
		s.logger.Info("auto_clean_reauth", "deleted", deleted, "scanned", scanned, "skipped", skipped)
	}
	return nil
}
