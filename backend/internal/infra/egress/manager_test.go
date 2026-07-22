package egress

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type responseHeaderTimeoutError struct{}

func (responseHeaderTimeoutError) Error() string   { return "http2: timeout awaiting response headers" }
func (responseHeaderTimeoutError) Timeout() bool   { return true }
func (responseHeaderTimeoutError) Temporary() bool { return true }

func TestBuildResponseHeaderTimeoutHotUpdateRebuildsCachedClients(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	var observed []time.Duration
	var clients []*scriptedRequestClient
	manager.newBuildClient = func(_ string, timeout time.Duration) (requestClient, error) {
		client := &scriptedRequestClient{}
		observed = append(observed, timeout)
		clients = append(clients, client)
		return client, nil
	}
	if _, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	manager.UpdateBuildResponseHeaderTimeout(7 * time.Minute)
	if len(clients) != 1 || clients[0].closedIdle != 1 {
		t.Fatalf("old clients=%d closed=%d", len(clients), clients[0].closedIdle)
	}
	if _, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || observed[0] != 5*time.Minute || observed[1] != 7*time.Minute {
		t.Fatalf("observed timeouts = %v", observed)
	}
}

func TestResponseHeaderTimeoutDoesNotPenalizeEgress(t *testing.T) {
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "fixed", Scope: domain.ScopeBuild, Enabled: true, Health: 1}}
	manager := NewManager(repository, nil)
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, 0, responseHeaderTimeoutError{})
	if repository.updates != 0 || repository.node.Health != 1 || repository.node.CooldownUntil != nil {
		t.Fatalf("response-header timeout changed node health: updates=%d node=%#v", repository.updates, repository.node)
	}
	key := clientCacheKey{nodeID: 0, scope: domain.ScopeBuild, fingerprint: "direct"}
	manager.clients[key] = cachedClient{client: &scriptedRequestClient{}}
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 0, 0, responseHeaderTimeoutError{})
	if _, exists := manager.clients[key]; !exists {
		t.Fatal("response-header timeout invalidated the direct Build client")
	}
}

func TestResponseHeaderTimeoutRetainsWebEgressFeedback(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	key := clientCacheKey{nodeID: 0, scope: domain.ScopeWeb, fingerprint: "direct"}
	manager.clients[key] = cachedClient{client: &scriptedRequestClient{}}
	manager.FeedbackForScope(context.Background(), domain.ScopeWeb, 0, 0, responseHeaderTimeoutError{})
	if _, exists := manager.clients[key]; exists {
		t.Fatal("Build-specific timeout policy suppressed Web egress feedback")
	}
}

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
	closeRequestClients(manager.cleanupClientCacheLocked(now))
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
	closeRequestClients(manager.ensureClientCacheCapacityLocked())
	if len(manager.clients) != maxCachedClients-1 || oldestClient.closedIdle != 1 {
		t.Fatalf("cache size=%d oldest closed=%d", len(manager.clients), oldestClient.closedIdle)
	}
}

func TestClientVersionTombstonesRemainBounded(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	manager.clientMu.Lock()
	for nodeID := uint64(1); nodeID <= maxClientVersionEntries+256; nodeID++ {
		manager.invalidateClientVersionLocked(nodeID)
	}
	manager.clientMu.Unlock()
	if len(manager.clientVersions) > maxClientVersionEntries {
		t.Fatalf("client version tombstones = %d, limit = %d", len(manager.clientVersions), maxClientVersionEntries)
	}
	if manager.clientGeneration == 0 {
		t.Fatal("bounded version reset did not advance the invalidation generation")
	}
}

