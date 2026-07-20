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
const clientCacheIdleTTL = 30 * time.Minute
const clientCacheCleanupInterval = time.Minute
const maxCachedClients = 4096
const clearanceLockGrace = 30 * time.Second
const clearanceCacheCleanupInterval = time.Minute
const clearanceCacheMinIdleTTL = 30 * time.Minute
const maxCachedClearances = 16384
const clearanceCacheEvictionBatch = 256

type Lease struct {
	NodeID           uint64
	NodeName         string
	Scope            domain.Scope
	ProxyURL         string
	UserAgent        string
	CFCookies        string
	client           requestClient
	browser          *browserClient
	sticky           bool
	clearanceKey     string
	clearanceManager *Manager
	release          func()
}

type requestClient interface {
	Do(*http.Request) (*http.Response, error)
	CloseIdleConnections()
}

func (l *Lease) Do(request *http.Request) (*http.Response, error) {
	if l == nil || l.client == nil {
		return nil, errors.New("出口客户端未初始化")
	}
	response, err := l.do(request)
	if err == nil && response != nil && response.StatusCode == http.StatusForbidden && l.clearanceManager != nil && l.clearanceKey != "" {
		l.clearanceManager.invalidateClearanceKey(l.clearanceKey, l.client)
	}
	return response, err
}
func (l *Lease) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

type Manager struct {
	repository           repository.EgressRepository
	cipher               *security.Cipher
	mu                   sync.Mutex
	clients              map[clientCacheKey]cachedClient
	inflight             map[uint64]int
	nodes                map[domain.Scope]cachedNodeSnapshot
	nodeLoads            singleflight.Group
	lastClientCleanup    time.Time
	clearanceLoads       singleflight.Group
	clearanceConfig      ClearanceConfig
	clearanceVersion     uint64
	clearances           map[string]clearanceState
	lastClearanceCleanup time.Time
	solver               clearanceSolver
	clearanceLock        repository.DistributedLock
}

type clearanceState struct {
	cookies            string
	userAgent          string
	refreshedAt        time.Time
	invalid            bool
	used               bool
	version            uint64
	fingerprint        string
	bindingFingerprint string
	lastUsedAt         time.Time
}

type egressStateRepository interface {
	UpdateEgressNodeClearance(context.Context, uint64, string, string, string, string, time.Time) error
	UpdateEgressNodeHealth(context.Context, uint64, float64, int, *time.Time, string) error
	UpdateEgressNodeLastError(context.Context, uint64, string) error
}

type cachedClient struct {
	client   requestClient
	browser  *browserClient
	lastUsed time.Time
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
	return &Manager{
		repository: repository, cipher: cipher,
		clients: make(map[clientCacheKey]cachedClient), inflight: make(map[uint64]int),
		nodes: make(map[domain.Scope]cachedNodeSnapshot), clearances: make(map[string]clearanceState),
		solver:          flaresolverrSolver{},
		clearanceConfig: ClearanceConfig{Mode: "manual", TargetURL: "https://grok.com", Timeout: time.Minute, RefreshInterval: 10 * time.Minute},
	}
}

// SetClearanceLock enables cross-instance coordination for shared, fixed egress
// nodes. Account-bound Resin clearances remain process-local because they must
// never be persisted into the node-wide cookie fields.
func (m *Manager) SetClearanceLock(value repository.DistributedLock) {
	m.mu.Lock()
	m.clearanceLock = value
	m.mu.Unlock()
}

func (m *Manager) UpdateClearanceConfig(value ClearanceConfig) {
	value.Mode = strings.TrimSpace(value.Mode)
	value.FlareSolverrURL = strings.TrimSpace(value.FlareSolverrURL)
	value.TargetURL = strings.TrimRight(strings.TrimSpace(value.TargetURL), "/")
	m.mu.Lock()
	previous := m.clearanceConfig
	m.clearanceConfig = value
	configurationChanged := previous.Mode != value.Mode || previous.FlareSolverrURL != value.FlareSolverrURL || previous.TargetURL != value.TargetURL
	if configurationChanged {
		m.clearanceVersion++
	}
	m.mu.Unlock()
}

func (m *Manager) Acquire(ctx context.Context, scope domain.Scope, affinity string) (*Lease, error) {
	lease, _, err := m.acquire(ctx, scope, affinity, true, "")
	return lease, err
}

