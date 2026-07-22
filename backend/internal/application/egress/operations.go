package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	defaultProbeIntervalSeconds      = 900
	defaultAssignmentIntervalSeconds = 300
	maxEgressAccountCapacity         = 100000
	maxManualProbeNodes              = 200
	maxConcurrentProbes              = 8
)

var ErrOperationsUnavailable = errors.New("代理运营功能不可用")

// OperationsRepository is deliberately optional. Existing egress consumers
// still only need the narrow routing repository while relational persistence
// provides this richer administrative surface.
type OperationsRepository interface {
	ListEgressSources(context.Context) ([]domain.SubscriptionSource, error)
	ListDueEgressSources(context.Context, time.Time, int) ([]domain.SubscriptionSource, error)
	GetEgressSource(context.Context, uint64) (domain.SubscriptionSource, error)
	CreateEgressSource(context.Context, domain.SubscriptionSource) (domain.SubscriptionSource, error)
	UpdateEgressSource(context.Context, domain.SubscriptionSource) (domain.SubscriptionSource, error)
	DeleteEgressSource(context.Context, uint64) error
	UpdateEgressSourceSync(context.Context, uint64, time.Time, time.Time, int, string) error
	UpsertEgressNodesFromSource(context.Context, uint64, []domain.Node) (int, error)
	CreateEgressNodes(context.Context, []domain.Node) (int, error)
	UpdateEgressNodeProbe(context.Context, uint64, domain.ProbeResult) error
	ListDueEgressNodes(context.Context, time.Time, time.Duration, int) ([]domain.Node, error)
	GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error)
	SaveEgressOperationsConfig(context.Context, domain.OperationsConfig) (domain.OperationsConfig, error)
}

// NodeProber is implemented by the infrastructure egress manager. Its fixed
// probe endpoint prevents admin input from controlling the outbound target.
type NodeProber interface {
	ProbeEgressNode(context.Context, uint64) (domain.ProbeResult, error)
}

type OperationsConfigInvalidator interface {
	InvalidateOperationsConfig()
}

type SubscriptionSourceInput struct {
	Name                   string
	Scope                  domain.Scope
	Enabled                bool
	URL                    *string
	ClearURL               bool
	RefreshIntervalSeconds *int
	DefaultAccountCapacity *int
}

type ImportInput struct {
	Name            string
	Scope           domain.Scope
	AccountCapacity int
	Content         string
}

type ImportResult struct {
	Imported int
	Skipped  int
}

type ProbeBatchResult struct {
	Requested int
	Healthy   int
	Unhealthy int
}

type OperationsConfigInput struct {
	ProbeIntervalSeconds      int
	AutoAssignEnabled         bool
	AutoBalanceEnabled        bool
	AssignmentIntervalSeconds int
	Fallbacks                 map[domain.Scope]FallbackConfigInput
}

type FallbackConfigInput struct {
	Mode   domain.FallbackMode
	NodeID uint64
}

func (s *Service) operationsRepository() (OperationsRepository, error) {
	if s == nil || s.operations == nil {
		return nil, ErrOperationsUnavailable
	}
	return s.operations, nil
}

func (s *Service) SetNodeProber(value NodeProber) {
	s.mu.Lock()
	s.prober = value
	s.mu.Unlock()
}

func (s *Service) SetOperationsConfigInvalidator(value OperationsConfigInvalidator) {
	s.mu.Lock()
	s.operationsCache = value
	s.mu.Unlock()
}

func (s *Service) invalidateOperationsConfig() {
	s.mu.RLock()
	value := s.operationsCache
	s.mu.RUnlock()
	if value != nil {
		value.InvalidateOperationsConfig()
	}
}

func (s *Service) nodeProber() NodeProber {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.prober
}

func (s *Service) ListSources(ctx context.Context) ([]domain.PublicSubscriptionSource, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return nil, err
	}
	values, err := operations.ListEgressSources(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]domain.PublicSubscriptionSource, 0, len(values))
	for _, value := range values {
		result = append(result, publicSource(value))
	}
	return result, nil
}

func (s *Service) CreateSource(ctx context.Context, input SubscriptionSourceInput) (domain.PublicSubscriptionSource, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	value, err := s.applySourceInput(domain.SubscriptionSource{}, input, true)
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	created, err := operations.CreateEgressSource(ctx, value)
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	return publicSource(created), nil
}

