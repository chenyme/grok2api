package egress

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestDirectFallbackRebuildsClientAfterAntiBotRejection(t *testing.T) {
	manager := &Manager{clients: map[clientCacheKey]cachedClient{{nodeID: 0, scope: domain.ScopeWeb, fingerprint: "web"}: {}}}
	manager.Feedback(context.Background(), 0, http.StatusForbidden, nil)
	if len(manager.clients) != 0 {
		t.Fatal("direct fallback client was not invalidated after anti-bot rejection")
	}
}

func TestClientCacheEvictsIdleEntriesAndEnforcesCapacity(t *testing.T) {
	now := time.Now()
	idleClient := &scriptedRequestClient{}
	freshClient := &scriptedRequestClient{}
	idleKey := clientCacheKey{nodeID: 1, scope: domain.ScopeWeb, fingerprint: "idle"}
	freshKey := clientCacheKey{nodeID: 1, scope: domain.ScopeWeb, fingerprint: "fresh"}
	manager := &Manager{clients: map[clientCacheKey]cachedClient{
		idleKey:  {client: idleClient, lastUsed: now.Add(-clientCacheIdleTTL)},
		freshKey: {client: freshClient, lastUsed: now},
	}}
	manager.cleanupClientCacheLocked(now)
	if _, exists := manager.clients[idleKey]; exists || idleClient.closedIdle != 1 {
		t.Fatalf("idle client exists=%v closed=%d", exists, idleClient.closedIdle)
	}
	if _, exists := manager.clients[freshKey]; !exists || freshClient.closedIdle != 0 {
		t.Fatalf("fresh client exists=%v closed=%d", exists, freshClient.closedIdle)
	}

	oldestClient := &scriptedRequestClient{}
	oldestKey := clientCacheKey{nodeID: 2, scope: domain.ScopeBuild, fingerprint: "oldest"}
	manager.clients = make(map[clientCacheKey]cachedClient, maxCachedClients)
	manager.clients[oldestKey] = cachedClient{client: oldestClient, lastUsed: now.Add(-time.Hour)}
	for index := 1; index < maxCachedClients; index++ {
		key := clientCacheKey{nodeID: uint64(index + 2), scope: domain.ScopeBuild, fingerprint: "cached"}
		manager.clients[key] = cachedClient{lastUsed: now}
	}
	manager.ensureClientCacheCapacityLocked()
	if len(manager.clients) != maxCachedClients-1 || oldestClient.closedIdle != 1 {
		t.Fatalf("cache size=%d oldest closed=%d", len(manager.clients), oldestClient.closedIdle)
	}
}

func TestClearanceCacheEvictsIdleEntriesAndEnforcesCapacity(t *testing.T) {
	now := time.Now().UTC()
	manager := &Manager{clearances: map[string]clearanceState{
		"idle":  {cookies: "cf_clearance=idle", lastUsedAt: now.Add(-clearanceCacheMinIdleTTL)},
		"fresh": {cookies: "cf_clearance=fresh", lastUsedAt: now},
	}}
	manager.cleanupClearanceCacheLocked(now, time.Minute)
	if _, exists := manager.clearances["idle"]; exists {
		t.Fatal("idle Clearance entry was not evicted")
	}
	if _, exists := manager.clearances["fresh"]; !exists {
		t.Fatal("fresh Clearance entry was evicted")
	}

	manager.clearances = make(map[string]clearanceState, maxCachedClearances)
	manager.clearances["oldest"] = clearanceState{lastUsedAt: now.Add(-time.Hour)}
	for index := 1; index < maxCachedClearances; index++ {
		manager.clearances[fmt.Sprintf("cached-%d", index)] = clearanceState{lastUsedAt: now}
	}
	manager.ensureClearanceCacheCapacityLocked()
	if len(manager.clearances) != maxCachedClearances-clearanceCacheEvictionBatch {
		t.Fatalf("Clearance cache size = %d", len(manager.clearances))
	}
	if _, exists := manager.clearances["oldest"]; exists {
		t.Fatal("oldest Clearance entry was not evicted")
	}
}