func TestClientCreationDoesNotHoldManagerLock(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		close(started)
		<-release
		return &scriptedRequestClient{}, nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false)
		result <- err
	}()
	<-started

	selected := make(chan struct{})
	go func() {
		manager.selectNode([]domain.Node{{ID: 1, Health: 1}}, "")
		close(selected)
	}()
	select {
	case <-selected:
	case <-time.After(time.Second):
		t.Fatal("client creation held the manager lock")
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestSelectNodeUsesAtomicInflightCounters(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	nodes := []domain.Node{{ID: 1, Health: 1}, {ID: 2, Health: 1}}
	manager.incrementInflight(1)
	if selected := manager.selectNode(nodes, ""); selected.ID != 2 {
		t.Fatalf("selected node = %d, want 2", selected.ID)
	}
	manager.decrementInflight(1)
	if selected := manager.selectNode(nodes, ""); selected.ID != 1 {
		t.Fatalf("selected node after release = %d, want stable node 1", selected.ID)
	}
}

func TestInflightCountersRemainBalancedConcurrently(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	const workers = 64
	const iterations = 1000
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range iterations {
				manager.incrementInflight(1)
				manager.decrementInflight(1)
			}
		}()
	}
	wait.Wait()
	if value := manager.inflightCount(1); value != 0 {
		t.Fatalf("inflight count = %d, want 0", value)
	}
}

func TestClientCacheCoalescesLastUsedWrites(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	client := &scriptedRequestClient{}
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) { return client, nil }
	if _, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false); err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC()
	var key clientCacheKey
	manager.clientMu.Lock()
	for candidate, value := range manager.clients {
		key = candidate
		value.lastUsed = base
		manager.clients[candidate] = value
	}
	manager.lastClientCleanup = base
	manager.clientMu.Unlock()

	if _, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	manager.clientMu.RLock()
	untouched := manager.clients[key].lastUsed
	manager.clientMu.RUnlock()
	if !untouched.Equal(base) {
		t.Fatalf("fresh cache hit rewrote lastUsed: got %s want %s", untouched, base)
	}

	stale := base.Add(-clientCacheTouchInterval - time.Second)
	manager.clientMu.Lock()
	value := manager.clients[key]
	value.lastUsed = stale
	manager.clients[key] = value
	manager.lastClientCleanup = time.Now().UTC()
	manager.clientMu.Unlock()
	if _, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	manager.clientMu.RLock()
	refreshed := manager.clients[key].lastUsed
	manager.clientMu.RUnlock()
	if !refreshed.After(stale) {
		t.Fatalf("stale cache hit did not refresh lastUsed: got %s, stale %s", refreshed, stale)
	}
}

func TestClientCreationDiscardsInvalidatedResult(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	first := &scriptedRequestClient{}
	second := &scriptedRequestClient{}
	var calls atomic.Int32
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
			return first, nil
		}
		return second, nil
	}
	result := make(chan cachedClient, 1)
	errorsCh := make(chan error, 1)
	go func() {
		value, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false)
		if err != nil {
			errorsCh <- err
			return
		}
		result <- value
	}()
	<-firstStarted
	manager.InvalidateClearance(1)
	close(releaseFirst)
	select {
	case err := <-errorsCh:
		t.Fatal(err)
	case value := <-result:
		if value.client != second || calls.Load() != 2 || first.closedIdle != 1 {
			t.Fatalf("client result=%#v calls=%d firstClosed=%d", value, calls.Load(), first.closedIdle)
		}
	case <-time.After(time.Second):
		t.Fatal("client creation did not recover after invalidation")
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

func TestUnavailablePrimaryUsesConfiguredDirectFallback(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute)
	config := domain.DefaultOperationsConfig()
	config.Fallbacks[domain.ScopeWeb] = domain.FallbackConfig{Mode: domain.FallbackModeDirect}
	manager := NewManager(fallbackEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{{
			ID: 1, Name: "cooling", Scope: domain.ScopeWeb, Enabled: true, CooldownUntil: &until,
		}}},
		config: config,
	}, cipher)
	ctx, trace := WithTrace(context.Background())
	lease, err := manager.Acquire(ctx, domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 0 || lease.NodeName != "direct" || lease.ProxyURL != "" {
		t.Fatalf("direct fallback lease = %#v", lease)
	}
	selection, ok := trace.Selection(domain.ScopeWeb)
	if !ok || selection.NodeID != 0 || selection.NodeName != "direct" || selection.Proxied {
		t.Fatalf("direct fallback selection = %#v, ok=%v", selection, ok)
	}
}

