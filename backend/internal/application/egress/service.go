package egress

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrInvalidInput            = errors.New("代理节点参数无效")
	ErrInvalidFilter           = errors.New("出口代理筛选条件无效")
	ErrInvalidSort             = errors.New("代理节点排序条件无效")
	ErrNotFound                = errors.New("代理节点不存在")
	ErrProbeStale              = errors.New("代理配置在探测期间已更新，请重新测试")
	ErrQualityProbeUnavailable = errors.New("出口质量探测不可用")
	ErrQualityProbeNoAccount   = errors.New("质量检测暂无可调度账号")
	ErrClearanceUnavailable    = errors.New("Clearance 刷新不可用")
)

const (
	DefaultQualityProbePrompt          = "Reply with exactly QUALITY_OK."
	DefaultQualityProbeExpected        = "QUALITY_OK"
	DefaultQualityProbeMaxOutputTokens = 64
	MaxQualityProbePromptBytes         = 4096
	MaxQualityProbeExpectedBytes       = 512
	MaxQualityProbeOutputTokens        = 2048
)

type QualityProbeInput struct {
	ClientKeyID     uint64
	Model           string
	Prompt          string
	Expected        string
	MatchMode       string
	RequireThinking bool
	MaxOutputTokens int
}

type QualityProbeResult struct {
	RequestID             string
	NodeID                uint64
	Model                 string
	StatusCode            int
	FirstTokenMS          int64
	DurationMS            int64
	GenerationMS          int64
	ChunkCount            int
	OutputTokens          int64
	ReasoningTokens       int64
	VisibleTokens         int64
	VisibleCharacters     int
	OutputTokensPerSecond float64
	ExpectedMatched       bool
	ThinkingRequired      bool
	ResponseSHA256        string
}

type QualityProber interface {
	ProbeEgressQuality(context.Context, uint64, QualityProbeInput) (QualityProbeResult, error)
}

const (
	maxProxyURLBytes         = 8192
	maxCloudflareCookieBytes = 16 << 10
	ProxyAccountPlaceholder  = "{account}"
	proxyAccountSentinel     = "grok2api_account_placeholder"
)

type Input struct {
	Name              string
	Scope             domain.Scope
	Enabled           bool
	ProxyPool         *bool
	AccountCapacity   *int
	ProxyURL          *string
	ClearProxyURL     bool
	UserAgent         string
	CloudflareCookies *string
	ClearCookies      bool
}

type ListFilter struct {
	Scope       domain.Scope
	Enabled     string
	ProbeStatus string
	Assignment  string
	Sort        repository.SortQuery
}

type ServiceRepository interface {
	repository.EgressRepository
	repository.EgressNodePageRepository
}

type Service struct {
	repository        ServiceRepository
	accounts          AccountBindingRepository
	operations        OperationsRepository
	cipher            *security.Cipher
	mu                sync.RWMutex
	browserUA         string
	clearance         ClearanceManager
	prober            NodeProber
	operationsCache   OperationsConfigInvalidator
	qualityProber     QualityProber
	assignmentMu      sync.Mutex
	lastAssignmentRun time.Time
	assignmentRunning bool
}

func (s *Service) SetQualityProber(value QualityProber) {
	s.mu.Lock()
	s.qualityProber = value
	s.mu.Unlock()
}