func TestDirectBuildAndWebClientsDoNotEvictEachOther(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{}, cipher)
	buildFirst, err := manager.Acquire(context.Background(), domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	defer buildFirst.Release()
	web, err := manager.Acquire(context.Background(), domain.ScopeWeb, "")
	if err != nil {
		t.Fatal(err)
	}
	defer web.Release()
	buildSecond, err := manager.Acquire(context.Background(), domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	defer buildSecond.Release()

	if buildFirst.client != buildSecond.client {
		t.Fatal("Web direct traffic evicted the reusable Build connection pool")
	}
	if buildFirst.client == web.client || len(manager.clients) != 2 {
		t.Fatalf("direct clients were not isolated: build=%T web=%T cached=%d", buildFirst.client, web.client, len(manager.clients))
	}
	manager.FeedbackForScope(context.Background(), domain.ScopeWeb, 0, http.StatusForbidden, nil)
	buildAfterWebFailure, err := manager.Acquire(context.Background(), domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	defer buildAfterWebFailure.Release()
	if buildAfterWebFailure.client != buildFirst.client || len(manager.clients) != 1 {
		t.Fatalf("Web failure evicted Build direct client: reused=%v cached=%d", buildAfterWebFailure.client == buildFirst.client, len(manager.clients))
	}
}

func TestBrowserRequestLeavesHeaderOrderingToTLSProfile(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "https://grok.com/rest/app-chat/conversations/new", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("User-Agent", DefaultUserAgent)
	request.Header.Set("Accept", "*/*")
	converted, err := toFHTTPRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(converted.Header[fhttp.HeaderOrderKey]) != 0 || len(converted.Header[fhttp.PHeaderOrderKey]) != 0 {
		t.Fatalf("manual header order=%#v pseudo=%#v", converted.Header[fhttp.HeaderOrderKey], converted.Header[fhttp.PHeaderOrderKey])
	}
}

func TestBrowserProfileTracksFlareSolverrChromiumUserAgent(t *testing.T) {
	if actual := browserProfile("Mozilla/5.0 Chrome/144.0.0.0 Safari/537.36").GetClientHelloStr(); actual != profiles.Chrome_144.GetClientHelloStr() {
		t.Fatalf("Chrome 144 selected %q", actual)
	}
	if actual := browserProfile("Mozilla/5.0 Chrome/145.0.0.0 Safari/537.36").GetClientHelloStr(); actual != profiles.Chrome_146.GetClientHelloStr() {
		t.Fatalf("Chrome 145 did not select nearest profile: %q", actual)
	}
}

func TestConfiguredCoolingAppNodesNeverFallBackToDirect(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute)
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 1, Name: "proxy", Scope: domain.ScopeWeb, Enabled: true, CooldownUntil: &until,
	}}}, cipher)
	if _, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account"); err == nil {
		t.Fatal("cooling configured node unexpectedly fell back to direct")
	}
}

func TestDisabledConfiguredNodesAllowDirectFallback(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 1, Name: "disabled-proxy", Scope: domain.ScopeBuild, Enabled: false, Health: 1,
	}}}, nil)
	lease, configured, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "")
	if err != nil || configured || lease != nil {
		t.Fatalf("disabled proxy fallback: lease=%#v configured=%v err=%v", lease, configured, err)
	}
}

func TestAcquireIfConfiguredDoesNotChangeBuildDirectTransport(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{}, cipher)
	ctx, trace := WithTrace(context.Background())
	lease, configured, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "")
	if err != nil || configured || lease != nil {
		t.Fatalf("lease=%#v configured=%v err=%v", lease, configured, err)
	}
	selection, ok := trace.Selection(domain.ScopeBuild)
	if !ok || selection.NodeID != 0 || selection.NodeName != "direct" || selection.Proxied {
		t.Fatalf("direct selection = %#v, ok=%v", selection, ok)
	}
}