func TestUnavailableBuildPrimaryUsesConfiguredDirectFallbackTransport(t *testing.T) {
	until := time.Now().Add(time.Minute)
	config := domain.DefaultOperationsConfig()
	config.Fallbacks[domain.ScopeBuild] = domain.FallbackConfig{Mode: domain.FallbackModeDirect}
	manager := NewManager(fallbackEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{{
			ID: 1, Name: "cooling", Scope: domain.ScopeBuild, Enabled: true, CooldownUntil: &until,
		}}},
		config: config,
	}, nil)
	ctx, trace := WithTrace(context.Background())
	lease, configured, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "account")
	if err != nil || configured || lease != nil {
		t.Fatalf("direct fallback transport lease=%#v configured=%v err=%v", lease, configured, err)
	}
	selection, ok := trace.Selection(domain.ScopeBuild)
	if !ok || selection.NodeID != 0 || selection.NodeName != "direct" || selection.Proxied {
		t.Fatalf("direct fallback selection = %#v, ok=%v", selection, ok)
	}
}

func TestFixedFallbackIsReservedFromPrimarySelection(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	primaryURL, err := cipher.Encrypt("http://primary.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	fallbackURL, err := cipher.Encrypt("http://fallback.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	config := domain.DefaultOperationsConfig()
	config.Fallbacks[domain.ScopeBuild] = domain.FallbackConfig{Mode: domain.FallbackModeFixed, NodeID: 2}
	manager := NewManager(fallbackEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{
			{ID: 1, Name: "primary", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: primaryURL},
			{ID: 2, Name: "fallback", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: fallbackURL},
		}},
		config: config,
	}, cipher)
	lease, configured, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "reserved-fallback")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if !configured || lease.NodeID != 1 {
		t.Fatalf("primary lease=%#v configured=%v", lease, configured)
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

func TestAcquireCredentialUsesExplicitBoundNode(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := cipher.Encrypt("http://bound-node.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 1, Name: "pool-node", Scope: domain.ScopeBuild, Enabled: true, Health: 1},
		{ID: 2, Name: "bound-node", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL},
	}}, cipher)
	lease, err := manager.AcquireCredential(context.Background(), domain.ScopeBuild, accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderBuild, EgressNodeID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 2 || lease.NodeName != "bound-node" {
		t.Fatalf("bound lease = node %d (%q)", lease.NodeID, lease.NodeName)
	}
}

func TestAcquireCredentialDoesNotRouteDirectWhenBoundNodeHasNoProxy(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 2, Name: "empty-node", Scope: domain.ScopeBuild, Enabled: true, Health: 1},
	}}, cipher)
	_, err = manager.AcquireCredential(context.Background(), domain.ScopeBuild, accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderBuild, EgressNodeID: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "未配置代理地址") {
		t.Fatalf("bound node without proxy error = %v", err)
	}
}

func TestAcquireCredentialDoesNotFallbackWhenBoundNodeIsUnavailable(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 1, Name: "pool-node", Scope: domain.ScopeBuild, Enabled: true, Health: 1},
		{ID: 2, Name: "disabled-node", Scope: domain.ScopeBuild, Enabled: false, Health: 1},
	}}, cipher)
	_, err = manager.AcquireCredential(context.Background(), domain.ScopeBuild, accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderBuild, EgressNodeID: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "已禁用") {
		t.Fatalf("bound unavailable error = %v", err)
	}
}

