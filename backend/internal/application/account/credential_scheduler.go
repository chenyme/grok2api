package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// CredentialStartupReport 汇总启动阶段的凭据调度与恢复结果。
type CredentialStartupReport struct {
	SchedulesBackfilled int
	CriticalFound       int
	Refreshed           int
	Failed              int
}

// ReconcileCredentialSchedules 为升级前账号补齐持久化调度，不解密凭据，也不访问上游。
func (s *Service) ReconcileCredentialSchedules(ctx context.Context) (int, error) {
	total := 0
	for {
		count, err := s.accounts.BackfillCredentialRefreshSchedules(ctx, s.now(), credentialRefreshBatchSize)
		total += count
		if err != nil || count < credentialRefreshBatchSize {
			return total, err
		}
	}
}

// RecoverCriticalCredentials 在启动预算内仅恢复缺失、已过期、两分钟内到期或失败重试到期的凭据。
func (s *Service) RecoverCriticalCredentials(ctx context.Context, expiresWithin time.Duration, limit int) (CredentialStartupReport, error) {
	report := CredentialStartupReport{}
	backfilled, err := s.ReconcileCredentialSchedules(ctx)
	report.SchedulesBackfilled = backfilled
	if err != nil {
		return report, err
	}
	if limit < 1 || limit > credentialRefreshBatchSize {
		limit = credentialRefreshBatchSize
	}
	now := s.now()
	ids, err := s.accounts.ListCriticalCredentialRefreshIDs(ctx, now, now.Add(expiresWithin), limit)
	if err != nil {
		return report, err
	}
	report.CriticalFound = len(ids)
	if len(ids) == 0 {
		return report, nil
	}
	report.Refreshed, report.Failed, err = s.runAccountBatch(ctx, "credential_startup_recovery", ids, s.refreshPool, nil, func(workCtx context.Context, id uint64) error {
		taskCtx, cancel := context.WithTimeout(workCtx, credentialRefreshTimeout)
		defer cancel()
		credential, getErr := s.accounts.Get(taskCtx, id)
		if getErr != nil {
			return getErr
		}
		if credential.RefreshPermanent && !isRecoverableRefreshErrorCode(credential.LastRefreshErrorCode) {
			if !credential.ExpiresAt.IsZero() && credential.ExpiresAt.After(s.now()) {
				return nil
			}
			return s.MarkReauthRequired(taskCtx, id, permanentRefreshExpiredReason)
		}
		// 临界凭据不受进程内强制刷新节流影响；分布式账号锁和旋转 Token 比对仍避免重复 OAuth。
		_, refreshErr := s.ensureCredential(taskCtx, credential, true, true, false)
		return refreshErr
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return report, err
	}
	return report, err
}

// WakeCredentialRefresh 合并调度唤醒；导入、手动刷新和失败退避更新不会阻塞调用方。
func (s *Service) WakeCredentialRefresh() {
	select {
	case s.credentialRefreshWake <- struct{}{}:
	default:
	}
}

// RunCredentialRefresh 使用单个 Timer 和数据库到期索引驱动刷新，内存占用与账号总数无关。
func (s *Service) RunCredentialRefresh(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.credentialRefreshWake:
		case <-timer.C:
		}
		runFailed := false
		if err := s.refreshDueCredentials(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("credential_refresh_scheduler_failed", "error", err)
			runFailed = true
		}
		delay, err := s.nextCredentialRefreshDelay(ctx)
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("credential_refresh_schedule_read_failed", "error", err)
			delay = credentialRefreshSafetyPoll
		}
		if runFailed && delay < 30*time.Second {
			delay = 30 * time.Second
		}
		resetCredentialRefreshTimer(timer, delay)
	}
}

func (s *Service) refreshDueCredentials(ctx context.Context) error {
	if _, err := s.ReconcileCredentialSchedules(ctx); err != nil {
		return err
	}
	for {
		ids, err := s.accounts.ListDueCredentialRefreshIDs(ctx, s.now(), credentialRefreshBatchSize)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		_, failed, batchErr := s.runAccountBatch(ctx, "credential_auto_refresh", ids, s.refreshPool, nil, func(workCtx context.Context, id uint64) error {
			taskCtx, cancel := context.WithTimeout(workCtx, credentialRefreshTimeout)
			defer cancel()
			credential, err := s.accounts.Get(taskCtx, id)
			if err != nil {
				return err
			}
			if !credential.Enabled || credential.AuthStatus != accountdomain.AuthStatusActive || s.providers == nil || !s.providers.SupportsCredentialRefresh(credential.Provider) || credential.EncryptedRefreshToken == "" {
				return nil
			}
			if credential.RefreshPermanent && !isRecoverableRefreshErrorCode(credential.LastRefreshErrorCode) {
				if !credential.ExpiresAt.IsZero() && credential.ExpiresAt.After(s.now()) {
					return nil
				}
				return s.MarkReauthRequired(taskCtx, id, permanentRefreshExpiredReason)
			}
			if credential.RefreshDueAt != nil && credential.RefreshDueAt.After(s.now()) {
				return nil
			}
			_, err = s.ensureCredential(taskCtx, credential, true, false, true)
			return err
		})
		if batchErr != nil {
			return fmt.Errorf("自动刷新批次执行失败: %w", batchErr)
		}
		if failed > 0 {
			return fmt.Errorf("自动刷新批次失败 %d/%d", failed, len(ids))
		}
		if len(ids) < credentialRefreshBatchSize {
			return nil
		}
	}
}

func (s *Service) nextCredentialRefreshDelay(ctx context.Context) (time.Duration, error) {
	next, err := s.accounts.NextCredentialRefreshDueAt(ctx)
	if err != nil {
		return 0, err
	}
	delay := credentialRefreshSafetyPoll
	if next != nil {
		until := next.Sub(s.now())
		if until < delay {
			delay = until
		}
	}
	if delay < 100*time.Millisecond {
		delay = 100 * time.Millisecond
	}
	return delay, nil
}

func resetCredentialRefreshTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}