func (s *Service) ProbeQuality(ctx context.Context, nodeID uint64, input QualityProbeInput) (QualityProbeResult, error) {
	if nodeID == 0 || input.ClientKeyID == 0 {
		return QualityProbeResult{}, fmt.Errorf("%w: nodeId 和 clientKeyId 必填", ErrInvalidInput)
	}
	input.Model = strings.TrimSpace(input.Model)
	input.Prompt = strings.TrimSpace(input.Prompt)
	input.Expected = strings.TrimSpace(input.Expected)
	rawMatchMode := strings.TrimSpace(input.MatchMode)
	input.MatchMode = NormalizeMatchMode(input.MatchMode)
	if input.Model == "" {
		return QualityProbeResult{}, fmt.Errorf("%w: model 必填", ErrInvalidInput)
	}
	if input.Prompt == "" {
		input.Prompt = DefaultQualityProbePrompt
	}
	if input.Expected == "" && rawMatchMode == "" {
		input.Expected = DefaultQualityProbeExpected
	}
	if len(input.Prompt) > MaxQualityProbePromptBytes || len(input.Expected) > MaxQualityProbeExpectedBytes {
		return QualityProbeResult{}, fmt.Errorf("%w: 探测文本过长", ErrInvalidInput)
	}
	if input.MaxOutputTokens == 0 {
		input.MaxOutputTokens = DefaultQualityProbeMaxOutputTokens
	}
	if input.MaxOutputTokens < 1 || input.MaxOutputTokens > MaxQualityProbeOutputTokens {
		return QualityProbeResult{}, fmt.Errorf("%w: maxOutputTokens 必须在 1 到 %d 之间", ErrInvalidInput, MaxQualityProbeOutputTokens)
	}
	node, err := s.repository.GetEgressNode(ctx, nodeID)
	if errors.Is(err, repository.ErrNotFound) {
		return QualityProbeResult{}, ErrNotFound
	}
	if err != nil {
		return QualityProbeResult{}, err
	}
	if node.Scope != domain.ScopeBuild || strings.TrimSpace(node.EncryptedProxyURL) == "" {
		return QualityProbeResult{}, fmt.Errorf("%w: 质量探测仅支持已配置代理的 grok_build 节点", ErrInvalidInput)
	}
	s.mu.RLock()
	prober := s.qualityProber
	s.mu.RUnlock()
	if prober == nil {
		return QualityProbeResult{}, ErrQualityProbeUnavailable
	}
	// A profile may request the thinking guard, but only a known reasoning-capable
	// Build model can make zero reasoning tokens meaningful. Unknown/custom and
	// non-reasoning models stay observable without being falsely quarantined.
	input.RequireThinking = input.RequireThinking && modeldomain.SupportsReasoningForProvider(accountdomain.ProviderBuild, input.Model)
	result, err := prober.ProbeEgressQuality(ctx, nodeID, input)
	if err != nil {
		return QualityProbeResult{}, err
	}
	result.ThinkingRequired = input.RequireThinking
	return result, nil
}

// AccountBindingRepository is intentionally narrow so existing account
// repository consumers do not gain egress concerns.
type AccountBindingRepository interface {
	CountProviderAccountsByIDs(context.Context, accountdomain.Provider, []uint64) (int64, error)
	UpdateEgressBindings(context.Context, accountdomain.Provider, []uint64, *uint64, accountdomain.EgressAssignmentMode, time.Time) (int64, error)
	ListEgressAssignments(context.Context, accountdomain.Provider) ([]accountdomain.Credential, error)
	ListEgressBindingProviders(context.Context, uint64) ([]accountdomain.Provider, error)
	ListEgressSourceBindingProviders(context.Context, uint64) ([]accountdomain.Provider, error)
}

type AssignmentResult struct {
	Assigned int
}

type UnhealthyCleanupPreview struct {
	Nodes               int64
	BoundAccounts       int64
	SubscriptionManaged int64
}

// BatchNodeDeleter is optional so lightweight repository adapters only need
// the single-node contract unless they can provide an atomic bulk operation.
type BatchNodeDeleter interface {
	DeleteEgressNodes(context.Context, []uint64) (int, error)
}

type BatchNodeEnabledUpdater interface {
	UpdateEgressNodesEnabled(context.Context, []uint64, bool) (int, error)
}

type ClearanceManager interface {
	RefreshClearance(context.Context, uint64) error
	ForgetClearance(uint64)
}

type BatchClearanceManager interface {
	ForgetClearances([]uint64)
}

func NewService(repository ServiceRepository, cipher *security.Cipher, browserUA string, accounts ...AccountBindingRepository) *Service {
	service := &Service{repository: repository, cipher: cipher, browserUA: strings.TrimSpace(browserUA)}
	if operations, ok := repository.(OperationsRepository); ok {
		service.operations = operations
	}
	if len(accounts) > 0 {
		service.accounts = accounts[0]
	}
	return service
}

func (s *Service) UpdateDefaults(browserUA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.browserUA = strings.TrimSpace(browserUA)
}

