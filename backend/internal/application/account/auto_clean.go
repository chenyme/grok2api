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
// 仅唤醒调度器重排 timer；不会在唤醒时立刻硬删（等下一次 interval tick）。
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

func autoCleanInterval(cfg AutoCleanConfig) time.Duration {
	if !cfg.Enabled {
		return time.Hour
	}
	interval := cfg.Interval
	if interval < time.Minute {
		return time.Minute
	}
	if interval > time.Hour {
		return time.Hour
	}
	return interval
}

// RunAccountAutoClean 在启用时周期性删除过期的 reauthRequired 账号；默认关闭。
// 硬删除只在 timer 到期时执行：启动首轮与热更新（含首次启用）都只排程，不立刻清库。
func (s *Service) RunAccountAutoClean(ctx context.Context) {
	// NewService / 启动接线可能已向 wake 写入；丢弃以免无意义空转。
	select {
	case <-s.autoCleanWake:
	default:
	}
	timer := time.NewTimer(autoCleanInterval(s.autoCleanConfig()))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.autoCleanWake:
			// 配置变更：只重排下一次扫描时间。
			resetCredentialRefreshTimer(timer, autoCleanInterval(s.autoCleanConfig()))
		case <-timer.C:
			cfg := s.autoCleanConfig()
			if cfg.Enabled {
				if err := s.runAutoCleanReauth(ctx, cfg); err != nil && ctx.Err() == nil {
					s.logger.Warn("account_auto_clean_failed", "error", err)
				}
			}
			resetCredentialRefreshTimer(timer, autoCleanInterval(cfg))
		}
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
		// skipped 含 media 跳过与 delete 条件竞态未删；v1 不拆分指标。
		skipped += candidates - len(ids)
		for _, id := range ids {
			if s.sticky != nil {
				_ = s.sticky.DeleteByAccount(ctx, id)
			}
			s.clearRefreshState(id)
		}
		if nextAfter == 0 || candidates < autoCleanReauthBatchSize {
			break
		}
		afterID = nextAfter
	}
	if deleted > 0 {
		s.invalidateBuildBotFlagCache()
	}
	if scanned > 0 || deleted > 0 || skipped > 0 {
		s.logger.Info("auto_clean_reauth", "deleted", deleted, "scanned", scanned, "skipped", skipped, "min_age", minAge.String(), "include_disabled", cfg.IncludeDisabled)
	}
	return nil
}
