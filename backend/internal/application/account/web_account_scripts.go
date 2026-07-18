package account

import (
	"context"
	"errors"
	"strconv"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

const (
	maxWebAccountScriptAccounts = 1000
	webAccountScriptLockTTL     = 5 * time.Minute
)

var ErrWebAccountScriptBusy = errors.New("Grok Web 账号脚本正在执行")

// WebAccountScriptOptions 定义 Grok Web 账号脚本需要执行的步骤。
// EnableNSFW 会隐式启用 SetBirthDate，保证上游年龄前置条件成立。
type WebAccountScriptOptions struct {
	AcceptTerms  bool
	SetBirthDate bool
	EnableNSFW   bool
}

// RunWebAccountScriptsWithProgress 为指定 Web 账号并发执行所选脚本，并按账号报告进度。
func (s *Service) RunWebAccountScriptsWithProgress(ctx context.Context, ids []uint64, options WebAccountScriptOptions, progress BatchProgressObserver) (int, int, error) {
	options, err := normalizeWebAccountScriptOptions(options)
	if err != nil {
		return 0, 0, err
	}
	ids, err = normalizeIDs(ids, maxWebAccountScriptAccounts)
	if err != nil {
		return 0, 0, err
	}
	return s.runWebAccountScriptBatch(ctx, ids, options, progress)
}

// RunAllWebAccountScriptsWithProgress 分页处理完整 Web 号池，避免一次性加载全部账号。
func (s *Service) RunAllWebAccountScriptsWithProgress(ctx context.Context, options WebAccountScriptOptions, progress BatchProgressObserver) (int, int, error) {
	options, err := normalizeWebAccountScriptOptions(options)
	if err != nil {
		return 0, 0, err
	}
	var (
		afterID   uint64
		completed int
		total     int
		succeeded int
		failed    int
		started   bool
	)
	for {
		values, count, err := s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderWeb, afterID, accountTaskBatchSize)
		if err != nil {
			return succeeded, failed, mapRepositoryError(err)
		}
		if !started {
			total = int(count)
			started = true
			if progress != nil {
				if err := progress(0, total); err != nil {
					return succeeded, failed, err
				}
			}
		}
		if len(values) == 0 {
			return succeeded, failed, nil
		}
		remaining := total - completed
		if remaining <= 0 {
			return succeeded, failed, nil
		}
		if len(values) > remaining {
			values = values[:remaining]
		}
		ids := make([]uint64, 0, len(values))
		for _, value := range values {
			ids = append(ids, value.ID)
		}
		batchSucceeded, batchFailed, err := s.runWebAccountScriptBatch(ctx, ids, options, offsetBatchProgress(progress, completed, total))
		succeeded += batchSucceeded
		failed += batchFailed
		if err != nil {
			return succeeded, failed, err
		}
		completed += len(ids)
		afterID = ids[len(ids)-1]
		if completed >= total || len(ids) < accountTaskBatchSize {
			return succeeded, failed, nil
		}
	}
}

func normalizeWebAccountScriptOptions(options WebAccountScriptOptions) (WebAccountScriptOptions, error) {
	if !options.AcceptTerms && !options.SetBirthDate && !options.EnableNSFW {
		return WebAccountScriptOptions{}, invalidInput("至少选择一个账号脚本")
	}
	if options.EnableNSFW {
		options.SetBirthDate = true
	}
	return options, nil
}

func (s *Service) runSingleWebAccountScript(ctx context.Context, id uint64, options WebAccountScriptOptions) error {
	if s.syncPool == nil {
		return s.runWebAccountScript(ctx, id, options)
	}
	return s.syncPool.Do(ctx, func(workCtx context.Context) error {
		return s.runWebAccountScript(workCtx, id, options)
	})
}

func (s *Service) acquireWebAccountScriptLock(ctx context.Context, id uint64) (func(), error) {
	if s.refreshLock == nil {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := "web-account-script:" + strconv.FormatUint(id, 10)
	release, acquired, err := s.refreshLock.Acquire(ctx, key, webAccountScriptLockTTL)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, ErrWebAccountScriptBusy
	}
	if release == nil {
		return func() {}, nil
	}
	return release, nil
}

func (s *Service) runWebAccountScriptBatch(ctx context.Context, ids []uint64, options WebAccountScriptOptions, progress BatchProgressObserver) (int, int, error) {
	return s.runAccountBatch(ctx, "web_account_scripts", ids, s.syncPool, progress, func(workCtx context.Context, id uint64) error {
		err := s.runWebAccountScript(workCtx, id, options)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Warn("web_account_script_failed", "account_id", id, "error", err)
		}
		return err
	})
}