func TestTraceRecordsConfiguredProxyWithoutCredentials(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("socks5h://secret:password@127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 42, Name: "primary-proxy", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy,
	}}}, cipher)
	ctx, trace := WithTrace(context.Background())
	lease, configured, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "")
	if err != nil || !configured || lease == nil {
		t.Fatalf("lease=%#v configured=%v err=%v", lease, configured, err)
	}
	defer lease.Release()
	selection, ok := trace.Selection(domain.ScopeBuild)
	if !ok || selection.NodeID != 42 || selection.NodeName != "primary-proxy" || !selection.Proxied {
		t.Fatalf("proxy selection = %#v, ok=%v", selection, ok)
	}
}

func TestConfiguredBuildNodeDoesNotOverrideProviderUserAgent(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("socks5h://warp:1080")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 1, Name: "build", Scope: domain.ScopeBuild, Enabled: true, Health: 1, UserAgent: "legacy-build-agent", EncryptedProxyURL: encryptedProxy,
	}}}, cipher)
	lease, configured, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	if !configured || lease == nil {
		t.Fatal("configured build node did not produce a lease")
	}
	defer lease.Release()
	if lease.UserAgent != "" {
		t.Fatalf("build lease userAgent = %q", lease.UserAgent)
	}
	if _, ok := lease.client.(*http.Client); !ok || lease.browser != nil || lease.Scope != domain.ScopeBuild {
		t.Fatalf("build lease client=%T browser=%p scope=%q", lease.client, lease.browser, lease.Scope)
	}
	if _, _, err := lease.DialWebSocket(context.Background(), "wss://example.com", nil, time.Second); err == nil {
		t.Fatal("build lease unexpectedly exposed browser WebSocket")
	}
}

func TestConfiguredWebNodeKeepsChromeBrowserTransport(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1,
	}}}, cipher)
	lease, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, ok := lease.client.(*browserClient); !ok || lease.browser == nil || lease.Scope != domain.ScopeWeb {
		t.Fatalf("web lease client=%T browser=%p scope=%q", lease.client, lease.browser, lease.Scope)
	}
}

func TestAcquireCredentialRendersResinAccountAndOverridesNodeCookie(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	nodeCookie, err := cipher.Encrypt("cf_clearance=node")
	if err != nil {
		t.Fatal(err)
	}
	accountCookie, err := cipher.Encrypt("cf_clearance=account")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 1, Name: "resin", Scope: domain.ScopeWeb, Enabled: true, Health: 1,
		EncryptedProxyURL: proxyURL, EncryptedCloudflareCookie: nodeCookie,
	}}}, cipher)
	first, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderWeb, EncryptedCloudflareCookie: accountCookie,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if first.ProxyURL != "socks5h://Default.grok_web_42:token@resin:2260" {
		t.Fatalf("first proxy URL = %q", first.ProxyURL)
	}
	if first.CFCookies != "cf_clearance=account" || !first.sticky {
		t.Fatalf("first lease cookie=%q sticky=%v", first.CFCookies, first.sticky)
	}
	second, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{
		ID: 43, Provider: accountdomain.ProviderWeb,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if second.ProxyURL != "socks5h://Default.grok_web_43:token@resin:2260" {
		t.Fatalf("second proxy URL = %q", second.ProxyURL)
	}
	if second.CFCookies != "cf_clearance=node" {
		t.Fatalf("second lease cookie = %q", second.CFCookies)
	}
	if first.client == second.client {
		t.Fatal("different Resin accounts unexpectedly shared one connection pool")
	}
	if len(manager.clients) != 2 {
		t.Fatalf("cached Resin account pools = %d, want 2", len(manager.clients))
	}
}

