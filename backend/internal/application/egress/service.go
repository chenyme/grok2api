package egress

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrInvalidInput         = errors.New("代理节点参数无效")
	ErrInvalidSort          = errors.New("代理节点排序条件无效")
	ErrNotFound             = errors.New("代理节点不存在")
	ErrClearanceUnavailable = errors.New("Clearance 刷新不可用")
)

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
	ProxyURL          *string
	ClearProxyURL     bool
	UserAgent         string
	CloudflareCookies *string
	ClearCookies      bool
}

type Service struct {
	repository repository.EgressRepository
	cipher     *security.Cipher
	mu         sync.RWMutex
	browserUA  string
	clearance  ClearanceManager
}

type ClearanceManager interface {
	RefreshClearance(context.Context, uint64) error
	ForgetClearance(uint64)
}

func NewService(repository repository.EgressRepository, cipher *security.Cipher, browserUA string) *Service {
	return &Service{repository: repository, cipher: cipher, browserUA: strings.TrimSpace(browserUA)}
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
		string(domain.ScopeWebAsset): s.browserUA,
	}
}

func (s *Service) List(ctx context.Context, scope domain.Scope, sort repository.SortQuery) ([]domain.PublicNode, error) {
	if !repository.IsValidSort(sort, "name", "scope", "proxy", "clearance", "health") {
		return nil, ErrInvalidSort
	}
	values, err := s.repository.ListEgressNodes(ctx, scope, sort)
	if err != nil {
		return nil, err
	}
	result := make([]domain.PublicNode, 0, len(values))
	for _, value := range values {
		result = append(result, s.publicNode(value))
	}
	return result, nil
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
	value, err = s.applyInput(value, input, false)
	if err != nil {
		return domain.PublicNode{}, err
	}
	updated, err := s.repository.UpdateEgressNode(ctx, value)
	if err == nil {
		s.forgetClearance(updated.ID)
	}
	return s.publicNode(updated), err
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	err := s.repository.DeleteEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err == nil {
		s.forgetClearance(id)
	}
	return err
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

func (s *Service) forgetClearance(id uint64) {
	s.mu.RLock()
	manager := s.clearance
	s.mu.RUnlock()
	if manager != nil {
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
	if input.Scope != domain.ScopeBuild && input.Scope != domain.ScopeWeb && input.Scope != domain.ScopeConsole && input.Scope != domain.ScopeWebAsset {
		return domain.Node{}, fmt.Errorf("%w: scope 必须是 grok_build、grok_web、grok_console 或 grok_web_asset", ErrInvalidInput)
	}
	value.Name, value.Scope, value.Enabled, value.ProxyPool = name, input.Scope, input.Enabled, proxyPool
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
	if input.Scope == domain.ScopeBuild {
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
		AccountBoundProxy: accountBoundProxy,
		Health:            health, FailureCount: failureCount, CooldownUntil: cooldownUntil, LastError: lastError,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
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
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return "", errors.New("代理地址格式无效")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
	default:
		return "", errors.New("代理地址协议必须是 HTTP、HTTPS、SOCKS4 或 SOCKS5")
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