func (s *Service) UpdateSource(ctx context.Context, id uint64, input SubscriptionSourceInput) (domain.PublicSubscriptionSource, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	value, err := operations.GetEgressSource(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.PublicSubscriptionSource{}, ErrNotFound
	}
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	previousScope := value.Scope
	value, err = s.applySourceInput(value, input, false)
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	if previousScope != value.Scope {
		if err := s.validateSourceBindingScope(ctx, value.ID, value.Scope); err != nil {
			return domain.PublicSubscriptionSource{}, err
		}
	}
	updated, err := operations.UpdateEgressSource(ctx, value)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.PublicSubscriptionSource{}, ErrNotFound
	}
	if err != nil {
		return domain.PublicSubscriptionSource{}, err
	}
	return publicSource(updated), nil
}

func (s *Service) DeleteSource(ctx context.Context, id uint64) error {
	operations, err := s.operationsRepository()
	if err != nil {
		return err
	}
	err = operations.DeleteEgressSource(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func (s *Service) SyncSource(ctx context.Context, id uint64) (ImportResult, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return ImportResult{}, err
	}
	source, err := operations.GetEgressSource(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ImportResult{}, ErrNotFound
	}
	if err != nil {
		return ImportResult{}, err
	}
	return s.syncSource(ctx, operations, source)
}

func (s *Service) ImportText(ctx context.Context, input ImportInput) (ImportResult, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return ImportResult{}, err
	}
	if err := validateImportInput(input); err != nil {
		return ImportResult{}, err
	}
	entries, skipped, err := parseProxySubscription(input.Content)
	if err != nil {
		return ImportResult{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	nodes := make([]domain.Node, 0, len(entries))
	for index, entry := range entries {
		encryptedProxy, encryptErr := s.cipher.Encrypt(entry.ProxyURL)
		if encryptErr != nil {
			return ImportResult{}, encryptErr
		}
		nodes = append(nodes, domain.Node{
			Name: sourceNodeName(input.Name, index), Scope: input.Scope, Enabled: true,
			AccountCapacity: input.AccountCapacity, EncryptedProxyURL: encryptedProxy, Health: 1,
			ProbeStatus: domain.ProbeStatusUnknown,
		})
	}
	created, err := operations.CreateEgressNodes(ctx, nodes)
	if err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Imported: created, Skipped: skipped}, nil
}

func (s *Service) TestNode(ctx context.Context, id uint64) (domain.ProbeResult, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return domain.ProbeResult{}, err
	}
	if _, err := s.repository.GetEgressNode(ctx, id); errors.Is(err, repository.ErrNotFound) {
		return domain.ProbeResult{}, ErrNotFound
	} else if err != nil {
		return domain.ProbeResult{}, err
	}
	prober := s.nodeProber()
	if prober == nil {
		return domain.ProbeResult{}, ErrOperationsUnavailable
	}
	result, probeErr := prober.ProbeEgressNode(ctx, id)
	if result.TestedAt.IsZero() {
		result.TestedAt = time.Now().UTC()
	}
	if !result.Status.IsValid() {
		result.Status = domain.ProbeStatusUnhealthy
	}
	if probeErr != nil {
		result.Status = domain.ProbeStatusUnhealthy
		if strings.TrimSpace(result.Error) == "" {
			result.Error = "代理探测失败"
		}
	}
	if updateErr := operations.UpdateEgressNodeProbe(ctx, id, result); updateErr != nil {
		if errors.Is(updateErr, repository.ErrNotFound) {
			return result, ErrNotFound
		}
		return result, updateErr
	}
	// An unreachable proxy is a completed probe with an unhealthy result, not
	// an API operation failure. Persistence and repository failures still return
	// above so callers can distinguish them from node health.
	return result, nil
}