func TestAcquireCredentialUsesConfiguredFixedFallbackWhenBoundNodeIsUnavailable(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := cipher.Encrypt("http://fixed-fallback.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	config := domain.DefaultOperationsConfig()
	config.Fallbacks[domain.ScopeBuild] = domain.FallbackConfig{Mode: domain.FallbackModeFixed, NodeID: 2}
	manager := NewManager(fallbackEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{
			{ID: 1, Name: "disabled", Scope: domain.ScopeBuild, Enabled: false},
			{ID: 2, Name: "fixed-fallback", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL},
		}},
		config: config,
	}, cipher)
	lease, err := manager.AcquireCredential(context.Background(), domain.ScopeBuild, accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderBuild, EgressNodeID: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 2 || lease.NodeName != "fixed-fallback" || lease.ProxyURL != "http://fixed-fallback.example:8080" {
		t.Fatalf("fixed fallback lease = %#v", lease)
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

func TestHealthySuccessFeedbackSkipsRepositoryReadAndWrite(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "healthy", Scope: domain.ScopeBuild, Enabled: true, Health: 1}}
	manager := NewManager(repository, cipher)
	lease, configured, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "")
	if err != nil || !configured || lease == nil {
		t.Fatalf("lease = %#v, configured = %v, err = %v", lease, configured, err)
	}
	lease.Release()

	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, http.StatusOK, nil)
	if repository.reads != 0 || repository.updates != 0 {
		t.Fatalf("healthy success performed repository I/O: reads=%d updates=%d", repository.reads, repository.updates)
	}
}

func TestRecoveringSuccessFeedbackPersistsHealthTransition(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "recovering", Scope: domain.ScopeBuild, Enabled: true, Health: 0.8, FailureCount: 1, LastError: "transport error"}}
	manager := NewManager(repository, cipher)
	lease, configured, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "")
	if err != nil || !configured || lease == nil {
		t.Fatalf("lease = %#v, configured = %v, err = %v", lease, configured, err)
	}
	lease.Release()

	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, http.StatusOK, nil)
	if repository.reads != 1 || repository.updates != 1 {
		t.Fatalf("recovery I/O: reads=%d updates=%d", repository.reads, repository.updates)
	}
	if repository.node.Health != 0.9 || repository.node.FailureCount != 0 || repository.node.LastError != "" {
		t.Fatalf("recovered node = %#v", repository.node)
	}
}

func TestExpiredHealthySnapshotRechecksRepositoryOnSuccess(t *testing.T) {
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "healthy", Scope: domain.ScopeBuild, Enabled: true, Health: 1}}
	manager := NewManager(repository, nil)
	if _, err := manager.listNodes(context.Background(), domain.ScopeBuild, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	manager.nodeMu.Lock()
	manager.healthyNodes[1] = time.Now().UTC().Add(-time.Second)
	manager.nodeMu.Unlock()
	repository.node = domain.Node{ID: 1, Name: "recovering", Scope: domain.ScopeBuild, Enabled: true, Health: 0.8, FailureCount: 1, LastError: "transport error"}

	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, http.StatusOK, nil)
	if repository.reads != 1 || repository.updates != 1 {
		t.Fatalf("expired health state did not recheck repository: reads=%d updates=%d", repository.reads, repository.updates)
	}
}

func TestNodeSnapshotReplacementRemovesRetiredHealthState(t *testing.T) {
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "first", Scope: domain.ScopeBuild, Enabled: true, Health: 1}}
	manager := NewManager(repository, nil)
	now := time.Now().UTC()
	if _, err := manager.listNodes(context.Background(), domain.ScopeBuild, now); err != nil {
		t.Fatal(err)
	}
	if !manager.cachedNodeIsHealthy(1) {
		t.Fatal("initial node was not cached as healthy")
	}
	repository.node = domain.Node{ID: 2, Name: "replacement", Scope: domain.ScopeBuild, Enabled: true, Health: 1}
	manager.nodeMu.Lock()
	snapshot := manager.nodes[domain.ScopeBuild]
	snapshot.expiresAt = now.Add(-time.Second)
	manager.nodes[domain.ScopeBuild] = snapshot
	manager.nodeMu.Unlock()
	if _, err := manager.listNodes(context.Background(), domain.ScopeBuild, now); err != nil {
		t.Fatal(err)
	}
	if manager.cachedNodeIsHealthy(1) {
		t.Fatal("retired node retained healthy cache state")
	}
	if !manager.cachedNodeIsHealthy(2) {
		t.Fatal("replacement node was not cached as healthy")
	}
}