func TestFlareSolverrModeIgnoresCredentialCookie(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	credentialCookie, err := cipher.Encrypt("cf_clearance=imported-account")
	if err != nil {
		t.Fatal(err)
	}
	solver := &clearanceSolverStub{}
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1,
	}}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})

	lease, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderWeb, EncryptedCloudflareCookie: credentialCookie,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if solver.calls != 1 || lease.CFCookies != "cf_clearance=value-1" {
		t.Fatalf("solver calls=%d lease cookie=%q", solver.calls, lease.CFCookies)
	}
}

func TestFlareSolverrModeRecoversFromDamagedStoredCookies(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	solver := &clearanceSolverStub{}
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1,
		EncryptedCloudflareCookie: "damaged-node-ciphertext",
	}}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})

	lease, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderWeb, EncryptedCloudflareCookie: "damaged-account-ciphertext",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if solver.calls != 1 || lease.CFCookies != "cf_clearance=value-1" {
		t.Fatalf("solver calls=%d lease cookie=%q", solver.calls, lease.CFCookies)
	}
}

func TestLinkedProvidersSharePersistedResinIdentity(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	firstToken, _ := cipher.Encrypt("first-sso")
	rotatedToken, _ := cipher.Encrypt("rotated-sso")
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL},
		{ID: 2, Name: "build", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL},
	}}, cipher)
	const identity = "sso_persisted_identity"
	web, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{
		ID: 11, Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		EncryptedAccessToken: firstToken, EgressIdentity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer web.Release()
	console, err := manager.AcquireCredential(context.Background(), domain.ScopeConsole, accountdomain.Credential{
		ID: 22, Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		EncryptedAccessToken: rotatedToken, EgressIdentity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer console.Release()
	buildCtx := WithCredential(context.Background(), accountdomain.Credential{ID: 33, Provider: accountdomain.ProviderBuild, EgressIdentity: identity})
	build, configured, err := manager.AcquireIfConfigured(buildCtx, domain.ScopeBuild, AccountFromContext(buildCtx))
	if err != nil || !configured {
		t.Fatalf("build configured=%v err=%v", configured, err)
	}
	defer build.Release()
	for name, proxy := range map[string]string{"web": web.ProxyURL, "console": console.ProxyURL, "build": build.ProxyURL} {
		if !strings.Contains(proxy, "Default."+identity+":") {
			t.Fatalf("%s proxy = %q", name, proxy)
		}
	}
}

func TestConsoleFallsBackToWebAndSharesSSOResinIdentity(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	token := "shared-web-console-sso"
	encryptedToken, err := cipher.Encrypt(token)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 7, Name: "shared-web", Scope: domain.ScopeWeb, Enabled: true, Health: 1,
		EncryptedProxyURL: proxyURL,
	}}}, cipher)
	web, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{
		ID: 11, Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		EncryptedAccessToken: encryptedToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer web.Release()
	console, err := manager.AcquireCredential(context.Background(), domain.ScopeConsole, accountdomain.Credential{
		ID: 22, Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		EncryptedAccessToken: encryptedToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer console.Release()
	wantAccount := "sso_" + security.HashToken(token)[:32]
	if web.NodeID != 7 || console.NodeID != 7 {
		t.Fatalf("nodes web=%d console=%d, want shared Web node", web.NodeID, console.NodeID)
	}
	if !strings.Contains(web.ProxyURL, "Default."+wantAccount+":") || web.ProxyURL != console.ProxyURL {
		t.Fatalf("proxy identities web=%q console=%q", web.ProxyURL, console.ProxyURL)
	}
}

func TestBuildForbiddenDoesNotPoisonEgressNode(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "build", Scope: domain.ScopeBuild, Enabled: true, Health: 1}}
	manager := NewManager(repository, cipher)
	lease, _, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, http.StatusForbidden, nil)
	if repository.updates != 0 || repository.node.Health != 1 || repository.node.LastError != "" {
		t.Fatalf("build 403 poisoned node: updates=%d node=%#v", repository.updates, repository.node)
	}
	if !managerHasClientForNode(manager, 1) {
		t.Fatal("build client was invalidated by an ambiguous 403")
	}
}