func (s *Service) SetClearanceManager(value ClearanceManager) {
	s.mu.Lock()
	s.clearance = value
	s.mu.Unlock()
}

func (s *Service) DefaultUserAgents() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]string{
		string(domain.ScopeBuild): "", string(domain.ScopeWeb): s.browserUA, string(domain.ScopeConsole): s.browserUA,
		string(domain.ScopeWebAsset): s.browserUA, string(domain.ScopeConsoleAsset): s.browserUA,
	}
}

func (s *Service) List(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]domain.PublicNode, int64, error) {
	page, pageSize = repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
	if !validListScope(filter.Scope) || !validListValue(filter.Enabled, "enabled", "disabled") ||
		!validListValue(filter.ProbeStatus, string(domain.ProbeStatusHealthy), string(domain.ProbeStatusUnhealthy), string(domain.ProbeStatusUnknown)) ||
		!validListValue(filter.Assignment, "bound", "unbound") {
		return nil, 0, ErrInvalidFilter
	}
	if !repository.IsValidSort(filter.Sort, "name", "scope", "proxy", "clearance", "health") {
		return nil, 0, ErrInvalidSort
	}
	var enabled *bool
	if filter.Enabled != "" {
		value := filter.Enabled == "enabled"
		enabled = &value
	}
	values, total, err := s.repository.ListEgressNodePage(ctx, repository.EgressNodeListQuery{
		Page: repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: strings.TrimSpace(search), Sort: filter.Sort},
		Filter: repository.EgressNodeListFilter{
			Scope: filter.Scope, Enabled: enabled, ProbeStatus: domain.ProbeStatus(filter.ProbeStatus), Assignment: filter.Assignment,
		},
	})
	if err != nil {
		return nil, 0, err
	}
	return s.publicNodes(values), total, nil
}

func (s *Service) ListAll(ctx context.Context, scope domain.Scope, sort repository.SortQuery) ([]domain.PublicNode, error) {
	if !validListScope(scope) {
		return nil, ErrInvalidFilter
	}
	if !repository.IsValidSort(sort, "name", "scope", "proxy", "clearance", "health") {
		return nil, ErrInvalidSort
	}
	values, err := s.repository.ListEgressNodes(ctx, scope, sort)
	if err != nil {
		return nil, err
	}
	return s.publicNodes(values), nil
}

func (s *Service) publicNodes(values []domain.Node) []domain.PublicNode {
	result := make([]domain.PublicNode, 0, len(values))
	for _, value := range values {
		result = append(result, s.publicNode(value))
	}
	return result
}

func validListScope(scope domain.Scope) bool {
	return scope == "" || scope == domain.ScopeBuild || scope == domain.ScopeWeb || scope == domain.ScopeConsole || scope == domain.ScopeWebAsset || scope == domain.ScopeConsoleAsset
}

func allServiceScopes() []domain.Scope {
	return []domain.Scope{domain.ScopeBuild, domain.ScopeWeb, domain.ScopeConsole, domain.ScopeWebAsset, domain.ScopeConsoleAsset}
}

func validListValue(value string, allowed ...string) bool {
	if value == "" {
		return true
	}
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func (s *Service) Create(ctx context.Context, input Input) (domain.PublicNode, error) {
	value, err := s.applyInput(domain.Node{}, input, true)
	if err != nil {
		return domain.PublicNode{}, err
	}
	created, err := s.repository.CreateEgressNode(ctx, value)
	if err == nil {
		s.forgetClearance(created.ID)
	}
	return s.publicNode(created), err
}

func (s *Service) Update(ctx context.Context, id uint64, input Input) (domain.PublicNode, error) {
	value, err := s.repository.GetEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.PublicNode{}, ErrNotFound
	}
	if err != nil {
		return domain.PublicNode{}, err
	}
	previousScope := value.Scope
	value, err = s.applyInput(value, input, false)
	if err != nil {
		return domain.PublicNode{}, err
	}
	if err := s.validateFallbackNodeUpdate(ctx, value); err != nil {
		return domain.PublicNode{}, err
	}
	if previousScope != value.Scope {
		if err := s.validateNodeBindingScope(ctx, value.ID, value.Scope); err != nil {
			return domain.PublicNode{}, err
		}
	}
	updated, err := s.repository.UpdateEgressNode(ctx, value)
	if err == nil {
		s.forgetClearance(updated.ID)
	}
	return s.publicNode(updated), err
}