func TestConcurrentFailurePreventsStaleHealthySnapshotInstall(t *testing.T) {
	repository := &blockingEgressRepository{
		node:        domain.Node{ID: 1, Name: "healthy", Scope: domain.ScopeBuild, Enabled: true, Health: 1},
		listStarted: make(chan struct{}),
		listRelease: make(chan struct{}),
	}
	manager := NewManager(repository, nil)
	loaded := make(chan []domain.Node, 1)
	loadErrors := make(chan error, 1)
	go func() {
		values, err := manager.listNodes(context.Background(), domain.ScopeBuild, time.Now().UTC())
		if err != nil {
			loadErrors <- err
			return
		}
		loaded <- values
	}()
	<-repository.listStarted
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, 0, errors.New("proxy timeout"))
	close(repository.listRelease)
	select {
	case err := <-loadErrors:
		t.Fatal(err)
	case values := <-loaded:
		if len(values) != 1 || values[0].FailureCount != 1 || values[0].CooldownUntil == nil {
			t.Fatalf("stale list result was returned after invalidation: %#v", values)
		}
	case <-time.After(time.Second):
		t.Fatal("node list did not complete")
	}
	if manager.cachedNodeIsHealthy(1) {
		t.Fatal("stale list result restored healthy cache state after failure feedback")
	}
	values, err := manager.listNodes(context.Background(), domain.ScopeBuild, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].FailureCount != 1 || values[0].CooldownUntil == nil {
		t.Fatalf("reloaded node = %#v", values)
	}
}

func TestForgetClearancePreventsStaleNodeSnapshotInstall(t *testing.T) {
	repository := &blockingEgressRepository{
		node:        domain.Node{ID: 1, Name: "before", Scope: domain.ScopeBuild, Enabled: true, Health: 1},
		listStarted: make(chan struct{}),
		listRelease: make(chan struct{}),
	}
	manager := NewManager(repository, nil)
	loaded := make(chan []domain.Node, 1)
	loadErrors := make(chan error, 1)
	go func() {
		values, err := manager.listNodes(context.Background(), domain.ScopeBuild, time.Now().UTC())
		if err != nil {
			loadErrors <- err
			return
		}
		loaded <- values
	}()
	<-repository.listStarted
	if _, err := repository.UpdateEgressNode(context.Background(), domain.Node{ID: 2, Name: "after", Scope: domain.ScopeBuild, Enabled: true, Health: 1}); err != nil {
		t.Fatal(err)
	}
	manager.ForgetClearance(1)
	close(repository.listRelease)

	select {
	case err := <-loadErrors:
		t.Fatal(err)
	case values := <-loaded:
		if len(values) != 1 || values[0].Name != "after" {
			t.Fatalf("stale node snapshot returned after administrative invalidation: %#v", values)
		}
	case <-time.After(time.Second):
		t.Fatal("node list did not complete")
	}

	manager.nodeMu.RLock()
	snapshot := manager.nodes[domain.ScopeBuild]
	_, staleHealthy := manager.healthyNodes[1]
	_, currentHealthy := manager.healthyNodes[2]
	manager.nodeMu.RUnlock()
	if len(snapshot.values) != 1 || snapshot.values[0].Name != "after" || staleHealthy || !currentHealthy {
		t.Fatalf("runtime node state was not replaced cleanly: snapshot=%#v staleHealthy=%v currentHealthy=%v", snapshot, staleHealthy, currentHealthy)
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

func TestOperationsConfigSnapshotAvoidsRepeatedRepositoryReads(t *testing.T) {
	repository := &countingFallbackRepository{config: domain.DefaultOperationsConfig()}
	manager := NewManager(repository, nil)
	for range 2 {
		lease, configured, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "")
		if err != nil || configured || lease != nil {
			t.Fatalf("lease=%#v configured=%v err=%v", lease, configured, err)
		}
	}
	if repository.configCalls != 1 {
		t.Fatalf("operations config reads = %d, want 1", repository.configCalls)
	}
}

func TestOperationsConfigSnapshotCanBeInvalidated(t *testing.T) {
	repository := &countingFallbackRepository{config: domain.DefaultOperationsConfig()}
	manager := NewManager(repository, nil)
	first, _, err := manager.fallbackFor(context.Background(), domain.ScopeWeb, time.Now().UTC())
	if err != nil || first.Mode != domain.FallbackModeNone {
		t.Fatalf("first fallback=%#v err=%v", first, err)
	}
	repository.config.Fallbacks[domain.ScopeWeb] = domain.FallbackConfig{Mode: domain.FallbackModeDirect}
	manager.InvalidateOperationsConfig()
	second, _, err := manager.fallbackFor(context.Background(), domain.ScopeWeb, time.Now().UTC())
	if err != nil || second.Mode != domain.FallbackModeDirect {
		t.Fatalf("second fallback=%#v err=%v", second, err)
	}
	if repository.configCalls != 2 {
		t.Fatalf("operations config reads = %d, want 2", repository.configCalls)
	}
}

type egressRepositoryTestStub struct{ nodes []domain.Node }

type fallbackEgressRepository struct {
	egressRepositoryTestStub
	config    domain.OperationsConfig
	configErr error
}

type countingFallbackRepository struct {
	egressRepositoryTestStub
	config      domain.OperationsConfig
	configCalls int
}

func (r *countingFallbackRepository) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	r.configCalls++
	return r.config, nil
}