// AcquireCredential binds the outbound proxy identity to one persisted
// Provider credential. Resin templates use this identity as their Account.
func (m *Manager) AcquireCredential(ctx context.Context, scope domain.Scope, credential accountdomain.Credential) (*Lease, error) {
	identity := strings.TrimSpace(credential.EgressIdentity)
	if identity == "" {
		identity = string(credential.Provider) + "_" + strconv.FormatUint(credential.ID, 10)
	}
	// Web and Console accounts can be two database projections of the same SSO
	// login.  Resin must see one stable account identity across both channels;
	// otherwise the proxy rotates the IP while the clearance remains bound to
	// the other lease.  The digest is non-reversible and is only used as a proxy
	// template account label.
	if strings.TrimSpace(credential.EgressIdentity) == "" && credential.AuthType == accountdomain.AuthTypeSSO && strings.TrimSpace(credential.EncryptedAccessToken) != "" {
		token, decryptErr := m.cipher.Decrypt(credential.EncryptedAccessToken)
		if decryptErr != nil {
			return nil, decryptErr
		}
		identity = "sso_" + security.HashToken(token)[:32]
	}
	ctx = WithAccountIdentity(ctx, identity)
	lease, _, err := m.acquire(ctx, scope, strconv.FormatUint(credential.ID, 10), true, credential.EncryptedCloudflareCookie)
	return lease, err
}

func (m *Manager) AcquireIfConfigured(ctx context.Context, scope domain.Scope, affinity string) (*Lease, bool, error) {
	return m.acquire(ctx, scope, affinity, false, "")
}