func (s *Service) TestNodes(ctx context.Context, ids []uint64) (ProbeBatchResult, error) {
	if len(ids) == 0 {
		nodes, err := s.repository.ListEgressNodes(ctx, "", repository.SortQuery{})
		if err != nil {
			return ProbeBatchResult{}, err
		}
		ids = make([]uint64, 0, len(nodes))
		for _, node := range nodes {
			if node.Enabled && node.EncryptedProxyURL != "" {
				ids = append(ids, node.ID)
			}
		}
	}
	ids = uniqueIDs(ids)
	if len(ids) > maxManualProbeNodes {
		return ProbeBatchResult{}, fmt.Errorf("%w: 单次最多测试 %d 个代理", ErrInvalidInput, maxManualProbeNodes)
	}
	result := ProbeBatchResult{Requested: len(ids)}
	if len(ids) == 0 {
		return result, nil
	}
	var mu sync.Mutex
	jobs := make(chan uint64)
	var workers sync.WaitGroup
	for range min(maxConcurrentProbes, len(ids)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				probe, err := s.TestNode(ctx, id)
				mu.Lock()
				if err == nil && probe.Status == domain.ProbeStatusHealthy {
					result.Healthy++
				} else {
					result.Unhealthy++
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return result, ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	return result, nil
}

func (s *Service) OperationsConfig(ctx context.Context) (domain.OperationsConfig, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return domain.OperationsConfig{}, err
	}
	return operations.GetEgressOperationsConfig(ctx)
}

func (s *Service) UpdateOperationsConfig(ctx context.Context, input OperationsConfigInput) (domain.OperationsConfig, error) {
	operations, err := s.operationsRepository()
	if err != nil {
		return domain.OperationsConfig{}, err
	}
	if input.ProbeIntervalSeconds < 60 || input.ProbeIntervalSeconds > 86400 || input.AssignmentIntervalSeconds < 60 || input.AssignmentIntervalSeconds > 86400 {
		return domain.OperationsConfig{}, fmt.Errorf("%w: 自动任务间隔必须在 60 到 86400 秒之间", ErrInvalidInput)
	}
	current, err := operations.GetEgressOperationsConfig(ctx)
	if err != nil {
		return domain.OperationsConfig{}, err
	}
	fallbacks := current.Fallbacks
	if input.Fallbacks != nil {
		fallbacks, err = s.validateFallbacks(ctx, current, input.Fallbacks)
		if err != nil {
			return domain.OperationsConfig{}, err
		}
	}
	saved, err := operations.SaveEgressOperationsConfig(ctx, domain.OperationsConfig{
		ProbeIntervalSeconds: input.ProbeIntervalSeconds, AutoAssignEnabled: input.AutoAssignEnabled,
		AutoBalanceEnabled: input.AutoBalanceEnabled, AssignmentIntervalSeconds: input.AssignmentIntervalSeconds,
		Fallbacks: fallbacks, UpdatedAt: time.Now().UTC(),
	})
	if err == nil {
		s.invalidateOperationsConfig()
	}
	return saved, err
}

func (s *Service) validateFallbacks(ctx context.Context, current domain.OperationsConfig, input map[domain.Scope]FallbackConfigInput) (map[domain.Scope]domain.FallbackConfig, error) {
	result := make(map[domain.Scope]domain.FallbackConfig, 4)
	for _, scope := range []domain.Scope{domain.ScopeBuild, domain.ScopeWeb, domain.ScopeConsole, domain.ScopeWebAsset} {
		result[scope] = current.FallbackFor(scope)
	}
	for scope, fallback := range input {
		if !validScope(scope) {
			return nil, fmt.Errorf("%w: 回退作用域无效", ErrInvalidInput)
		}
		mode := fallback.Mode.Normalized()
		if !mode.IsValid() {
			return nil, fmt.Errorf("%w: 回退模式无效", ErrInvalidInput)
		}
		switch mode {
		case domain.FallbackModeNone, domain.FallbackModeDirect:
			if fallback.NodeID != 0 {
				return nil, fmt.Errorf("%w: 仅固定代理回退可以指定节点", ErrInvalidInput)
			}
		case domain.FallbackModeFixed:
			if fallback.NodeID == 0 {
				return nil, fmt.Errorf("%w: 固定代理回退必须指定节点", ErrInvalidInput)
			}
			node, err := s.repository.GetEgressNode(ctx, fallback.NodeID)
			if errors.Is(err, repository.ErrNotFound) {
				return nil, fmt.Errorf("%w: 固定回退节点不存在", ErrInvalidInput)
			}
			if err != nil {
				return nil, err
			}
			if err := s.validateFixedFallbackNode(scope, node, true); err != nil {
				return nil, err
			}
		}
		result[scope] = domain.FallbackConfig{Mode: mode, NodeID: fallback.NodeID}
	}
	return result, nil
}

func (s *Service) validateFixedFallbackNode(scope domain.Scope, node domain.Node, rejectCooldown bool) error {
	if !domain.SupportsScope(node.Scope, scope) {
		return fmt.Errorf("%w: 固定回退节点与 %s 作用域不兼容", ErrInvalidInput, scope)
	}
	if !node.Enabled || strings.TrimSpace(node.EncryptedProxyURL) == "" {
		return fmt.Errorf("%w: 固定回退节点必须启用且配置代理地址", ErrInvalidInput)
	}
	if node.ProxyPool {
		return fmt.Errorf("%w: 固定回退节点不能使用代理池模式", ErrInvalidInput)
	}
	if rejectCooldown && node.CooldownUntil != nil && time.Now().UTC().Before(*node.CooldownUntil) {
		return fmt.Errorf("%w: 固定回退节点正在冷却", ErrInvalidInput)
	}
	proxyURL, err := s.cipher.Decrypt(node.EncryptedProxyURL)
	if err != nil {
		return fmt.Errorf("%w: 固定回退节点代理配置无效", ErrInvalidInput)
	}
	proxyURL, err = NormalizeProxyURL(proxyURL)
	if err != nil || proxyURL == "" {
		return fmt.Errorf("%w: 固定回退节点代理地址无效", ErrInvalidInput)
	}
	if strings.Contains(proxyURL, ProxyAccountPlaceholder) {
		return fmt.Errorf("%w: 固定回退节点不能使用账号代理模板", ErrInvalidInput)
	}
	return nil
}

func (s *Service) applySourceInput(value domain.SubscriptionSource, input SubscriptionSourceInput, create bool) (domain.SubscriptionSource, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 160 {
		return domain.SubscriptionSource{}, fmt.Errorf("%w: 订阅名称必须在 1 到 160 个字符之间", ErrInvalidInput)
	}
	if !validScope(input.Scope) {
		return domain.SubscriptionSource{}, fmt.Errorf("%w: 订阅作用域无效", ErrInvalidInput)
	}
	value.Name, value.Scope, value.Enabled = name, input.Scope, input.Enabled
	if input.RefreshIntervalSeconds != nil {
		if *input.RefreshIntervalSeconds < 60 || *input.RefreshIntervalSeconds > 86400 {
			return domain.SubscriptionSource{}, fmt.Errorf("%w: 订阅刷新间隔必须在 60 到 86400 秒之间", ErrInvalidInput)
		}
		value.RefreshIntervalSeconds = *input.RefreshIntervalSeconds
	}
	if value.RefreshIntervalSeconds == 0 {
		value.RefreshIntervalSeconds = defaultProbeIntervalSeconds
	}
	if input.DefaultAccountCapacity != nil {
		if *input.DefaultAccountCapacity < 0 || *input.DefaultAccountCapacity > maxEgressAccountCapacity {
			return domain.SubscriptionSource{}, fmt.Errorf("%w: 每个代理的账号容量必须在 0 到 %d 之间", ErrInvalidInput, maxEgressAccountCapacity)
		}
		value.DefaultAccountCapacity = *input.DefaultAccountCapacity
	}
	if input.ClearURL {
		value.EncryptedURL = ""
	} else if input.URL != nil {
		urlValue, err := normalizeSubscriptionURL(*input.URL)
		if err != nil {
			return domain.SubscriptionSource{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		encryptedURL, err := s.cipher.Encrypt(urlValue)
		if err != nil {
			return domain.SubscriptionSource{}, err
		}
		value.EncryptedURL = encryptedURL
	}
	if create && value.EncryptedURL == "" {
		return domain.SubscriptionSource{}, fmt.Errorf("%w: 必须提供订阅地址", ErrInvalidInput)
	}
	if create || input.URL != nil || input.ClearURL {
		value.NextSyncAt = nil
		value.LastSyncError = ""
	}
	return value, nil
}

func publicSource(value domain.SubscriptionSource) domain.PublicSubscriptionSource {
	return domain.PublicSubscriptionSource{
		ID: value.ID, Name: value.Name, Scope: value.Scope, Enabled: value.Enabled, URLConfigured: value.EncryptedURL != "",
		RefreshIntervalSeconds: value.RefreshIntervalSeconds, DefaultAccountCapacity: value.DefaultAccountCapacity,
		LastSyncedAt: value.LastSyncedAt, NextSyncAt: value.NextSyncAt, LastSyncImported: value.LastSyncImported, LastSyncError: value.LastSyncError,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func validScope(scope domain.Scope) bool {
	return scope == domain.ScopeBuild || scope == domain.ScopeWeb || scope == domain.ScopeConsole || scope == domain.ScopeWebAsset
}

func validateImportInput(input ImportInput) error {
	if strings.TrimSpace(input.Name) == "" || len(strings.TrimSpace(input.Name)) > 150 || !validScope(input.Scope) || input.AccountCapacity < 0 || input.AccountCapacity > maxEgressAccountCapacity || strings.TrimSpace(input.Content) == "" {
		return fmt.Errorf("%w: 批量导入参数无效", ErrInvalidInput)
	}
	return nil
}
