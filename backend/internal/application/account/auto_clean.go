package account

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

const autoCleanReauthBatchSize = 100

// UpdateAutoCleanConfig 热更新账号自动清理策略。
func (s *Service) UpdateAutoCleanConfig(value config.AccountsConfig) {
	s.autoCleanMu.Lock()
	defer s.autoCleanMu.Unlock()
	s.autoClean = value
	select {
	case s.autoCleanWake <- struct{}{}:
	default:
	}
}

func (s *Service) autoCleanConfig() config.AccountsConfig {
	s.autoCleanMu.RLock()
	defer s.autoCleanMu.RUnlock()
	return s.autoClean
}

// RunAccountAutoClean 在启用时周期性删除过期的 reauthRequired 账号；默认关闭。
func (s *Service) RunAccountAutoClean(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.autoCleanWake:
		case <-timer.C:
		}
		cfg := s.autoCleanConfig()
		if !cfg.AutoCleanReauthEnabled {
			resetCredentialRefreshTimer(timer, time.Hour)
			continue
		}
		interval := cfg.AutoCleanReauthInterval.Value()
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

func (s *Service) runAutoCleanReauth(ctx context.Context, cfg config.AccountsConfig) error {
	if !cfg.AutoCleanReauthEnabled {
		return nil
	}
	minAge := cfg.AutoCleanReauthMinAge.Value()
	if minAge < time.Minute {
		minAge = time.Minute
	}
	updatedBefore := s.now().Add(-minAge)
	scanned := 0
	deleted := int64(0)
	for {
		ids, err := s.accounts.ListAutoCleanReauthIDs(ctx, updatedBefore, cfg.AutoCleanDisabledEnabled, autoCleanReauthBatchSize)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		scanned += len(ids)
		n, err := s.BatchDelete(ctx, ids)
		if err != nil {
			s.logger.Info("auto_clean_reauth", "deleted", deleted, "scanned", scanned, "error", err.Error())
			return err
		}
		deleted += n
		if len(ids) < autoCleanReauthBatchSize {
			break
		}
	}
	if scanned > 0 || deleted > 0 {
		s.logger.Info("auto_clean_reauth", "deleted", deleted, "scanned", scanned)
	}
	return nil
}

// AutoCleanReauthOnce 立即执行一轮自动清理（供测试使用）。
func (s *Service) AutoCleanReauthOnce(ctx context.Context) (deleted int64, scanned int, err error) {
	cfg := s.autoCleanConfig()
	if !cfg.AutoCleanReauthEnabled {
		return 0, 0, nil
	}
	minAge := cfg.AutoCleanReauthMinAge.Value()
	if minAge < time.Minute {
		minAge = time.Minute
	}
	updatedBefore := s.now().Add(-minAge)
	ids, err := s.accounts.ListAutoCleanReauthIDs(ctx, updatedBefore, cfg.AutoCleanDisabledEnabled, autoCleanReauthBatchSize)
	if err != nil {
		return 0, 0, err
	}
	if len(ids) == 0 {
		return 0, 0, nil
	}
	n, err := s.BatchDelete(ctx, ids)
	return n, len(ids), err
}
