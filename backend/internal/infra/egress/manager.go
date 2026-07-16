package egress

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	application "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
const nodeSnapshotTTL = time.Second
const stickyProxyRetryLimit = 2

type Lease struct {
	NodeID    uint64
	NodeName  string
	Scope     domain.Scope
	ProxyURL  string
	UserAgent string
	CFCookies string
	client    requestClient
	browser   *browserClient
	sticky    bool
	release   func()
}

type requestClient interface {
	Do(*http.Request) (*http.Response, error)
	CloseIdleConnections()
}

func (l *Lease) Do(request *http.Request) (*http.Response, error) {
	if l == nil || l.client == nil {
		return nil, errors.New("出口客户端未初始化")
	}
	return l.do(request)
}
func (l *Lease) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

type Manager struct {
	repository repository.EgressRepository
	cipher     *security.Cipher
	mu         sync.Mutex
	clients    map[clientCacheKey]cachedClient
	inflight   map[uint64]int
	nodes      map[domain.Scope]cachedNodeSnapshot
	nodeLoads  singleflight.Group
}

type cachedClient struct {
	client  requestClient
	browser *browserClient
}

type clientCacheKey struct {
	nodeID      uint64
	scope       domain.Scope
	fingerprint string
}

type cachedNodeSnapshot struct {
	values    []domain.Node
	expiresAt time.Time
}

func NewManager(repository repository.EgressRepository, cipher *security.Cipher) *Manager {
	return &Manager{repository: repository, cipher: cipher, clients: make(map[clientCacheKey]cachedClient), inflight: make(map[uint64]int), nodes: make(map[domain.Scope]cachedNodeSnapshot)}
}

func (m *Manager) Acquire(ctx context.Context, scope domain.Scope, affinity string) (*Lease, error) {
	lease, _, err := m.acquire(ctx, scope, affinity, true, "")
	return lease, err
}

// AcquireCredential binds the outbound proxy identity to one persisted
// Provider credential. Resin templates use this identity as their Account.
func (m *Manager) AcquireCredential(ctx context.Context, scope domain.Scope, credential accountdomain.Credential) (*Lease, error) {
	identity := string(credential.Provider) + "_" + strconv.FormatUint(credential.ID, 10)
	credentialCookies := ""
	if scope != domain.ScopeBuild && strings.TrimSpace(credential.EncryptedCloudflareCookie) != "" {
		cookies, decryptErr := m.cipher.Decrypt(credential.EncryptedCloudflareCookie)
		if decryptErr != nil {
			return nil, decryptErr
		}
		credentialCookies = application.SanitizeCloudflareCookies(cookies)
	}
	// Web and Console accounts can be two database projections of the same SSO
	// login.  Resin must see one stable account identity across both channels;
	// otherwise the proxy rotates the IP while the clearance remains bound to
	// the other lease.  The digest is non-reversible and is only used as a proxy
	// template account label.
	if credential.AuthType == accountdomain.AuthTypeSSO && strings.TrimSpace(credential.EncryptedAccessToken) != "" {
		token, decryptErr := m.cipher.Decrypt(credential.EncryptedAccessToken)
		if decryptErr != nil {
			return nil, decryptErr
		}
		identity = "sso_" + security.HashToken(token)[:32]
	}
	ctx = WithAccountIdentity(ctx, identity)
	lease, _, err := m.acquire(ctx, scope, strconv.FormatUint(credential.ID, 10), true, credentialCookies)
	return lease, err
}

func (m *Manager) AcquireIfConfigured(ctx context.Context, scope domain.Scope, affinity string) (*Lease, bool, error) {
	return m.acquire(ctx, scope, affinity, false, "")
}

