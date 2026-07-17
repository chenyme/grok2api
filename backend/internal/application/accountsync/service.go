package accountsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"golang.org/x/sync/singleflight"
)

const (
	defaultWorkerCount = 25
	operationTimeout   = 2 * time.Minute
)

type billingSynchronizer interface {
	HasBillingSnapshot(ctx context.Context, accountID uint64) (bool, error)
	RefreshBilling(ctx context.Context, accountID uint64) (accountdomain.Billing, error)
}

type modelSynchronizer interface {
	HasSuccessfulAccountSync(ctx context.Context, accountID uint64) (bool, error)
	SyncAccount(ctx context.Context, accountID uint64) (int, error)
}

type accountReader interface {
	Get(ctx context.Context, id uint64) (accountapp.View, error)
}

type providerPolicy interface {
	ProviderDefinition(value accountdomain.Provider) (provider.Definition, bool)
}

type quotaSynchronizer interface {
	HasQuotaWindows(ctx context.Context, accountID uint64) (bool, error)
	RefreshQuota(ctx context.Context, accountID uint64) ([]accountdomain.QuotaWindow, error)
}

// Service 对新接入账号执行一次性额度与模型补齐，并限制批量同步并发。
type Service struct {
	logger   *slog.Logger
	accounts accountReader
	billing  billingSynchronizer
	quota    quotaSynchronizer
	models   modelSynchronizer
	syncs    singleflight.Group
	workers  atomic.Int64
	bulkPool *batch.Pool
}

func NewService(logger *slog.Logger, accounts accountReader, billing billingSynchronizer, quota quotaSynchronizer, models modelSynchronizer) *Service {
	service := &Service{logger: logger, accounts: accounts, billing: billing, quota: quota, models: models, bulkPool: batch.NewPool(defaultWorkerCount)}
	service.workers.Store(defaultWorkerCount)
	return service
}

func (s *Service) SetBulkPool(pool *batch.Pool) {
	if pool != nil {
		s.bulkPool = pool
	}
}

func (s *Service) UpdateConcurrency(value int) {
	if value < 1 {
		value = defaultWorkerCount
	}
	s.workers.Store(int64(value))
	s.bulkPool.UpdateLimit(value)
}

// Result 汇总本次初始同步成功与失败的账号数。
type Result struct {
	Succeeded int
	Failed    int
}

// Sync 等待本次涉及的账号完成额度与模型补齐；已同步数据会跳过，同账号并发请求会合并。
func (s *Service) Sync(ctx context.Context, accountIDs ...uint64) Result {
	input := make(chan uint64, len(accountIDs))
	for _, accountID := range accountIDs {
		input <- accountID
	}
	close(input)
	return s.SyncStream(ctx, input)
}

// SyncStream 以固定 Worker 数消费持续到达的账号，使导入与上游同步形成有界流水线。
func (s *Service) SyncStream(ctx context.Context, accountIDs <-chan uint64) Result {
	return s.syncStream(ctx, accountIDs, nil)
}

// SyncStreamObserved 在每个去重账号完成初始同步后报告进度。
func (s *Service) SyncStreamObserved(ctx context.Context, accountIDs <-chan uint64, observer func(completed, total int)) Result {
	return s.syncStream(ctx, accountIDs, observer)
}