func TestUpstreamServerErrorDoesNotPoisonFixedEgressNode(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "build", Scope: domain.ScopeBuild, Enabled: true, Health: 1}}
	manager := NewManager(repository, cipher)
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, http.StatusBadGateway, nil)
	if repository.updates != 0 || repository.node.Health != 1 || repository.node.CooldownUntil != nil {
		t.Fatalf("upstream 502 poisoned fixed node: updates=%d node=%#v", repository.updates, repository.node)
	}
}

func TestProxyPoolTransportFailureDoesNotCreateGlobalCooldown(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	cooldown := time.Now().UTC().Add(time.Minute)
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "pool", Scope: domain.ScopeBuild, Enabled: true, ProxyPool: true,
		Health: 0.2, FailureCount: 3, CooldownUntil: &cooldown, LastError: "old failure",
	}}
	manager := NewManager(repository, cipher)
	lease, configured, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "")
	if err != nil || !configured || lease == nil {
		t.Fatalf("pool lease blocked by stale cooldown: configured=%v lease=%#v err=%v", configured, lease, err)
	}
	lease.Release()
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, 0, errors.New("connection refused"))
	if repository.updates != 0 || repository.node.FailureCount != 3 || repository.node.CooldownUntil == nil {
		t.Fatalf("pool transport failure changed global state: updates=%d node=%#v", repository.updates, repository.node)
	}
	if !managerHasClientForNode(manager, 1) {
		t.Fatal("pool transport failure evicted the shared node client cache")
	}
}

func TestFixedProxyTransportFailureStillCreatesCooldown(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "fixed", Scope: domain.ScopeBuild, Enabled: true, Health: 1}}
	manager := NewManager(repository, cipher)
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, 0, errors.New("connection refused"))
	if repository.updates != 1 || repository.node.FailureCount != 1 || repository.node.CooldownUntil == nil || repository.node.LastError != "transport error" {
		t.Fatalf("fixed transport failure did not create cooldown: updates=%d node=%#v", repository.updates, repository.node)
	}
}

func TestAccountTemplateIsAnEffectiveProxyPool(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("socks5h://Default.{account}:token@resin.example:2260")
	if err != nil {
		t.Fatal(err)
	}
	cooldown := time.Now().UTC().Add(time.Minute)
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "resin", Scope: domain.ScopeBuild, Enabled: true, Health: 0.2,
		EncryptedProxyURL: encryptedProxy, CooldownUntil: &cooldown,
	}}
	manager := NewManager(repository, cipher)
	lease, configured, err := manager.AcquireIfConfigured(WithAccountIdentity(context.Background(), "account-1"), domain.ScopeBuild, "")
	if err != nil || !configured || lease == nil {
		t.Fatalf("account-template lease blocked by stale cooldown: configured=%v lease=%#v err=%v", configured, lease, err)
	}
	defer lease.Release()
	if !lease.sticky || !lease.proxyPool {
		t.Fatalf("account-template lease flags: sticky=%v proxyPool=%v", lease.sticky, lease.proxyPool)
	}
}

func TestWebForbiddenStillRebuildsBrowserSession(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1}}
	manager := NewManager(repository, cipher)
	lease, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	manager.Feedback(context.Background(), 1, http.StatusForbidden, nil)
	if repository.updates != 1 || repository.node.Health >= 1 || repository.node.LastError != "anti-bot rejection" {
		t.Fatalf("web 403 feedback = updates=%d node=%#v", repository.updates, repository.node)
	}
	if managerHasClientForNode(manager, 1) {
		t.Fatal("web browser session was not invalidated after 403")
	}
}

