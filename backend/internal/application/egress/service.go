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
	ErrInvalidInput = errors.New("代理节点参数无效")
	ErrInvalidSort  = errors.New("代理节点排序条件无效")
	ErrNotFound     = errors.New("代理节点不存在")
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
	webUA      string
	consoleUA  string
}

func NewService(repository repository.EgressRepository, cipher *security.Cipher, webUA, consoleUA string) *Service {
	return &Service{repository: repository, cipher: cipher, webUA: strings.TrimSpace(webUA), consoleUA: strings.TrimSpace(consoleUA)}
}

func (s *Service) UpdateDefaults(webUA, consoleUA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webUA = strings.TrimSpace(webUA)
	s.consoleUA = strings.TrimSpace(consoleUA)
}

func (s *Service) DefaultUserAgents() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]string{
		string(domain.ScopeBuild): "", string(domain.ScopeWeb): s.webUA, string(domain.ScopeConsole): s.consoleUA,
		string(domain.ScopeWebAsset): s.webUA,
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
		result = append(result, publicNode(value))
	}
	return result, nil
}

func (s *Service) Create(ctx context.Context, input Input) (domain.PublicNode, error) {
	value, err := s.applyInput(domain.Node{}, input, true)
	if err != nil {
		return domain.PublicNode{}, err
	}
	created, err := s.repository.CreateEgressNode(ctx, value)
	return publicNode(created), err
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
	return publicNode(updated), err
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	err := s.repository.DeleteEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func (s *Service) applyInput(value domain.Node, input Input, create bool) (domain.Node, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 160 {
		return domain.Node{}, fmt.Errorf("%w: 名称必须在 1 到 160 个字符之间", ErrInvalidInput)
	}
	if input.Scope != domain.ScopeBuild && input.Scope != domain.ScopeWeb && input.Scope != domain.ScopeConsole && input.Scope != domain.ScopeWebAsset {
		return domain.Node{}, fmt.Errorf("%w: scope 必须是 grok_build、grok_web、grok_console 或 grok_web_asset", ErrInvalidInput)
	}
	value.Name, value.Scope, value.Enabled = name, input.Scope, input.Enabled
	if input.Scope == domain.ScopeBuild {
		// Build 请求始终沿用 Provider 生成的 CLI User-Agent，出口节点不得覆盖协议身份。
		value.UserAgent = ""
	} else {
		value.UserAgent = strings.TrimSpace(input.UserAgent)
	}
	if input.Scope != domain.ScopeBuild && value.UserAgent == "" {
		s.mu.RLock()
		value.UserAgent = s.defaultUserAgent(input.Scope)
		s.mu.RUnlock()
	}
	if len(value.UserAgent) > 512 {
		return domain.Node{}, fmt.Errorf("%w: User-Agent 过长", ErrInvalidInput)
	}
	if input.ClearProxyURL {
		value.EncryptedProxyURL = ""
	} else if input.ProxyURL != nil {
		normalized, err := NormalizeProxyURL(*input.ProxyURL)
		if err != nil {
			return domain.Node{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		if normalized != "" || create {
			value.EncryptedProxyURL, err = s.cipher.Encrypt(normalized)
			if err != nil {
				return domain.Node{}, err
			}
		}
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
	if create {
		value.Health = 1
	}
	return value, nil
}

func (s *Service) defaultUserAgent(scope domain.Scope) string {
	if scope == domain.ScopeConsole {
		return s.consoleUA
	}
	return s.webUA
}

func publicNode(value domain.Node) domain.PublicNode {
	userAgent := value.UserAgent
	if value.Scope == domain.ScopeBuild {
		userAgent = ""
	}
	return domain.PublicNode{
		ID: value.ID, Name: value.Name, Scope: value.Scope, Enabled: value.Enabled,
		ProxyConfigured: value.EncryptedProxyURL != "", UserAgent: userAgent, CookieConfigured: value.EncryptedCloudflareCookie != "",
		Health: value.Health, FailureCount: value.FailureCount, CooldownUntil: value.CooldownUntil, LastError: value.LastError,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
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