func (m *Manager) acquire(ctx context.Context, scope domain.Scope, affinity string, allowDirect bool, credentialCookies string) (*Lease, bool, error) {
	now := time.Now().UTC()
	configured := false
	var available []domain.Node
	for _, candidateScope := range fallbackScopes(scope) {
		nodes, err := m.listNodes(ctx, candidateScope, now)
		if err != nil {
			return nil, false, err
		}
		configured = configured || len(nodes) > 0
		candidateAvailable := make([]domain.Node, 0, len(nodes))
		for _, node := range nodes {
			if node.Enabled && (node.CooldownUntil == nil || !now.Before(*node.CooldownUntil)) {
				candidateAvailable = append(candidateAvailable, node)
			}
		}
		if len(candidateAvailable) > 0 {
			available = candidateAvailable
			break
		}
	}
	if len(available) == 0 {
		if configured {
			return nil, false, fmt.Errorf("当前没有可用的 %s 出口节点", scope)
		}
		if !allowDirect {
			recordSelection(ctx, Selection{NodeName: "direct", Scope: scope})
			return nil, false, nil
		}
		available = []domain.Node{{ID: 0, Name: "direct", Scope: scope, Enabled: true, Health: 1}}
	}
	sort.SliceStable(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	selected := m.selectNode(available, affinity)
	proxyURL, err := m.cipher.Decrypt(selected.EncryptedProxyURL)
	if err != nil {
		return nil, false, err
	}
	proxyURL, err = application.NormalizeProxyURL(proxyURL)
	if err != nil {
		return nil, false, err
	}
	sticky := strings.Contains(proxyURL, application.ProxyAccountPlaceholder)
	if sticky {
		accountKey := accountFromContext(ctx)
		if accountKey == "" && strings.TrimSpace(affinity) != "" {
			accountKey = string(scope) + "_" + strings.TrimSpace(affinity)
		}
		proxyURL, err = renderAccountProxyURL(proxyURL, accountKey)
		if err != nil {
			return nil, false, err
		}
	}
	cookies := ""
	if scope != domain.ScopeBuild {
		cookies, err = m.cipher.Decrypt(selected.EncryptedCloudflareCookie)
		if err != nil {
			return nil, false, err
		}
		cookies = application.SanitizeCloudflareCookies(cookies)
		if credentialCookies != "" {
			cookies = credentialCookies
		}
	}
	userAgent := ""
	if scope != domain.ScopeBuild {
		userAgent = strings.TrimSpace(selected.UserAgent)
	}
	if scope != domain.ScopeBuild && userAgent == "" {
		userAgent = DefaultUserAgent
	}
	client, err := m.clientFor(selected.ID, scope, proxyURL, userAgent, cookies, sticky)
	if err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	m.inflight[selected.ID]++
	m.mu.Unlock()
	recordSelection(ctx, Selection{NodeID: selected.ID, NodeName: selected.Name, Scope: scope, Proxied: proxyURL != ""})
	var once sync.Once
	return &Lease{NodeID: selected.ID, NodeName: selected.Name, Scope: scope, ProxyURL: proxyURL, UserAgent: userAgent, CFCookies: cookies, client: client.client, browser: client.browser, sticky: sticky, release: func() {
		once.Do(func() {
			m.mu.Lock()
			m.inflight[selected.ID]--
			if m.inflight[selected.ID] <= 0 {
				delete(m.inflight, selected.ID)
			}
			m.mu.Unlock()
		})
	}}, true, nil
}

func renderAccountProxyURL(template, accountKey string) (string, error) {
	if !strings.Contains(template, application.ProxyAccountPlaceholder) {
		return template, nil
	}
	accountKey = normalizeProxyAccount(accountKey)
	if accountKey == "" {
		return "", errors.New("粘性代理需要有效的账号身份")
	}
	return strings.ReplaceAll(template, application.ProxyAccountPlaceholder, accountKey), nil
}

func normalizeProxyAccount(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.Map(func(character rune) rune {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' {
			return character
		}
		return '_'
	}, value)
	if len(value) <= 128 {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return value[:95] + "_" + fmt.Sprintf("%x", digest[:16])
}

func (m *Manager) listNodes(ctx context.Context, scope domain.Scope, now time.Time) ([]domain.Node, error) {
	m.mu.Lock()
	if snapshot, ok := m.nodes[scope]; ok && now.Before(snapshot.expiresAt) {
		values := append([]domain.Node(nil), snapshot.values...)
		m.mu.Unlock()
		return values, nil
	}
	m.mu.Unlock()
	loaded, err, _ := m.nodeLoads.Do(string(scope), func() (any, error) {
		checkTime := time.Now().UTC()
		m.mu.Lock()
		if snapshot, ok := m.nodes[scope]; ok && checkTime.Before(snapshot.expiresAt) {
			values := append([]domain.Node(nil), snapshot.values...)
			m.mu.Unlock()
			return values, nil
		}
		m.mu.Unlock()
		values, err := m.repository.ListEgressNodes(ctx, scope, repository.SortQuery{})
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.nodes[scope] = cachedNodeSnapshot{values: append([]domain.Node(nil), values...), expiresAt: checkTime.Add(nodeSnapshotTTL)}
		m.mu.Unlock()
		return values, nil
	})
	if err != nil {
		return nil, err
	}
	return append([]domain.Node(nil), loaded.([]domain.Node)...), nil
}

func (m *Manager) invalidateNodes(scope domain.Scope) {
	m.mu.Lock()
	delete(m.nodes, scope)
	m.mu.Unlock()
}

func fallbackScopes(scope domain.Scope) []domain.Scope {
	if scope == domain.ScopeWebAsset {
		return []domain.Scope{domain.ScopeWebAsset, domain.ScopeWeb}
	}
	if scope == domain.ScopeConsole {
		// Console uses the same browser/clearance surface as Grok Web.  A
		// dedicated Console node is preferred, but a Web node is a safe and
		// expected fallback for deployments that configure one shared pool.
		return []domain.Scope{domain.ScopeConsole, domain.ScopeWeb}
	}
	return []domain.Scope{scope}
}

func (m *Manager) selectNode(nodes []domain.Node, affinity string) domain.Node {
	if affinity != "" {
		digest := sha256.Sum256([]byte(affinity))
		selected := nodes[int(binary.BigEndian.Uint64(digest[:8])%uint64(len(nodes)))]
		if selected.Health >= 0.8 || len(nodes) == 1 {
			return selected
		}
		for _, node := range nodes {
			if node.Health > selected.Health {
				selected = node
			}
		}
		return selected
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	best := nodes[0]
	for _, node := range nodes[1:] {
		if m.inflight[node.ID] < m.inflight[best.ID] || (m.inflight[node.ID] == m.inflight[best.ID] && node.Health > best.Health) {
			best = node
		}
	}
	return best
}

func (m *Manager) clientFor(id uint64, scope domain.Scope, proxyURL, userAgent, cookies string, sticky bool) (cachedClient, error) {
	clientKind := "browser"
	if scope == domain.ScopeBuild {
		clientKind = "build"
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(clientKind+"\x00"+proxyURL+"\x00"+userAgent+"\x00"+cookies)))
	cacheScope := scope
	if cacheScope == domain.ScopeWebAsset {
		cacheScope = domain.ScopeWeb
	}
	key := clientCacheKey{nodeID: id, scope: cacheScope, fingerprint: fingerprint}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cached, ok := m.clients[key]; ok {
		return cached, nil
	}
	var value cachedClient
	if scope == domain.ScopeBuild {
		client, err := newBuildClient(proxyURL)
		if err != nil {
			return cachedClient{}, err
		}
		value.client = client
	} else {
		client, err := newBrowserClient(proxyURL)
		if err != nil {
			return cachedClient{}, err
		}
		value.client = client
		value.browser = client
	}
	// 固定代理同节点出现新指纹说明配置已更新，旧连接池应淘汰。
	// 账号模板代理的指纹会随 Resin Account 变化，必须并存才能维持各账号的粘性连接池。
	// 直连节点统一使用 ID 0，不同 Provider 的传输必须并存，避免 Build 与 Web 互相重建客户端。
	if id != 0 && !sticky {
		for previousKey, previous := range m.clients {
			if previousKey.nodeID != id {
				continue
			}
			if previous.client != nil {
				previous.client.CloseIdleConnections()
			}
			delete(m.clients, previousKey)
		}
	}
	m.clients[key] = value
	return value, nil
}

func (m *Manager) Feedback(ctx context.Context, nodeID uint64, status int, transportErr error) {
	m.FeedbackForScope(ctx, domain.ScopeWeb, nodeID, status, transportErr)
}

func (m *Manager) FeedbackForScope(ctx context.Context, scope domain.Scope, nodeID uint64, status int, transportErr error) {
	if nodeID == 0 {
		if transportErr != nil || status >= 500 || (scope != domain.ScopeBuild && status == http.StatusForbidden) {
			m.mu.Lock()
			m.invalidateClientForScopeLocked(0, scope)
			m.mu.Unlock()
		}
		return
	}
	value, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	switch {
	case transportErr == nil && status >= 200 && status < 400:
		value.Health = min(1, value.Health+0.1)
		value.FailureCount = 0
		value.CooldownUntil = nil
		value.LastError = ""
	case status == http.StatusUnauthorized || status == http.StatusTooManyRequests:
		return
	case scope == domain.ScopeBuild && status == http.StatusForbidden:
		// Build 403 可能是账号权限、额度、Token 或出口策略，响应体由网关层
		// 分类；仅凭状态码不能把标准 CLI 出口误判为 Web anti-bot。
		return
	case status == http.StatusForbidden:
		if m.isStickyProxyNode(value) {
			// A 403 on an account-bound Resin lease usually means that account's
			// clearance is stale. Do not cool or invalidate the shared node for
			// unrelated accounts.
			return
		}
		value.FailureCount++
		value.Health = max(0.05, value.Health*0.7)
		value.CooldownUntil = nil
		value.LastError = "anti-bot rejection"
		m.mu.Lock()
		m.invalidateClientLocked(nodeID)
		m.mu.Unlock()
	default:
		value.FailureCount++
		value.Health = max(0.05, value.Health*0.7)
		cooldown := min(10*time.Minute, 30*time.Second*time.Duration(1<<min(value.FailureCount-1, 4)))
		until := now.Add(cooldown)
		value.CooldownUntil = &until
		if transportErr != nil {
			value.LastError = "transport error"
		} else {
			value.LastError = fmt.Sprintf("upstream status %d", status)
		}
		m.mu.Lock()
		m.invalidateClientLocked(nodeID)
		m.mu.Unlock()
	}
	if _, err := m.repository.UpdateEgressNode(ctx, value); err == nil {
		m.invalidateNodes(value.Scope)
	}
}

func (m *Manager) isStickyProxyNode(value domain.Node) bool {
	if m == nil || m.cipher == nil || strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return false
	}
	proxyURL, err := m.cipher.Decrypt(value.EncryptedProxyURL)
	return err == nil && strings.Contains(proxyURL, application.ProxyAccountPlaceholder)
}

func (m *Manager) invalidateClientLocked(nodeID uint64) {
	for key, cached := range m.clients {
		if key.nodeID != nodeID {
			continue
		}
		if cached.client != nil {
			cached.client.CloseIdleConnections()
		}
		delete(m.clients, key)
	}
}

func (m *Manager) invalidateClientForScopeLocked(nodeID uint64, scope domain.Scope) {
	if scope == domain.ScopeWebAsset {
		scope = domain.ScopeWeb
	}
	for key, cached := range m.clients {
		if key.nodeID != nodeID || key.scope != scope {
			continue
		}
		if cached.client != nil {
			cached.client.CloseIdleConnections()
		}
		delete(m.clients, key)
	}
}

func BuildSSOCookie(token, cloudflareCookies string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(token), "sso=") {
		token = strings.TrimSpace(token[len("sso="):])
	}
	if value, _, found := strings.Cut(token, ";"); found {
		token = strings.TrimSpace(value)
	}
	token = strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(token)
	cookies := "sso=" + token + "; sso-rw=" + token
	if sanitized := application.SanitizeCloudflareCookies(cloudflareCookies); sanitized != "" {
		cookies += "; " + sanitized
	}
	return cookies
}