func (m *Manager) acquire(ctx context.Context, scope domain.Scope, affinity string, allowDirect bool, encryptedCredentialCookies string) (*Lease, bool, error) {
	now := time.Now().UTC()
	managedClearance := isGrokWebScope(scope) && m.clearanceMode() == "flaresolverr"
	credentialCookies := ""
	if !managedClearance && scope != domain.ScopeBuild && strings.TrimSpace(encryptedCredentialCookies) != "" {
		decryptedCookies, decryptErr := m.cipher.Decrypt(encryptedCredentialCookies)
		if decryptErr != nil {
			return nil, false, decryptErr
		}
		credentialCookies = application.SanitizeCloudflareCookies(decryptedCookies)
	}
	configured := false
	var available []domain.Node
	for _, candidateScope := range fallbackScopes(scope) {
		nodes, err := m.listNodes(ctx, candidateScope, now)
		if err != nil {
			return nil, false, err
		}
		candidateAvailable := make([]domain.Node, 0, len(nodes))
		for _, node := range nodes {
			if !node.Enabled {
				continue
			}
			configured = true
			if node.CooldownUntil == nil || !now.Before(*node.CooldownUntil) {
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
			// Managed mode can recover a damaged persisted cookie by asking the
			// solver for a fresh one. Manual mode must still surface the storage
			// error because it has no safe replacement source.
			if !managedClearance {
				return nil, false, err
			}
			cookies = ""
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
	clearanceKey := ""
	// Manual mode may prefer account-bound cookies. Managed mode always enters
	// the FlareSolverr lifecycle so stale imported cookies cannot bypass refresh.
	if managedClearance {
		clearanceKey = clearanceCacheKey(selected.ID, proxyURL, sticky)
		cookies, userAgent, err = m.ensureClearance(ctx, selected, proxyURL, cookies, userAgent, clearanceKey, !sticky)
		if err != nil {
			return nil, false, err
		}
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
	return &Lease{NodeID: selected.ID, NodeName: selected.Name, Scope: scope, ProxyURL: proxyURL, UserAgent: userAgent, CFCookies: cookies, client: client.client, browser: client.browser, sticky: sticky, clearanceKey: clearanceKey, clearanceManager: m, release: func() {
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

func clearanceCacheKey(nodeID uint64, proxyURL string, sticky bool) string {
	if nodeID == 0 {
		return "direct"
	}
	base := "node:" + strconv.FormatUint(nodeID, 10)
	if !sticky {
		return base
	}
	digest := sha256.Sum256([]byte(proxyURL))
	return base + ":account:" + fmt.Sprintf("%x", digest[:16])
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
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupClientCacheLocked(now)
	if cached, ok := m.clients[key]; ok {
		cached.lastUsed = now
		m.clients[key] = cached
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
		client, err := newBrowserClient(proxyURL, userAgent)
		if err != nil {
			return cachedClient{}, err
		}
		value.client = client
		value.browser = client
	}
	value.lastUsed = now
	// 固定代理同节点出现新指纹说明配置已更新，旧连接池应淘汰。
	// 账号模板代理的指纹会随 Resin Account 变化，必须并存才能维持各账号的粘性连接池。
	// 直连节点统一使用 ID 0，不同 Provider 的传输必须并存，避免 Build 与 Web 互相重建客户端。
	if id != 0 && !sticky {
		for previousKey, previous := range m.clients {
			if previousKey.nodeID != id {
				continue
			}
			m.evictClientLocked(previousKey, previous)
		}
	}
	m.ensureClientCacheCapacityLocked()
	m.clients[key] = value
	return value, nil
}

func (m *Manager) cleanupClientCacheLocked(now time.Time) {
	if m.clients == nil {
		m.clients = make(map[clientCacheKey]cachedClient)
	}
	if !m.lastClientCleanup.IsZero() && now.Sub(m.lastClientCleanup) < clientCacheCleanupInterval {
		return
	}
	m.lastClientCleanup = now
	for key, value := range m.clients {
		if !value.lastUsed.IsZero() && now.Sub(value.lastUsed) >= clientCacheIdleTTL {
			m.evictClientLocked(key, value)
		}
	}
}

func (m *Manager) ensureClientCacheCapacityLocked() {
	for len(m.clients) >= maxCachedClients {
		var oldestKey clientCacheKey
		var oldest cachedClient
		found := false
		for key, value := range m.clients {
			if !found || value.lastUsed.Before(oldest.lastUsed) {
				oldestKey, oldest, found = key, value, true
			}
		}
		if !found {
			break
		}
		m.evictClientLocked(oldestKey, oldest)
	}
}

func (m *Manager) evictClientLocked(key clientCacheKey, value cachedClient) {
	if value.client != nil {
		value.client.CloseIdleConnections()
	}
	delete(m.clients, key)
}

func (m *Manager) Feedback(ctx context.Context, nodeID uint64, status int, transportErr error) {
	m.FeedbackForScope(ctx, domain.ScopeWeb, nodeID, status, transportErr)
}

func (m *Manager) FeedbackForScope(ctx context.Context, scope domain.Scope, nodeID uint64, status int, transportErr error) {
	if nodeID == 0 {
		if transportErr != nil || status >= 500 || (scope != domain.ScopeBuild && status == http.StatusForbidden) {
			m.mu.Lock()
			if isGrokWebScope(scope) && status == http.StatusForbidden && m.clearanceConfig.Mode == "flaresolverr" {
				state := m.clearances["direct"]
				state.invalid = true
				state.used = true
				m.clearances["direct"] = state
			}
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
	case scope == domain.ScopeBuild && status == http.StatusBadRequest:
		// Device OAuth 在用户确认前会以 400 + authorization_pending 轮询，
		// 这是协议正常状态，不能当作出口节点故障进入冷却。
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
		if isGrokWebScope(scope) && m.clearanceConfig.Mode == "flaresolverr" {
			key := clearanceCacheKey(nodeID, "", false)
			state := m.clearances[key]
			state.invalid = true
			state.used = true
			m.clearances[key] = state
		}
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
	if stateRepository, ok := m.repository.(egressStateRepository); ok {
		if err := stateRepository.UpdateEgressNodeHealth(ctx, value.ID, value.Health, value.FailureCount, value.CooldownUntil, value.LastError); err == nil {
			m.invalidateNodes(value.Scope)
		}
		return
	}
	if _, err := m.repository.UpdateEgressNode(ctx, value); err == nil {
		m.invalidateNodes(value.Scope)
	}
}

func (m *Manager) clearanceMode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clearanceConfig.Mode
}

func (m *Manager) ensureClearance(ctx context.Context, node domain.Node, proxyURL, existingCookies, existingUserAgent, key string, persist bool) (string, string, error) {
	m.mu.Lock()
	cfg := m.clearanceConfig
	version := m.clearanceVersion
	interval := clearanceRefreshInterval(cfg)
	now := time.Now().UTC()
	fingerprint := clearanceFingerprint(cfg, proxyURL)
	bindingFingerprint := clearanceBindingFingerprint(cfg, proxyURL)
	m.cleanupClearanceCacheLocked(now, interval)
	state, known := m.clearances[key]
	if key == "direct" {
		if !known {
			m.ensureClearanceCacheCapacityLocked()
		}
		state.used = true
		m.clearances[key] = state
	}
	if (!known || state.cookies == "") && persist && existingCookies != "" {
		if !known {
			m.ensureClearanceCacheCapacityLocked()
		}
		state = clearanceState{
			cookies: existingCookies, userAgent: existingUserAgent, used: true, version: version,
			fingerprint: node.ClearanceFingerprint, bindingFingerprint: node.ClearanceBindingFingerprint,
			lastUsedAt: now,
		}
		if node.ClearanceRefreshedAt != nil {
			state.refreshedAt = *node.ClearanceRefreshedAt
		}
		known = true
		m.clearances[key] = state
	}
	fresh := known && !state.invalid && state.cookies != "" && state.version == version &&
		state.fingerprint == fingerprint && (state.bindingFingerprint == "" || state.bindingFingerprint == bindingFingerprint) &&
		!state.refreshedAt.IsZero() && now.Sub(state.refreshedAt) < interval
	if fresh {
		state.lastUsedAt = now
		m.clearances[key] = state
		cookies, userAgent := state.cookies, state.userAgent
		m.mu.Unlock()
		return cookies, userAgent, nil
	}
	fallbackAllowed := known && !state.invalid && state.cookies != "" &&
		(state.bindingFingerprint == "" || state.bindingFingerprint == bindingFingerprint)
	fallback := clearanceSolution{Cookies: state.cookies, UserAgent: state.userAgent}
	if fallbackAllowed {
		state.lastUsedAt = now
		m.clearances[key] = state
	}
	if cfg.Mode != "flaresolverr" {
		m.mu.Unlock()
		return existingCookies, existingUserAgent, nil
	}
	m.mu.Unlock()

	result, err, _ := m.clearanceLoads.Do(key, func() (any, error) {
		return m.refreshNode(ctx, node, proxyURL, key, persist, false, !fallbackAllowed)
	})
	if err != nil {
		if fallbackAllowed {
			return fallback.Cookies, fallback.UserAgent, nil
		}
		return "", "", err
	}
	solution := result.(clearanceSolution)
	return solution.Cookies, solution.UserAgent, nil
}

func (m *Manager) refreshNode(ctx context.Context, node domain.Node, proxyURL, key string, persist, force, waitForPeer bool) (clearanceSolution, error) {
	m.mu.Lock()
	cfg := m.clearanceConfig
	solveVersion := m.clearanceVersion
	solver := m.solver
	lock := m.clearanceLock
	m.mu.Unlock()
	if cfg.Mode != "flaresolverr" {
		return clearanceSolution{}, errors.New("FlareSolverr Clearance 未启用")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	fingerprint := clearanceFingerprint(cfg, proxyURL)
	bindingFingerprint := clearanceBindingFingerprint(cfg, proxyURL)
	interval := clearanceRefreshInterval(cfg)
	if persist && node.ID != 0 && lock != nil {
		release, acquired, err := lock.Acquire(ctx, "egress-clearance:"+strconv.FormatUint(node.ID, 10), timeout+clearanceLockGrace)
		if err != nil {
			return clearanceSolution{}, fmt.Errorf("协调 Clearance 刷新: %w", err)
		}
		if !acquired {
			if solution, refreshedAt, ok := m.loadPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval); ok {
				m.cacheClearance(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
				return solution, nil
			}
			if waitForPeer {
				if solution, refreshedAt, ok := m.waitPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval, timeout); ok {
					m.cacheClearance(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
					return solution, nil
				}
			}
			return clearanceSolution{}, errors.New("另一个实例正在刷新 Cloudflare Clearance")
		}
		defer release()
		if !force {
			if solution, refreshedAt, ok := m.loadPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval); ok {
				m.cacheClearance(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
				return solution, nil
			}
		}
	}
	solveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	solution, err := solver.Solve(solveCtx, cfg, proxyURL)
	if err != nil {
		m.recordClearanceError(ctx, node, persist)
		return clearanceSolution{}, fmt.Errorf("刷新出口 %q 的 Cloudflare Clearance: %w", node.Name, err)
	}
	now := time.Now().UTC()
	if persist && node.ID != 0 {
		encryptedCookies, encryptErr := m.cipher.Encrypt(solution.Cookies)
		if encryptErr != nil {
			return clearanceSolution{}, encryptErr
		}
		if stateRepository, ok := m.repository.(egressStateRepository); ok {
			if updateErr := stateRepository.UpdateEgressNodeClearance(ctx, node.ID, encryptedCookies, solution.UserAgent, fingerprint, bindingFingerprint, now); updateErr != nil {
				return clearanceSolution{}, updateErr
			}
		} else {
			latest, loadErr := m.repository.GetEgressNode(ctx, node.ID)
			if loadErr != nil {
				return clearanceSolution{}, loadErr
			}
			latest.EncryptedCloudflareCookie = encryptedCookies
			latest.UserAgent = solution.UserAgent
			latest.ClearanceFingerprint = fingerprint
			latest.ClearanceBindingFingerprint = bindingFingerprint
			latest.ClearanceRefreshedAt = &now
			latest.LastError = ""
			if _, updateErr := m.repository.UpdateEgressNode(ctx, latest); updateErr != nil {
				return clearanceSolution{}, updateErr
			}
		}
		m.invalidateNodes(node.Scope)
	}
	m.cacheClearance(key, solution, now, solveVersion, fingerprint, bindingFingerprint, interval)
	return solution, nil
}

func (m *Manager) waitPersistedClearance(ctx context.Context, nodeID uint64, fingerprint, bindingFingerprint string, interval, timeout time.Duration) (clearanceSolution, time.Time, bool) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return clearanceSolution{}, time.Time{}, false
		case <-ticker.C:
			if solution, refreshedAt, ok := m.loadPersistedClearance(waitCtx, nodeID, fingerprint, bindingFingerprint, interval); ok {
				return solution, refreshedAt, true
			}
		}
	}
}

func (m *Manager) loadPersistedClearance(ctx context.Context, nodeID uint64, fingerprint, bindingFingerprint string, interval time.Duration) (clearanceSolution, time.Time, bool) {
	latest, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil || latest.ClearanceRefreshedAt == nil || latest.ClearanceFingerprint != fingerprint ||
		(latest.ClearanceBindingFingerprint != "" && latest.ClearanceBindingFingerprint != bindingFingerprint) ||
		time.Since(*latest.ClearanceRefreshedAt) >= interval || strings.TrimSpace(latest.EncryptedCloudflareCookie) == "" {
		return clearanceSolution{}, time.Time{}, false
	}
	cookies, err := m.cipher.Decrypt(latest.EncryptedCloudflareCookie)
	if err != nil {
		return clearanceSolution{}, time.Time{}, false
	}
	cookies = application.SanitizeCloudflareCookies(cookies)
	userAgent := strings.TrimSpace(latest.UserAgent)
	if cookies == "" || userAgent == "" {
		return clearanceSolution{}, time.Time{}, false
	}
	return clearanceSolution{Cookies: cookies, UserAgent: userAgent}, *latest.ClearanceRefreshedAt, true
}

func (m *Manager) cacheClearance(key string, solution clearanceSolution, refreshedAt time.Time, version uint64, fingerprint, bindingFingerprint string, interval time.Duration) {
	m.mu.Lock()
	now := time.Now().UTC()
	m.cleanupClearanceCacheLocked(now, interval)
	if _, exists := m.clearances[key]; !exists {
		m.ensureClearanceCacheCapacityLocked()
	}
	m.clearances[key] = clearanceState{
		cookies: solution.Cookies, userAgent: solution.UserAgent, refreshedAt: refreshedAt,
		used: true, version: version, fingerprint: fingerprint, bindingFingerprint: bindingFingerprint, lastUsedAt: now,
	}
	m.mu.Unlock()
}

func (m *Manager) cleanupClearanceCacheLocked(now time.Time, interval time.Duration) {
	if m.clearances == nil {
		m.clearances = make(map[string]clearanceState)
	}
	if !m.lastClearanceCleanup.IsZero() && now.Sub(m.lastClearanceCleanup) < clearanceCacheCleanupInterval {
		return
	}
	m.lastClearanceCleanup = now
	idleTTL := interval * 2
	if idleTTL < clearanceCacheMinIdleTTL {
		idleTTL = clearanceCacheMinIdleTTL
	}
	for key, state := range m.clearances {
		lastUsedAt := state.lastUsedAt
		if lastUsedAt.IsZero() {
			lastUsedAt = state.refreshedAt
		}
		if !lastUsedAt.IsZero() && now.Sub(lastUsedAt) >= idleTTL {
			delete(m.clearances, key)
		}
	}
}

func (m *Manager) ensureClearanceCacheCapacityLocked() {
	if len(m.clearances) < maxCachedClearances {
		return
	}
	type candidate struct {
		key      string
		lastUsed time.Time
	}
	candidates := make([]candidate, 0, len(m.clearances))
	for key, state := range m.clearances {
		lastUsedAt := state.lastUsedAt
		if lastUsedAt.IsZero() {
			lastUsedAt = state.refreshedAt
		}
		candidates = append(candidates, candidate{key: key, lastUsed: lastUsedAt})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastUsed.Before(candidates[j].lastUsed)
	})
	removeCount := min(clearanceCacheEvictionBatch, len(candidates))
	for _, entry := range candidates[:removeCount] {
		delete(m.clearances, entry.key)
	}
}

func clearanceRefreshInterval(cfg ClearanceConfig) time.Duration {
	if cfg.RefreshInterval > 0 {
		return cfg.RefreshInterval
	}
	return 10 * time.Minute
}

func clearanceFingerprint(cfg ClearanceConfig, proxyURL string) string {
	value := strings.TrimSpace(cfg.FlareSolverrURL) + "\x00" + clearanceBindingFingerprint(cfg, proxyURL)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func clearanceBindingFingerprint(cfg ClearanceConfig, proxyURL string) string {
	value := strings.TrimRight(strings.TrimSpace(cfg.TargetURL), "/") + "\x00" + strings.TrimSpace(proxyURL)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func (m *Manager) recordClearanceError(ctx context.Context, node domain.Node, persist bool) {
	if node.ID == 0 || !persist {
		return
	}
	if stateRepository, ok := m.repository.(egressStateRepository); ok {
		if err := stateRepository.UpdateEgressNodeLastError(ctx, node.ID, "clearance refresh failed"); err == nil {
			m.invalidateNodes(node.Scope)
			return
		}
	}
	latest, err := m.repository.GetEgressNode(ctx, node.ID)
	if err != nil {
		return
	}
	latest.LastError = "clearance refresh failed"
	if _, err := m.repository.UpdateEgressNode(ctx, latest); err == nil {
		m.invalidateNodes(latest.Scope)
	}
}

func (m *Manager) RefreshClearance(ctx context.Context, nodeID uint64) error {
	if nodeID == 0 {
		_, err, _ := m.clearanceLoads.Do("direct", func() (any, error) {
			return m.refreshNode(ctx, domain.Node{Name: "direct", Scope: domain.ScopeWeb, Enabled: true}, "", "direct", false, true, true)
		})
		return err
	}
	node, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if !isGrokWebScope(node.Scope) {
		return fmt.Errorf("出口节点 %q 不支持 Clearance 刷新", node.Name)
	}
	proxyURL, err := m.cipher.Decrypt(node.EncryptedProxyURL)
	if err != nil {
		return err
	}
	if strings.Contains(proxyURL, application.ProxyAccountPlaceholder) {
		return fmt.Errorf("出口节点 %q 使用账号粘性代理，将在账号请求时按租约自动刷新 Clearance", node.Name)
	}
	proxyURL, err = application.NormalizeProxyURL(proxyURL)
	if err != nil {
		return err
	}
	key := clearanceCacheKey(node.ID, proxyURL, false)
	_, err, _ = m.clearanceLoads.Do(key, func() (any, error) {
		return m.refreshNode(ctx, node, proxyURL, key, true, true, true)
	})
	return err
}

func (m *Manager) InvalidateClearance(nodeID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := "node:" + strconv.FormatUint(nodeID, 10)
	if nodeID == 0 {
		prefix = "direct"
	}
	for key, state := range m.clearances {
		if key == prefix || strings.HasPrefix(key, prefix+":") {
			state.invalid = true
			state.used = true
			m.clearances[key] = state
		}
	}
	m.invalidateClientLocked(nodeID)
}

// ForgetClearance evicts runtime state after an administrator changes or
// removes a node. Unlike a 403 rejection, it does not mark the persisted
// last-known-good cookie as invalid; ensureClearance will still verify its
// binding before using it as a solver-failure fallback.
func (m *Manager) ForgetClearance(nodeID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := "node:" + strconv.FormatUint(nodeID, 10)
	if nodeID == 0 {
		prefix = "direct"
	}
	for key := range m.clearances {
		if key == prefix || strings.HasPrefix(key, prefix+":") {
			delete(m.clearances, key)
		}
	}
	// Node mutations are rare administration operations. Clearing the small
	// one-second snapshots prevents a just-edited proxy from being used once
	// more before its scope cache expires.
	clear(m.nodes)
	m.invalidateClientLocked(nodeID)
}

func (m *Manager) invalidateClearanceKey(key string, client requestClient) {
	m.mu.Lock()
	state := m.clearances[key]
	state.invalid = true
	state.used = true
	state.lastUsedAt = time.Now().UTC()
	m.clearances[key] = state
	m.mu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
}

func (m *Manager) RefreshDueClearances(ctx context.Context, force bool) error {
	m.mu.Lock()
	cfg := m.clearanceConfig
	direct := m.clearances["direct"]
	version := m.clearanceVersion
	m.mu.Unlock()
	if cfg.Mode != "flaresolverr" {
		return nil
	}
	interval := clearanceRefreshInterval(cfg)
	now := time.Now().UTC()
	nodes, err := m.repository.ListEgressNodes(ctx, "", repository.SortQuery{})
	if err != nil {
		return err
	}
	var refreshErrors []error
	webNodeCount := 0
	for _, node := range nodes {
		if !node.Enabled || !isGrokWebScope(node.Scope) {
			continue
		}
		webNodeCount++
		proxyURL, decryptErr := m.cipher.Decrypt(node.EncryptedProxyURL)
		if decryptErr != nil {
			refreshErrors = append(refreshErrors, decryptErr)
			continue
		}
		if strings.Contains(proxyURL, application.ProxyAccountPlaceholder) {
			// Resin clearance is account/IP bound and has no safe node-wide value
			// for a background task to solve or persist.
			continue
		}
		proxyURL, normalizeErr := application.NormalizeProxyURL(proxyURL)
		if normalizeErr != nil {
			refreshErrors = append(refreshErrors, normalizeErr)
			continue
		}
		m.mu.Lock()
		key := clearanceCacheKey(node.ID, proxyURL, false)
		state, known := m.clearances[key]
		m.mu.Unlock()
		fingerprint := clearanceFingerprint(cfg, proxyURL)
		memoryFresh := known && !state.invalid && state.version == version && state.fingerprint == fingerprint && now.Sub(state.refreshedAt) < interval
		persistedFresh := node.ClearanceRefreshedAt != nil && node.ClearanceFingerprint == fingerprint && now.Sub(*node.ClearanceRefreshedAt) < interval
		if !force && (memoryFresh || persistedFresh) {
			continue
		}
		_, refreshErr, _ := m.clearanceLoads.Do(key, func() (any, error) {
			return m.refreshNode(ctx, node, proxyURL, key, true, force, false)
		})
		if refreshErr != nil {
			refreshErrors = append(refreshErrors, refreshErr)
		}
	}
	shouldUseDirect := direct.used || force && webNodeCount == 0
	if shouldUseDirect && (force || direct.invalid || direct.cookies == "" || direct.version != version || now.Sub(direct.refreshedAt) >= interval) {
		_, err, _ := m.clearanceLoads.Do("direct", func() (any, error) {
			return m.refreshNode(ctx, domain.Node{Name: "direct", Scope: domain.ScopeWeb, Enabled: true}, "", "direct", false, force, false)
		})
		if err != nil {
			refreshErrors = append(refreshErrors, err)
		}
	}
	return errors.Join(refreshErrors...)
}

func isGrokWebScope(scope domain.Scope) bool {
	return scope == domain.ScopeWeb || scope == domain.ScopeWebAsset || scope == domain.ScopeConsole
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