func TestFlareSolverrRefreshesRejectedNodeBeforeNextLease(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1}}
	solver := &clearanceSolverStub{}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})

	first, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	if first.CFCookies != "cf_clearance=value-1" || first.UserAgent != "Chrome/146 test" {
		t.Fatalf("first lease = %#v", first)
	}
	first.Release()
	manager.Feedback(context.Background(), 1, http.StatusForbidden, nil)
	second, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if solver.calls != 2 || second.CFCookies != "cf_clearance=value-2" {
		t.Fatalf("calls=%d second cookies=%q", solver.calls, second.CFCookies)
	}
	stored, err := cipher.Decrypt(repository.node.EncryptedCloudflareCookie)
	if err != nil || stored != "cf_clearance=value-2" {
		t.Fatalf("stored cookies=%q err=%v", stored, err)
	}
}

func TestFlareSolverrSupportsDirectWebEgress(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	solver := &clearanceSolverStub{}
	manager := NewManager(egressRepositoryTestStub{}, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})
	lease, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 0 || lease.CFCookies != "cf_clearance=value-1" || solver.proxyURL != "" {
		t.Fatalf("direct lease=%#v proxy=%q", lease, solver.proxyURL)
	}
}

func TestFlareSolverrPrewarmsDirectWebEgressWhenNoNodesExist(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	solver := &clearanceSolverStub{}
	manager := NewManager(egressRepositoryTestStub{}, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})
	if err := manager.RefreshDueClearances(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if solver.calls != 1 || lease.CFCookies != "cf_clearance=value-1" {
		t.Fatalf("calls=%d cookies=%q", solver.calls, lease.CFCookies)
	}
}

func TestStickyProxyForbiddenDoesNotCooldownSharedNode(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "resin", Scope: domain.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: proxy}}
	manager := NewManager(repository, cipher)
	lease, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{ID: 42, Provider: accountdomain.ProviderWeb})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	manager.Feedback(context.Background(), 1, http.StatusForbidden, nil)
	if repository.updates != 0 || repository.node.Health != 1 || repository.node.LastError != "" {
		t.Fatalf("sticky proxy 403 changed shared node: updates=%d node=%#v", repository.updates, repository.node)
	}
}

func TestFlareSolverrIsolatesResinClearancePerAccount(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "resin", Scope: domain.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: proxy,
	}}
	solver := &clearanceSolverStub{}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})

	first, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{ID: 42, Provider: accountdomain.ProviderWeb})
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	second, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{ID: 43, Provider: accountdomain.ProviderWeb})
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	again, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{ID: 42, Provider: accountdomain.ProviderWeb})
	if err != nil {
		t.Fatal(err)
	}
	again.Release()

	if first.CFCookies != "cf_clearance=value-1" || second.CFCookies != "cf_clearance=value-2" || again.CFCookies != first.CFCookies {
		t.Fatalf("clearances leaked across accounts: first=%q second=%q again=%q", first.CFCookies, second.CFCookies, again.CFCookies)
	}
	if solver.calls != 2 || repository.updates != 0 || repository.node.EncryptedCloudflareCookie != "" {
		t.Fatalf("calls=%d updates=%d persisted=%q", solver.calls, repository.updates, repository.node.EncryptedCloudflareCookie)
	}
}

func TestClearanceRefreshFailureUsesLastKnownGoodUntilRejected(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1}}
	solver := &clearanceSolverStub{}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Nanosecond})

	first, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	solver.err = errors.New("solver unavailable")
	second, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil || second.CFCookies != first.CFCookies {
		t.Fatalf("last-known-good was not used: cookies=%q err=%v", second.CFCookies, err)
	}
	second.Release()

	manager.InvalidateClearance(1)
	if _, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account"); err == nil {
		t.Fatal("invalid clearance was reused after a rejection")
	}
}

func TestClearanceFallbackSurvivesSolverAddressChangeOnly(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1}}
	solver := &clearanceSolverStub{}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	base := ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver-a", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour}
	manager.UpdateClearanceConfig(base)
	first, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	first.Release()

	base.FlareSolverrURL = "http://solver-b"
	manager.UpdateClearanceConfig(base)
	solver.err = errors.New("new solver unavailable")
	second, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if second.CFCookies != first.CFCookies || solver.calls != 2 {
		t.Fatalf("fallback cookie=%q want=%q solver calls=%d", second.CFCookies, first.CFCookies, solver.calls)
	}
}