func (s *Service) validateFallbackNodeUpdate(ctx context.Context, node domain.Node) error {
	if s.operations == nil {
		return nil
	}
	config, err := s.operations.GetEgressOperationsConfig(ctx)
	if err != nil {
		return err
	}
	return s.validateFallbackNodeUpdateWithConfig(node, config)
}

func (s *Service) validateFallbackNodeUpdateWithConfig(node domain.Node, config domain.OperationsConfig) error {
	for _, scope := range allServiceScopes() {
		fallback := config.FallbackFor(scope)
		if fallback.Mode != domain.FallbackModeFixed || fallback.NodeID != node.ID {
			continue
		}
		if err := s.validateFixedFallbackNode(scope, node, false); err != nil {
			return fmt.Errorf("节点已配置为 %s 固定回退，无法应用当前修改: %w", scope, err)
		}
	}
	return nil
}

// UpdateManyEnabled changes only the scheduling state, leaving proxy secrets,
// health, probes, and account bindings untouched.
func (s *Service) UpdateManyEnabled(ctx context.Context, nodeIDs []uint64, enabled bool) (int, error) {
	ids := uniqueIDs(nodeIDs)
	if len(ids) == 0 {
		return 0, fmt.Errorf("%w: 代理节点参数无效", ErrInvalidInput)
	}

	// Disabling a fixed fallback would make the persisted routing policy
	// invalid. At most five fallback nodes need point lookups, regardless of
	// the batch size.
	if !enabled && s.operations != nil {
		config, err := s.operations.GetEgressOperationsConfig(ctx)
		if err != nil {
			return 0, err
		}
		selected := make(map[uint64]struct{}, len(ids))
		for _, id := range ids {
			selected[id] = struct{}{}
		}
		fallbackNodeIDs := make(map[uint64]struct{}, len(allServiceScopes()))
		for _, scope := range allServiceScopes() {
			fallback := config.FallbackFor(scope)
			if fallback.Mode == domain.FallbackModeFixed {
				if _, exists := selected[fallback.NodeID]; exists {
					fallbackNodeIDs[fallback.NodeID] = struct{}{}
				}
			}
		}
		for id := range fallbackNodeIDs {
			node, err := s.repository.GetEgressNode(ctx, id)
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			if err != nil {
				return 0, err
			}
			node.Enabled = false
			if err := s.validateFallbackNodeUpdateWithConfig(node, config); err != nil {
				return 0, err
			}
		}
	}

	if batch, ok := s.repository.(BatchNodeEnabledUpdater); ok {
		updated, err := batch.UpdateEgressNodesEnabled(ctx, ids, enabled)
		if errors.Is(err, repository.ErrEgressFallbackInUse) {
			return 0, fmt.Errorf("%w: 固定回退节点不能被批量禁用", ErrInvalidInput)
		}
		if err != nil {
			return 0, err
		}
		if updated > 0 {
			s.forgetClearances(ids)
		}
		return updated, nil
	}

	updated := 0
	for _, id := range ids {
		node, err := s.repository.GetEgressNode(ctx, id)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return updated, err
		}
		if node.Enabled == enabled {
			continue
		}
		node.Enabled = enabled
		if _, err := s.repository.UpdateEgressNode(ctx, node); err != nil {
			return updated, err
		}
		s.forgetClearance(id)
		updated++
	}
	return updated, nil
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	err := s.repository.DeleteEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err == nil {
		s.forgetClearance(id)
		s.invalidateOperationsConfig()
	}
	return err
}