func (s *Service) syncStream(ctx context.Context, accountIDs <-chan uint64, observer func(completed, total int)) Result {
	jobs := make(chan uint64)
	var workers sync.WaitGroup
	var succeeded atomic.Int64
	var failed atomic.Int64
	var total atomic.Int64
	var progressMu sync.Mutex
	completed := 0
	count := max(1, int(s.workers.Load()))
	workers.Add(count)
	for range count {
		go func() {
			defer workers.Done()
			for accountID := range jobs {
				err := s.bulkPool.Do(ctx, func(workCtx context.Context) error {
					_, syncErr, _ := s.syncs.Do(strconv.FormatUint(accountID, 10), func() (any, error) {
						return nil, s.syncAccount(workCtx, accountID)
					})
					return syncErr
				})
				if err != nil {
					var panicErr *batch.PanicError
					if errors.As(err, &panicErr) {
						s.logger.Error("account_initial_sync_panicked", "account_id", accountID, "error", panicErr, "stack", string(panicErr.Stack))
					}
					failed.Add(1)
				} else {
					succeeded.Add(1)
				}
				if observer != nil {
					progressMu.Lock()
					completed++
					observer(completed, int(total.Load()))
					progressMu.Unlock()
				}
			}
		}()
	}
	seen := make(map[uint64]struct{})
sendLoop:
	for {
		select {
		case <-ctx.Done():
			break sendLoop
		case accountID, ok := <-accountIDs:
			if !ok {
				break sendLoop
			}
			if accountID == 0 {
				continue
			}
			if _, exists := seen[accountID]; exists {
				continue
			}
			seen[accountID] = struct{}{}
			total.Add(1)
			select {
			case jobs <- accountID:
			case <-ctx.Done():
				break sendLoop
			}
		}
	}
	close(jobs)
	workers.Wait()
	return Result{Succeeded: int(succeeded.Load()), Failed: int(failed.Load())}
}

func (s *Service) syncAccount(ctx context.Context, accountID uint64) error {
	var syncErr error
	billingSnapshotCreated := false
	view, err := s.accounts.Get(ctx, accountID)
	if err != nil {
		return fmt.Errorf("读取账号: %w", err)
	}
	policy, ok := s.accounts.(providerPolicy)
	if !ok {
		return fmt.Errorf("账号读取器未提供 Provider 生命周期策略")
	}
	definition, ok := policy.ProviderDefinition(view.Credential.Provider)
	if !ok {
		return fmt.Errorf("Provider %s 未注册生命周期策略", view.Credential.Provider)
	}
	if definition.Quota == provider.QuotaRemoteWindow || definition.Quota == provider.QuotaLocalWindow {
		hasQuota, quotaErr := s.quota.HasQuotaWindows(ctx, accountID)
		if quotaErr != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("检查 Provider 额度快照: %w", quotaErr))
		} else if !hasQuota {
			operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
			_, quotaErr = s.quota.RefreshQuota(operationCtx, accountID)
			cancel()
			if quotaErr != nil {
				syncErr = errors.Join(syncErr, fmt.Errorf("同步 Provider 额度: %w", quotaErr))
			}
		}
	} else {
		hasBilling, err := s.billing.HasBillingSnapshot(ctx, accountID)
		if err != nil {
			s.logger.Warn("account_initial_billing_check_failed", "account_id", accountID, "error", err)
			syncErr = errors.Join(syncErr, fmt.Errorf("检查额度快照: %w", err))
		} else if !hasBilling {
			operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
			_, err = s.billing.RefreshBilling(operationCtx, accountID)
			cancel()
			if err != nil {
				s.logger.Warn("account_initial_billing_sync_failed", "account_id", accountID, "error", err)
				syncErr = errors.Join(syncErr, fmt.Errorf("同步额度: %w", err))
			} else {
				billingSnapshotCreated = true
			}
		}
	}

	hasModels, err := s.models.HasSuccessfulAccountSync(ctx, accountID)
	if err != nil {
		s.logger.Warn("account_initial_model_check_failed", "account_id", accountID, "error", err)
		return errors.Join(syncErr, fmt.Errorf("检查模型快照: %w", err))
	}
	if hasModels && !billingSnapshotCreated {
		return syncErr
	}
	operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	_, err = s.models.SyncAccount(operationCtx, accountID)
	cancel()
	if err != nil {
		s.logger.Warn("account_initial_model_sync_failed", "account_id", accountID, "error", err)
		syncErr = errors.Join(syncErr, fmt.Errorf("同步模型: %w", err))
	}
	return syncErr
}