func TestNodeEditForgetsRuntimeStateButKeepsBoundFallback(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1}}
	solver := &clearanceSolverStub{}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})
	first, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	first.Release()

	// Service updates clear freshness but preserve the binding that proves the
	// old cookie still belongs to this target/proxy pair.
	repository.node.Name = "renamed"
	repository.node.ClearanceRefreshedAt = nil
	repository.node.ClearanceFingerprint = ""
	manager.ForgetClearance(repository.node.ID)
	solver.err = errors.New("solver unavailable")
	second, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if second.CFCookies != first.CFCookies || solver.calls != 2 {
		t.Fatalf("fallback cookie=%q want=%q solver calls=%d", second.CFCookies, first.CFCookies, solver.calls)
	}
}

func TestClearanceFallbackRejectsDifferentBinding(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyA, err := cipher.Encrypt("socks5h://proxy-a:1080")
	if err != nil {
		t.Fatal(err)
	}
	proxyB, err := cipher.Encrypt("socks5h://proxy-b:1080")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: proxyA}}
	solver := &clearanceSolverStub{}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	config := ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour}
	manager.UpdateClearanceConfig(config)
	first, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	solver.err = errors.New("solver unavailable")

	config.TargetURL = "https://console.x.ai"
	manager.UpdateClearanceConfig(config)
	if _, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account"); err == nil {
		t.Fatal("Clearance from a different target binding was reused")
	}

	config.TargetURL = "https://grok.com"
	manager.UpdateClearanceConfig(config)
	repository.node.EncryptedProxyURL = proxyB
	manager.invalidateNodes(domain.ScopeWeb)
	if _, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account"); err == nil {
		t.Fatal("Clearance from a different proxy binding was reused")
	}
}

func TestClearanceBackgroundRefreshSkipsResinTemplate(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	solver := &clearanceSolverStub{}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 1, Name: "resin", Scope: domain.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: proxy,
	}}}, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})
	if err := manager.RefreshDueClearances(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if solver.calls != 0 {
		t.Fatalf("background refresh solved an account template %d times", solver.calls)
	}
}

func TestPersistedClearancePreventsDuplicateInstanceRefresh(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1}}
	solver := &clearanceSolverStub{}
	config := ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour}
	firstManager := NewManager(repository, cipher)
	firstManager.solver = solver
	firstManager.UpdateClearanceConfig(config)
	first, err := firstManager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	first.Release()

	secondManager := NewManager(repository, cipher)
	secondManager.solver = solver
	secondManager.UpdateClearanceConfig(config)
	second, err := secondManager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	if solver.calls != 1 || second.CFCookies != first.CFCookies {
		t.Fatalf("instances did not reuse persisted clearance: calls=%d first=%q second=%q", solver.calls, first.CFCookies, second.CFCookies)
	}
}

func TestWebAssetCredentialFallsBackToWebWithSameResinIdentity(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	accountCookie, err := cipher.Encrypt("cf_clearance=account")
	if err != nil {
		t.Fatal(err)
	}
	token := "shared-web-asset-sso"
	encryptedToken, err := cipher.Encrypt(token)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 2, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL},
	}}, cipher)
	credential := accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		EncryptedAccessToken: encryptedToken, EncryptedCloudflareCookie: accountCookie,
	}
	webLease, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer webLease.Release()
	lease, err := manager.AcquireCredential(context.Background(), domain.ScopeWebAsset, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 2 {
		t.Fatalf("node = %d, want web fallback node 2", lease.NodeID)
	}
	wantAccount := "sso_" + security.HashToken(token)[:32]
	if lease.ProxyURL != webLease.ProxyURL || !strings.Contains(lease.ProxyURL, "Default."+wantAccount+":") {
		t.Fatalf("proxy identities web=%q asset=%q", webLease.ProxyURL, lease.ProxyURL)
	}
	if lease.CFCookies != "cf_clearance=account" {
		t.Fatalf("asset lease cookie = %q", lease.CFCookies)
	}
	if lease.client != webLease.client {
		t.Fatal("Web Asset credential fallback did not reuse the matching Web browser session")
	}
}