// DeleteMany removes nodes in one repository operation when available. The
// relational implementation also clears any account bindings in that same
// transaction, so a deleted node can never remain referenced by an account.
func (s *Service) DeleteMany(ctx context.Context, nodeIDs []uint64) (int, error) {
	ids := uniqueIDs(nodeIDs)
	if len(ids) == 0 {
		return 0, fmt.Errorf("%w: 代理节点参数无效", ErrInvalidInput)
	}
	if batch, ok := s.repository.(BatchNodeDeleter); ok {
		deleted, err := batch.DeleteEgressNodes(ctx, ids)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			s.forgetClearance(id)
		}
		s.invalidateOperationsConfig()
		return deleted, nil
	}

	deleted := 0
	for _, id := range ids {
		if err := s.Delete(ctx, id); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func (s *Service) PreviewUnhealthyCleanup(ctx context.Context) (UnhealthyCleanupPreview, error) {
	if cleaner, ok := s.repository.(repository.EgressNodeUnhealthyCleaner); ok {
		value, err := cleaner.PreviewUnhealthyEgressNodes(ctx)
		return UnhealthyCleanupPreview{
			Nodes: value.Nodes, BoundAccounts: value.BoundAccounts, SubscriptionManaged: value.SubscriptionManaged,
		}, err
	}
	values, err := s.repository.ListEgressNodes(ctx, "", repository.SortQuery{})
	if err != nil {
		return UnhealthyCleanupPreview{}, err
	}
	result := UnhealthyCleanupPreview{}
	for _, value := range values {
		if value.IPv4Probe.Status != domain.ProbeStatusUnhealthy || value.IPv6Probe.Status != domain.ProbeStatusUnhealthy {
			continue
		}
		result.Nodes++
		result.BoundAccounts += int64(value.AssignedAccountCount)
		if value.SourceID != 0 {
			result.SubscriptionManaged++
		}
	}
	return result, nil
}

func (s *Service) DeleteUnhealthy(ctx context.Context) (int, error) {
	if cleaner, ok := s.repository.(repository.EgressNodeUnhealthyCleaner); ok {
		ids, err := cleaner.DeleteUnhealthyEgressNodes(ctx)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			s.forgetClearance(id)
		}
		if len(ids) > 0 {
			s.invalidateOperationsConfig()
		}
		return len(ids), nil
	}
	values, err := s.repository.ListEgressNodes(ctx, "", repository.SortQuery{})
	if err != nil {
		return 0, err
	}
	ids := make([]uint64, 0)
	for _, value := range values {
		if value.IPv4Probe.Status == domain.ProbeStatusUnhealthy && value.IPv6Probe.Status == domain.ProbeStatusUnhealthy {
			ids = append(ids, value.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return s.DeleteMany(ctx, ids)
}

func (s *Service) RefreshClearance(ctx context.Context, id uint64) error {
	if _, err := s.repository.GetEgressNode(ctx, id); errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	s.mu.RLock()
	manager := s.clearance
	s.mu.RUnlock()
	if manager == nil {
		return ErrClearanceUnavailable
	}
	return manager.RefreshClearance(ctx, id)
}

// AssignAccounts creates explicit many-to-one account bindings. A binding is
// not a proxy-pool preference: runtime requests must use the selected node.
func (s *Service) AssignAccounts(ctx context.Context, nodeID uint64, provider accountdomain.Provider, accountIDs []uint64, mode accountdomain.EgressAssignmentMode) (AssignmentResult, error) {
	if s.accounts == nil {
		return AssignmentResult{}, errors.New("账号出口绑定不可用")
	}
	if nodeID == 0 || !provider.IsValid() || !mode.IsValid() || len(accountIDs) == 0 {
		return AssignmentResult{}, fmt.Errorf("%w: 账号出口绑定参数无效", ErrInvalidInput)
	}
	node, err := s.repository.GetEgressNode(ctx, nodeID)
	if errors.Is(err, repository.ErrNotFound) {
		return AssignmentResult{}, ErrNotFound
	}
	if err != nil {
		return AssignmentResult{}, err
	}
	if !node.Enabled || strings.TrimSpace(node.EncryptedProxyURL) == "" {
		return AssignmentResult{}, fmt.Errorf("%w: 只能绑定启用且已配置代理地址的节点", ErrInvalidInput)
	}
	if !scopeSupportsProvider(node.Scope, provider) {
		return AssignmentResult{}, fmt.Errorf("%w: 代理节点作用域与账号来源不兼容", ErrInvalidInput)
	}
	unique := uniqueIDs(accountIDs)
	count, err := s.accounts.CountProviderAccountsByIDs(ctx, provider, unique)
	if err != nil {
		return AssignmentResult{}, err
	}
	if count != int64(len(unique)) {
		return AssignmentResult{}, fmt.Errorf("%w: 包含不属于当前账号池的账号", ErrInvalidInput)
	}
	assigned, err := s.accounts.UpdateEgressBindings(ctx, provider, unique, &nodeID, mode, time.Now().UTC())
	if err != nil {
		return AssignmentResult{}, err
	}
	return AssignmentResult{Assigned: int(assigned)}, nil
}

// UnassignAccounts removes an explicit binding and restores scope pool routing.
func (s *Service) UnassignAccounts(ctx context.Context, provider accountdomain.Provider, accountIDs []uint64) (AssignmentResult, error) {
	if s.accounts == nil {
		return AssignmentResult{}, errors.New("账号出口绑定不可用")
	}
	if !provider.IsValid() || len(accountIDs) == 0 {
		return AssignmentResult{}, fmt.Errorf("%w: 账号出口解绑参数无效", ErrInvalidInput)
	}
	unique := uniqueIDs(accountIDs)
	count, err := s.accounts.CountProviderAccountsByIDs(ctx, provider, unique)
	if err != nil {
		return AssignmentResult{}, err
	}
	if count != int64(len(unique)) {
		return AssignmentResult{}, fmt.Errorf("%w: 包含不属于当前账号池的账号", ErrInvalidInput)
	}
	updated, err := s.accounts.UpdateEgressBindings(ctx, provider, unique, nil, "", time.Time{})
	if err != nil {
		return AssignmentResult{}, err
	}
	return AssignmentResult{Assigned: int(updated)}, nil
}

func scopeSupportsProvider(scope domain.Scope, provider accountdomain.Provider) bool {
	switch provider {
	case accountdomain.ProviderBuild:
		return scope == domain.ScopeBuild
	case accountdomain.ProviderWeb:
		return scope == domain.ScopeWeb
	case accountdomain.ProviderConsole:
		return domain.SupportsScope(scope, domain.ScopeConsole)
	default:
		return false
	}
}

func (s *Service) validateNodeBindingScope(ctx context.Context, nodeID uint64, scope domain.Scope) error {
	if s.accounts == nil {
		return nil
	}
	providers, err := s.accounts.ListEgressBindingProviders(ctx, nodeID)
	if err != nil {
		return err
	}
	return validateBindingProviders(scope, providers)
}

func (s *Service) validateSourceBindingScope(ctx context.Context, sourceID uint64, scope domain.Scope) error {
	if s.accounts == nil {
		return nil
	}
	providers, err := s.accounts.ListEgressSourceBindingProviders(ctx, sourceID)
	if err != nil {
		return err
	}
	return validateBindingProviders(scope, providers)
}

func validateBindingProviders(scope domain.Scope, providers []accountdomain.Provider) error {
	for _, provider := range providers {
		if !scopeSupportsProvider(scope, provider) {
			return fmt.Errorf("%w: 当前节点仍绑定 %s 账号，不能改为 %s 作用域", ErrInvalidInput, provider, scope)
		}
	}
	return nil
}

func uniqueIDs(values []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(values))
	result := make([]uint64, 0, len(values))
	for _, value := range values {
		if value == 0 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (s *Service) forgetClearance(id uint64) {
	s.mu.RLock()
	manager := s.clearance
	s.mu.RUnlock()
	if manager != nil {
		manager.ForgetClearance(id)
	}
}

func (s *Service) forgetClearances(ids []uint64) {
	s.mu.RLock()
	manager := s.clearance
	s.mu.RUnlock()
	if manager == nil {
		return
	}
	if batch, ok := manager.(BatchClearanceManager); ok {
		batch.ForgetClearances(ids)
		return
	}
	for _, id := range ids {
		manager.ForgetClearance(id)
	}
}

func (s *Service) applyInput(value domain.Node, input Input, create bool) (domain.Node, error) {
	proxyPool := value.ProxyPool
	if input.ProxyPool != nil {
		proxyPool = *input.ProxyPool
	}
	configurationChanged := create || value.Scope != input.Scope || value.ProxyPool != proxyPool || (!value.Enabled && input.Enabled) || input.ClearProxyURL || input.ProxyURL != nil
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 160 {
		return domain.Node{}, fmt.Errorf("%w: 名称必须在 1 到 160 个字符之间", ErrInvalidInput)
	}
	if !validListScope(input.Scope) || input.Scope == "" {
		return domain.Node{}, fmt.Errorf("%w: scope 必须是 grok_build、grok_web、grok_console、grok_web_asset 或 grok_console_asset", ErrInvalidInput)
	}
	value.Name, value.Scope, value.Enabled, value.ProxyPool = name, input.Scope, input.Enabled, proxyPool
	if input.AccountCapacity != nil {
		if *input.AccountCapacity < 0 || *input.AccountCapacity > 100000 {
			return domain.Node{}, fmt.Errorf("%w: 每个代理的账号容量必须在 0 到 100000 之间", ErrInvalidInput)
		}
		value.AccountCapacity = *input.AccountCapacity
	}
	if input.Scope == domain.ScopeBuild {
		// Build 请求始终沿用 Provider 生成的 CLI User-Agent，出口节点不得覆盖协议身份。
		value.UserAgent = ""
	} else {
		value.UserAgent = strings.TrimSpace(input.UserAgent)
	}
	if input.Scope != domain.ScopeBuild && value.UserAgent == "" {
		s.mu.RLock()
		value.UserAgent = s.browserUA
		s.mu.RUnlock()
	}
	if len(value.UserAgent) > 512 {
		return domain.Node{}, fmt.Errorf("%w: User-Agent 过长", ErrInvalidInput)
	}
	if input.ClearProxyURL {
		value.EncryptedProxyURL = ""
		value.ProxyPool = false
	} else if input.ProxyURL != nil {
		normalized, err := NormalizeProxyURL(*input.ProxyURL)
		if err != nil {
			return domain.Node{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		if normalized != "" {
			value.EncryptedProxyURL, err = s.cipher.Encrypt(normalized)
			if err != nil {
				return domain.Node{}, err
			}
		}
	}
	if value.ProxyPool && strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return domain.Node{}, fmt.Errorf("%w: 代理池模式需要配置代理地址", ErrInvalidInput)
	}
	if input.Scope == domain.ScopeBuild || input.Scope == domain.ScopeConsoleAsset {
		value.EncryptedCloudflareCookie = ""
	} else if input.ClearCookies {
		value.EncryptedCloudflareCookie = ""
	} else if input.CloudflareCookies != nil {
		if len(*input.CloudflareCookies) > maxCloudflareCookieBytes {
			return domain.Node{}, fmt.Errorf("%w: Cloudflare Cookie 不能超过 16 KiB", ErrInvalidInput)
		}
		cookies := SanitizeCloudflareCookies(*input.CloudflareCookies)
		if cookies != "" || create {
			var err error
			value.EncryptedCloudflareCookie, err = s.cipher.Encrypt(cookies)
			if err != nil {
				return domain.Node{}, err
			}
		}
	}
	if configurationChanged {
		value.Health = 1
		value.FailureCount = 0
		value.CooldownUntil = nil
		value.LastError = ""
		value.ProbeStatus = domain.ProbeStatusUnknown
		value.LastProbedAt = nil
		value.ProbeLatencyMS = 0
		value.ExitIP = ""
		value.ProbeError = ""
		value.ProbeProvider = ""
		value.IPv4Probe = domain.ProbeFamilyResult{Status: domain.ProbeStatusUnknown}
		value.IPv6Probe = domain.ProbeFamilyResult{Status: domain.ProbeStatusUnknown}
	}
	// Any administrator edit invalidates freshness. Keep the binding fingerprint:
	// managed mode may use the existing cookie as last-known-good only when the
	// target and actual proxy still match the binding that produced it.
	value.ClearanceRefreshedAt = nil
	value.ClearanceFingerprint = ""
	return value, nil
}

func (s *Service) publicNode(value domain.Node) domain.PublicNode {
	userAgent := value.UserAgent
	if value.Scope == domain.ScopeBuild {
		userAgent = ""
	}
	accountBoundProxy := s.accountBoundProxy(value)
	proxyPool := value.ProxyPool || accountBoundProxy
	health, failureCount, cooldownUntil, lastError := value.Health, value.FailureCount, value.CooldownUntil, value.LastError
	if proxyPool {
		health, failureCount, cooldownUntil, lastError = 1, 0, nil, ""
	}
	return domain.PublicNode{
		ID: value.ID, Name: value.Name, Scope: value.Scope, Enabled: value.Enabled,
		ProxyConfigured: value.EncryptedProxyURL != "", UserAgent: userAgent, CookieConfigured: value.EncryptedCloudflareCookie != "",
		ProxyPool:         proxyPool,
		SourceID:          value.SourceID,
		AccountCapacity:   value.AccountCapacity,
		AccountBoundProxy: accountBoundProxy,
		Health:            health, FailureCount: failureCount, CooldownUntil: cooldownUntil, LastError: lastError,
		ProbeStatus: value.ProbeStatus, LastProbedAt: value.LastProbedAt, ProbeLatencyMS: value.ProbeLatencyMS, ExitIP: value.ExitIP, ProbeError: value.ProbeError,
		ProbeProvider: value.ProbeProvider,
		IPv4Probe:     value.IPv4Probe, IPv6Probe: value.IPv6Probe,
		AssignedAccountCount: value.AssignedAccountCount,
		CreatedAt:            value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func (s *Service) accountBoundProxy(value domain.Node) bool {
	if s == nil || s.cipher == nil || strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return false
	}
	proxyURL, err := s.cipher.Decrypt(value.EncryptedProxyURL)
	return err == nil && strings.Contains(proxyURL, ProxyAccountPlaceholder)
}

func NormalizeProxyURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maxProxyURLBytes || strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return "", errors.New("代理地址过长或包含控制字符")
	}
	hasAccountPlaceholder := strings.Contains(value, ProxyAccountPlaceholder)
	if strings.Count(value, ProxyAccountPlaceholder) > 1 {
		return "", errors.New("代理地址最多包含一个 {account} 占位符")
	}
	if hasAccountPlaceholder && strings.Contains(value, proxyAccountSentinel) {
		return "", errors.New("代理地址包含保留的账号占位符文本")
	}
	parseValue := strings.ReplaceAll(value, ProxyAccountPlaceholder, proxyAccountSentinel)
	parsed, err := url.Parse(parseValue)
	if err != nil {
		return "", errors.New("代理地址格式无效")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if tunnelproxy.IsSupportedScheme(scheme) {
		if hasAccountPlaceholder {
			return "", errors.New("隧道代理不支持 {account} 占位符")
		}
		normalized, normalizeErr := tunnelproxy.Normalize(value)
		if normalizeErr != nil {
			return "", normalizeErr
		}
		return normalized, nil
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return "", errors.New("代理地址格式无效")
	}
	switch scheme {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
	default:
		return "", errors.New("代理地址协议必须是 HTTP、HTTPS、SOCKS4、SOCKS5、Trojan、VLESS、SS 或 VMess")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("代理地址不能包含路径、查询参数或片段")
	}
	if hasAccountPlaceholder {
		if parsed.User == nil || !strings.Contains(parsed.User.Username(), proxyAccountSentinel) {
			return "", errors.New("{account} 只能用于代理认证用户名")
		}
		return strings.ReplaceAll(parsed.String(), proxyAccountSentinel, ProxyAccountPlaceholder), nil
	}
	return parsed.String(), nil
}

func SanitizeCloudflareCookies(value string) string {
	allowed := make([]string, 0, 4)
	seen := make(map[string]struct{})
	for part := range strings.SplitSeq(value, ";") {
		name, cookieValue, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		lower := strings.ToLower(name)
		if lower != "cf_clearance" && lower != "__cf_bm" && lower != "_cfuvid" && !strings.HasPrefix(lower, "cf_chl_") {
			continue
		}
		if _, exists := seen[lower]; exists {
			continue
		}
		cookieValue = strings.TrimSpace(cookieValue)
		if cookieValue == "" || len(cookieValue) > maxCloudflareCookieBytes || strings.IndexFunc(cookieValue, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
			continue
		}
		seen[lower] = struct{}{}
		allowed = append(allowed, lower+"="+cookieValue)
	}
	return strings.Join(allowed, "; ")
}