func (r fallbackEgressRepository) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	if r.configErr != nil {
		return domain.OperationsConfig{}, r.configErr
	}
	return r.config, nil
}

func managerHasClientForNode(manager *Manager, nodeID uint64) bool {
	manager.clientMu.Lock()
	defer manager.clientMu.Unlock()
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
	reads   int
	updates int
}

type blockingEgressRepository struct {
	egressRepositoryTestStub
	mu          sync.Mutex
	node        domain.Node
	listStarted chan struct{}
	listRelease chan struct{}
	listOnce    sync.Once
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
	r.reads++
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

func (r *blockingEgressRepository) ListEgressNodes(_ context.Context, scope domain.Scope, _ repository.SortQuery) ([]domain.Node, error) {
	r.mu.Lock()
	node := r.node
	r.mu.Unlock()
	r.listOnce.Do(func() {
		close(r.listStarted)
		<-r.listRelease
	})
	if scope != "" && node.Scope != scope {
		return nil, nil
	}
	return []domain.Node{node}, nil
}

func (r *blockingEgressRepository) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.node.ID != id {
		return domain.Node{}, errors.New("not found")
	}
	return r.node, nil
}

func (r *blockingEgressRepository) UpdateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	r.mu.Lock()
	r.node = value
	r.mu.Unlock()
	return value, nil
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
func (s egressRepositoryTestStub) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	for _, node := range s.nodes {
		if node.ID == id {
			return node, nil
		}
	}
	return domain.Node{}, errors.New("not found")
}

func BenchmarkManagerAcquireCachedBuild(b *testing.B) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		b.Fatal(err)
	}
	node := domain.Node{ID: 1, Name: "build", Scope: domain.ScopeBuild, Enabled: true, Health: 1}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{node}}, cipher)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(int, *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}}, nil
	}
	lease, err := manager.Acquire(context.Background(), domain.ScopeBuild, "")
	if err != nil {
		b.Fatal(err)
	}
	lease.Release()
	manager.nodeMu.Lock()
	manager.nodes[domain.ScopeBuild] = cachedNodeSnapshot{values: []domain.Node{node}, expiresAt: time.Now().Add(time.Hour)}
	manager.nodeMu.Unlock()

	b.ReportAllocs()
	b.RunParallel(func(worker *testing.PB) {
		for worker.Next() {
			lease, acquireErr := manager.Acquire(context.Background(), domain.ScopeBuild, "")
			if acquireErr != nil {
				b.Error(acquireErr)
				continue
			}
			lease.Release()
		}
	})
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