func TestEgressNodeSnapshotAvoidsRepeatedRepositoryReads(t *testing.T) {
	repository := &countingEgressRepository{egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{{ID: 1, Scope: domain.ScopeWeb, Enabled: true}}}}
	manager := NewManager(repository, nil)
	now := time.Now().UTC()
	for range 2 {
		values, err := manager.listNodes(context.Background(), domain.ScopeWeb, now)
		if err != nil || len(values) != 1 {
			t.Fatalf("nodes=%#v err=%v", values, err)
		}
	}
	if repository.calls != 1 {
		t.Fatalf("repository reads = %d, want 1", repository.calls)
	}
}

type egressRepositoryTestStub struct{ nodes []domain.Node }

func managerHasClientForNode(manager *Manager, nodeID uint64) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for key := range manager.clients {
		if key.nodeID == nodeID {
			return true
		}
	}
	return false
}

type countingEgressRepository struct {
	egressRepositoryTestStub
	calls int
}

type mutableEgressRepository struct {
	node    domain.Node
	updates int
}

type clearanceSolverStub struct {
	calls    int
	proxyURL string
	err      error
}

func (s *clearanceSolverStub) Solve(_ context.Context, _ ClearanceConfig, proxyURL string) (clearanceSolution, error) {
	s.calls++
	s.proxyURL = proxyURL
	if s.err != nil {
		return clearanceSolution{}, s.err
	}
	return clearanceSolution{Cookies: fmt.Sprintf("cf_clearance=value-%d", s.calls), UserAgent: "Chrome/146 test"}, nil
}

func (r *mutableEgressRepository) ListEgressNodes(_ context.Context, scope domain.Scope, _ repository.SortQuery) ([]domain.Node, error) {
	if scope != "" && r.node.Scope != scope {
		return nil, nil
	}
	return []domain.Node{r.node}, nil
}

func (r *mutableEgressRepository) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	if r.node.ID != id {
		return domain.Node{}, errors.New("not found")
	}
	return r.node, nil
}

func (r *mutableEgressRepository) CreateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	r.node = value
	return value, nil
}

func (r *mutableEgressRepository) UpdateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	r.node = value
	r.updates++
	return value, nil
}

func (r *mutableEgressRepository) DeleteEgressNode(_ context.Context, id uint64) error {
	if r.node.ID != id {
		return errors.New("not found")
	}
	r.node = domain.Node{}
	return nil
}

func (r *countingEgressRepository) ListEgressNodes(ctx context.Context, scope domain.Scope, sort repository.SortQuery) ([]domain.Node, error) {
	r.calls++
	return r.egressRepositoryTestStub.ListEgressNodes(ctx, scope, sort)
}

func (s egressRepositoryTestStub) ListEgressNodes(_ context.Context, scope domain.Scope, _ repository.SortQuery) ([]domain.Node, error) {
	values := make([]domain.Node, 0, len(s.nodes))
	for _, node := range s.nodes {
		if scope == "" || node.Scope == scope {
			values = append(values, node)
		}
	}
	return values, nil
}
func (egressRepositoryTestStub) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	return domain.Node{}, errors.New("not found")
}
func (egressRepositoryTestStub) CreateEgressNode(context.Context, domain.Node) (domain.Node, error) {
	return domain.Node{}, errors.New("unsupported")
}
func (egressRepositoryTestStub) UpdateEgressNode(context.Context, domain.Node) (domain.Node, error) {
	return domain.Node{}, errors.New("unsupported")
}
func (egressRepositoryTestStub) DeleteEgressNode(context.Context, uint64) error {
	return errors.New("unsupported")
}
