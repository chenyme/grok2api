package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type responseHeaderTimeoutError struct{}

func (responseHeaderTimeoutError) Error() string   { return "http2: timeout awaiting response headers" }
func (responseHeaderTimeoutError) Timeout() bool   { return true }
func (responseHeaderTimeoutError) Temporary() bool { return true }

func TestForgetClearancesEvictsSelectedNodesInOneBatch(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	first := &scriptedRequestClient{}
	second := &scriptedRequestClient{}
	untouched := &scriptedRequestClient{}
	manager.clients[clientCacheKey{nodeID: 1, scope: domain.ScopeBuild}] = cachedClient{client: first}
	manager.clients[clientCacheKey{nodeID: 2, scope: domain.ScopeWeb}] = cachedClient{client: second}
	manager.clients[clientCacheKey{nodeID: 3, scope: domain.ScopeConsole}] = cachedClient{client: untouched}
	manager.clearances["node:1"] = clearanceState{cookies: "one"}
	manager.clearances["node:2:sticky"] = clearanceState{cookies: "two"}
	manager.clearances["node:3"] = clearanceState{cookies: "three"}
	manager.nodes[domain.ScopeBuild] = cachedNodeSnapshot{values: []domain.Node{{ID: 1}}}
	manager.healthyNodes[1] = time.Now().UTC()

	manager.ForgetClearances([]uint64{1, 2, 1})

	if _, exists := manager.clearances["node:1"]; exists {
		t.Fatal("node 1 clearance was retained")
	}
	if _, exists := manager.clearances["node:2:sticky"]; exists {
		t.Fatal("node 2 clearance was retained")
	}
	if _, exists := manager.clearances["node:3"]; !exists {
		t.Fatal("unselected clearance was removed")
	}
	if len(manager.clients) != 1 || first.closedIdle != 1 || second.closedIdle != 1 || untouched.closedIdle != 0 {
		t.Fatalf("clients=%d closed=(%d,%d,%d)", len(manager.clients), first.closedIdle, second.closedIdle, untouched.closedIdle)
	}
	if len(manager.nodes) != 0 || len(manager.healthyNodes) != 0 {
		t.Fatalf("node snapshots were not invalidated: nodes=%d healthy=%d", len(manager.nodes), len(manager.healthyNodes))
	}
	if manager.clientVersions[1] != 1 || manager.clientVersions[2] != 1 || manager.clientVersions[3] != 0 {
		t.Fatalf("client versions = %#v", manager.clientVersions)
	}
}

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
	if _, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false, ""); err != nil {
		t.Fatal(err)
	}
	manager.UpdateBuildResponseHeaderTimeout(7 * time.Minute)
	if len(clients) != 1 || clients[0].closedIdle != 1 {
		t.Fatalf("old clients=%d closed=%d", len(clients), clients[0].closedIdle)
	}
	if _, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false, ""); err != nil {
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

func TestCanceledRequestDoesNotPenalizeEgress(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		transportErr error
	}{
		{name: "canceled transport", transportErr: context.Canceled},
		{name: "wrapped canceled transport", transportErr: fmt.Errorf("request failed: %w", context.Canceled)},
		{name: "client closed status", status: clientClosedRequestStatus},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "fixed", Scope: domain.ScopeBuild, Enabled: true, Health: 1}}
			manager := NewManager(repository, nil)
			manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, test.status, test.transportErr)
			if repository.updates != 0 || repository.node.Health != 1 || repository.node.FailureCount != 0 || repository.node.CooldownUntil != nil {
				t.Fatalf("canceled request changed node health: updates=%d node=%#v", repository.updates, repository.node)
			}
		})
	}
}

func TestCanceledRequestDoesNotInvalidateDirectClient(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		transportErr error
	}{
		{name: "canceled transport", transportErr: context.Canceled},
		{name: "client closed status", status: clientClosedRequestStatus},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(egressRepositoryTestStub{}, nil)
			key := clientCacheKey{nodeID: 0, scope: domain.ScopeBuild, fingerprint: "direct"}
			manager.clients[key] = cachedClient{client: &scriptedRequestClient{}}
			manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 0, test.status, test.transportErr)
			if _, exists := manager.clients[key]; !exists {
				t.Fatal("canceled request invalidated the direct Build client")
			}
		})
	}
}

func TestConsoleAssetForbiddenDoesNotPenalizeProxy(t *testing.T) {
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "console", Scope: domain.ScopeConsole, Enabled: true, Health: 1}}
	manager := NewManager(repository, nil)
	manager.FeedbackForScope(context.Background(), domain.ScopeConsoleAsset, 1, http.StatusForbidden, nil)
	if repository.updates != 0 || repository.node.Health != 1 || repository.node.CooldownUntil != nil {
		t.Fatalf("Console asset object rejection changed proxy health: updates=%d node=%#v", repository.updates, repository.node)
	}
}

func TestProbeEgressNodeLogsSuccessWithoutProxyCredentials(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("socks5://user:secret@proxy.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 42, Name: "web-us", Scope: domain.ScopeWeb, EncryptedProxyURL: encryptedProxy}}
	manager := NewManager(repository, cipher)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(int, *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ip":"203.0.113.8"}`))}, nil
		}}, nil
	}
	var output bytes.Buffer
	manager.SetLogger(slog.New(slog.NewTextHandler(&output, nil)))

	result, err := manager.probeEgressEndpoint(context.Background(), preparedEgressProbe{
		nodeID: 42, nodeName: "web-us", nodeScope: domain.ScopeWeb, proxyURL: "socks5://user:secret@proxy.example:1080",
	}, "test", "ipv4", "https://probe.example/ip")
	if err != nil || result.Status != domain.ProbeStatusHealthy || result.ExitIP != "203.0.113.8" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	logOutput := output.String()
	if !strings.Contains(logOutput, "egress_probe_succeeded") || !strings.Contains(logOutput, "node_id=42") || !strings.Contains(logOutput, "exit_ip=203.0.113.8") {
		t.Fatalf("probe log missing fields: %s", logOutput)
	}
	if strings.Contains(logOutput, "user") || strings.Contains(logOutput, "secret") || strings.Contains(logOutput, "proxy.example") {
		t.Fatalf("probe log leaked proxy credentials: %s", logOutput)
	}
}

func TestProbeEgressNodeLogsSanitizedFailureStage(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("socks5://user:secret@proxy.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 7, Name: "web-jp", Scope: domain.ScopeWeb, EncryptedProxyURL: encryptedProxy}}
	manager := NewManager(repository, cipher)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(_ int, request *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(request.Context())
			trace.ConnectStart("tcp", "proxy.example:1080")
			trace.ConnectDone("tcp", "proxy.example:1080", errors.New("connection refused"))
			return nil, errors.New("dial socks5://user:secret@proxy.example:1080 failed; token=abc123")
		}}, nil
	}
	var output bytes.Buffer
	manager.SetLogger(slog.New(slog.NewTextHandler(&output, nil)))

	result, err := manager.probeEgressEndpoint(context.Background(), preparedEgressProbe{
		nodeID: 7, nodeName: "web-jp", nodeScope: domain.ScopeWeb, proxyURL: "socks5://user:secret@proxy.example:1080",
	}, "test", "ipv4", "https://probe.example/ip")
	if err == nil || result.Error != "代理连接失败" || result.LatencyMS < 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	logOutput := output.String()
	if !strings.Contains(logOutput, "egress_probe_failed") || !strings.Contains(logOutput, "stage=connect") || !strings.Contains(logOutput, "token=[redacted]") || !strings.Contains(logOutput, "socks5://***:***@") {
		t.Fatalf("probe failure log missing fields or redaction: %s", logOutput)
	}
	if strings.Contains(logOutput, "secret") || strings.Contains(logOutput, "abc123") {
		t.Fatalf("probe failure log leaked credentials: %s", logOutput)
	}
}

func TestProbeEgressNodeClassifiesTLSFailure(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(_ int, request *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(request.Context())
			trace.ConnectStart("tcp", "proxy.example:1080")
			trace.ConnectDone("tcp", "proxy.example:1080", nil)
			trace.TLSHandshakeStart()
			trace.TLSHandshakeDone(tls.ConnectionState{}, errors.New("certificate rejected"))
			return nil, errors.New("TLS handshake failed")
		}}, nil
	}
	var output bytes.Buffer
	manager.SetLogger(slog.New(slog.NewTextHandler(&output, nil)))

	result, err := manager.probeEgressEndpoint(context.Background(), preparedEgressProbe{
		nodeID: 8, nodeName: "tls-failure", nodeScope: domain.ScopeBuild, proxyURL: "http://proxy.example:1080",
	}, domain.ProbeProviderCloudflare, "ipv4", cloudflareIPv4ProbeEndpoint)
	if err == nil || result.Error != "代理连接失败" || result.LatencyMS < 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	logOutput := output.String()
	if !strings.Contains(logOutput, "stage=tls") || strings.Contains(logOutput, "tls_ms=0") {
		t.Fatalf("TLS failure log missing stage or duration: %s", logOutput)
	}
}

func TestProbeEgressNodeClassifiesFirstByteFailure(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(_ int, request *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(request.Context())
			trace.ConnectStart("tcp", "proxy.example:1080")
			trace.ConnectDone("tcp", "proxy.example:1080", nil)
			trace.TLSHandshakeStart()
			trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
			return nil, errors.New("timeout awaiting response headers")
		}}, nil
	}
	var output bytes.Buffer
	manager.SetLogger(slog.New(slog.NewTextHandler(&output, nil)))

	result, err := manager.probeEgressEndpoint(context.Background(), preparedEgressProbe{
		nodeID: 9, nodeName: "first-byte-failure", nodeScope: domain.ScopeBuild, proxyURL: "http://proxy.example:1080",
	}, domain.ProbeProviderCloudflare, "ipv4", cloudflareIPv4ProbeEndpoint)
	if err == nil || result.Error != "代理连接失败" || result.LatencyMS < 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	logOutput := output.String()
	if !strings.Contains(logOutput, "stage=first_byte") || strings.Contains(logOutput, "first_byte_ms=0") {
		t.Fatalf("first-byte failure log missing stage or duration: %s", logOutput)
	}
}

func TestProbeEgressNodeKeepsUntracedFailureAtExecuteRequest(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(_ int, _ *http.Request) (*http.Response, error) {
			return nil, errors.New("request execution failed")
		}}, nil
	}
	var output bytes.Buffer
	manager.SetLogger(slog.New(slog.NewTextHandler(&output, nil)))

	_, err := manager.probeEgressEndpoint(context.Background(), preparedEgressProbe{
		nodeID: 10, nodeName: "untraced-failure", nodeScope: domain.ScopeBuild, proxyURL: "http://proxy.example:1080",
	}, domain.ProbeProviderCloudflare, "ipv4", cloudflareIPv4ProbeEndpoint)
	if err == nil {
		t.Fatal("expected probe failure")
	}
	if logOutput := output.String(); !strings.Contains(logOutput, "stage=execute_request") {
		t.Fatalf("untraced failure was misclassified: %s", logOutput)
	}
}

func TestProbeEgressNodeKeepsIPv4AndIPv6ResultsSeparate(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	node := domain.Node{ID: 9, Name: "dual", Scope: domain.ScopeBuild, EncryptedProxyURL: encryptedProxy}
	manager := NewManager(&mutableEgressRepository{node: node}, cipher)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(_ int, request *http.Request) (*http.Response, error) {
			payload := `{"ip":"198.51.100.9"}`
			if request.URL.Hostname() == "2606:4700:4700::1111" {
				payload = `{"ip":"2001:db8::9"}`
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload))}, nil
		}}, nil
	}

	result, err := manager.ProbeEgressNode(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.ProbeStatusHealthy || result.IPv4.ExitIP != "198.51.100.9" || result.IPv6.ExitIP != "2001:db8::9" {
		t.Fatalf("dual-stack result = %#v", result)
	}
}

func TestProbeEgressNodeIsHealthyWhenOnlyIPv4Works(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	node := domain.Node{ID: 10, Name: "v4-only", Scope: domain.ScopeBuild, EncryptedProxyURL: encryptedProxy}
	manager := NewManager(&mutableEgressRepository{node: node}, cipher)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(_ int, request *http.Request) (*http.Response, error) {
			if request.URL.Hostname() == "2606:4700:4700::1111" {
				return nil, errors.New("network is unreachable")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ip":"198.51.100.10"}`))}, nil
		}}, nil
	}

	result, err := manager.ProbeEgressNode(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.ProbeStatusHealthy || result.IPv4.Status != domain.ProbeStatusHealthy || result.IPv6.Status != domain.ProbeStatusUnhealthy || result.IPv6.Error == "" {
		t.Fatalf("IPv4-only result = %#v", result)
	}
}

func TestProbeEgressNodeUsesConfiguredCloudflareEndpoints(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	config := domain.DefaultOperationsConfig()
	config.ProbeProvider = domain.ProbeProviderCloudflare
	node := domain.Node{ID: 11, Name: "cloudflare", Scope: domain.ScopeBuild, EncryptedProxyURL: encryptedProxy}
	repository := fallbackEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{node}},
		config:                   config,
	}
	manager := NewManager(repository, cipher)
	var requested sync.Map
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(_ int, request *http.Request) (*http.Response, error) {
			requested.Store(request.URL.String(), true)
			exitIP := "198.51.100.11"
			if request.URL.Hostname() == "2606:4700:4700::1111" {
				exitIP = "2001:db8::11"
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("fl=test\nip=" + exitIP + "\ncolo=SJC\n"))}, nil
		}}, nil
	}

	result, err := manager.ProbeEgressNode(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != domain.ProbeProviderCloudflare || result.IPv4.ExitIP != "198.51.100.11" || result.IPv6.ExitIP != "2001:db8::11" {
		t.Fatalf("Cloudflare probe result = %#v", result)
	}
	for _, endpoint := range []string{cloudflareIPv4ProbeEndpoint, cloudflareIPv6ProbeEndpoint} {
		if _, ok := requested.Load(endpoint); !ok {
			t.Fatalf("Cloudflare endpoint %q was not requested", endpoint)
		}
	}
}

func TestProbeEndpointsDefaultToCloudflare(t *testing.T) {
	if ipv4, ipv6 := probeEndpoints(""); ipv4 != cloudflareIPv4ProbeEndpoint || ipv6 != cloudflareIPv6ProbeEndpoint {
		t.Fatalf("default probe endpoints = %q, %q", ipv4, ipv6)
	}
}

func TestDecodeProbeIPSupportsJSONAndCloudflareTrace(t *testing.T) {
	for name, body := range map[string]string{
		"json":             `{"ip":"203.0.113.12"}`,
		"cloudflare trace": "fl=test\nip=2001:db8::12\ncolo=SJC\n",
	} {
		t.Run(name, func(t *testing.T) {
			value, err := decodeProbeIP([]byte(body))
			if err != nil || (value != "203.0.113.12" && value != "2001:db8::12") {
				t.Fatalf("decodeProbeIP() = %q, %v", value, err)
			}
		})
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
		_, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false, "")
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
	if _, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false, ""); err != nil {
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

	if _, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false, ""); err != nil {
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
	if _, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false, ""); err != nil {
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
		value, err := manager.clientFor(1, domain.ScopeBuild, "", "", "", false, "")
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

func TestConsoleAssetCredentialPrefersDedicatedNodeWithoutCookies(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptProxy := func(raw string) string {
		t.Helper()
		value, encryptErr := cipher.Encrypt(raw)
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		return value
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: encryptProxy("http://web.example:8080")},
		{ID: 2, Name: "console", Scope: domain.ScopeConsole, Enabled: true, Health: 1, EncryptedProxyURL: encryptProxy("http://console.example:8080")},
		{ID: 3, Name: "console-assets", Scope: domain.ScopeConsoleAsset, Enabled: true, Health: 1, EncryptedProxyURL: encryptProxy("http://assets.example:8080"), EncryptedCloudflareCookie: "damaged-node-cookie", UserAgent: "asset-agent"},
	}}, cipher)
	lease, err := manager.AcquireCredential(context.Background(), domain.ScopeConsoleAsset, accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderConsole, EncryptedCloudflareCookie: "damaged-account-cookie",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 3 || lease.Scope != domain.ScopeConsoleAsset || lease.UserAgent != "asset-agent" {
		t.Fatalf("asset lease = %#v", lease)
	}
	if lease.CFCookies != "" {
		t.Fatalf("anonymous Console asset lease exposed cookies: %q", lease.CFCookies)
	}
}

func TestConsoleAssetCredentialPreservesExplicitConsoleBinding(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	boundProxy, err := cipher.Encrypt("http://bound-console.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	assetProxy, err := cipher.Encrypt("http://assets.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 2, Name: "bound-console", Scope: domain.ScopeConsole, Enabled: true, Health: 1, EncryptedProxyURL: boundProxy},
		{ID: 3, Name: "console-assets", Scope: domain.ScopeConsoleAsset, Enabled: true, Health: 1, EncryptedProxyURL: assetProxy},
	}}, cipher)
	lease, err := manager.AcquireCredential(context.Background(), domain.ScopeConsoleAsset, accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderConsole, EgressNodeID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 2 || lease.NodeName != "bound-console" {
		t.Fatalf("explicit Console binding was not preserved: node=%d name=%q", lease.NodeID, lease.NodeName)
	}
}

func TestConsoleAssetClientDoesNotEvictPrimaryConsoleClient(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := cipher.Encrypt("http://console.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := cipher.Encrypt("cf_clearance=console")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 2, Name: "console", Scope: domain.ScopeConsole, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL,
	}}}, cipher)
	credential := accountdomain.Credential{
		ID: 42, Provider: accountdomain.ProviderConsole, EgressNodeID: 2, EncryptedCloudflareCookie: cookie,
	}
	primary, err := manager.AcquireCredential(context.Background(), domain.ScopeConsole, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Release()
	asset, err := manager.AcquireCredential(context.Background(), domain.ScopeConsoleAsset, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer asset.Release()
	primaryAgain, err := manager.AcquireCredential(context.Background(), domain.ScopeConsole, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer primaryAgain.Release()
	if primary.client != primaryAgain.client {
		t.Fatal("Console asset download evicted the primary Console connection pool")
	}
	if primary.client == asset.client || len(manager.clients) != 2 {
		t.Fatalf("primary and anonymous asset clients were not isolated: cached=%d", len(manager.clients))
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
	if !lease.freshTunnel {
		t.Fatal("explicit proxy-pool lease must request a fresh Build tunnel")
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

func TestQualityProbeCanUseDisabledCoolingBoundNode(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("http://127.0.0.1:18888")
	if err != nil {
		t.Fatal(err)
	}
	cooldown := time.Now().UTC().Add(time.Hour)
	repository := &synchronizedEgressRepository{node: domain.Node{
		ID: 1, Name: "quarantined", Scope: domain.ScopeBuild, Enabled: false,
		Health: 0, CooldownUntil: &cooldown, EncryptedProxyURL: encryptedProxy,
	}}
	manager := NewManager(repository, cipher)
	ordinary := WithEgressNode(context.Background(), 1)
	if lease, _, err := manager.AcquireIfConfigured(ordinary, domain.ScopeBuild, "ordinary"); err == nil {
		if lease != nil {
			lease.Release()
		}
		t.Fatal("ordinary request acquired a disabled node")
	}
	probe := WithQualityProbe(WithEgressNode(context.Background(), 1))
	lease, configured, err := manager.AcquireIfConfigured(probe, domain.ScopeBuild, "probe")
	if err != nil || !configured || lease == nil {
		t.Fatalf("quality probe acquire: configured=%v lease=%#v err=%v", configured, lease, err)
	}
	lease.Release()
}

func TestFixedProxyTransportFailureCoalescesRunningProbe(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "fixed", Scope: domain.ScopeBuild, Enabled: true, Health: 1}}
	manager := NewManager(repository, cipher)
	called := make(chan uint64, 1)
	release := make(chan struct{})
	manager.SetFailureProber(func(_ context.Context, id uint64) (domain.ProbeResult, error) {
		called <- id
		<-release
		return domain.ProbeResult{Status: domain.ProbeStatusHealthy}, nil
	})

	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, 0, errors.New("connection refused"))
	select {
	case id := <-called:
		if id != 1 {
			t.Fatalf("probed node = %d", id)
		}
	case <-time.After(time.Second):
		t.Fatal("transport failure did not schedule an immediate probe")
	}

	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, 0, errors.New("connection reset"))
	select {
	case <-called:
		t.Fatal("concurrent transport failures scheduled duplicate probes")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if _, err := manager.waitForFailureProbe(waitCtx, 1); err != nil {
		t.Fatal(err)
	}
}

func TestBoundFixedProxyWaitsForHealthyFailureProbe(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("http://127.0.0.1:18888")
	if err != nil {
		t.Fatal(err)
	}
	repository := &synchronizedEgressRepository{node: domain.Node{
		ID: 1, Name: "fixed", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy,
	}}
	manager := NewManager(repository, cipher)
	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	manager.SetFailureProber(func(_ context.Context, _ uint64) (domain.ProbeResult, error) {
		close(probeStarted)
		<-probeRelease
		repository.recoverTransportFailure()
		return domain.ProbeResult{Status: domain.ProbeStatusHealthy}, nil
	})
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, 0, errors.New("unexpected EOF"))
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("failure probe did not start")
	}

	type acquireResult struct {
		lease      *Lease
		configured bool
		err        error
	}
	acquired := make(chan acquireResult, 1)
	go func() {
		lease, configured, acquireErr := manager.AcquireIfConfigured(WithEgressNode(context.Background(), 1), domain.ScopeBuild, "account-2")
		acquired <- acquireResult{lease: lease, configured: configured, err: acquireErr}
	}()
	select {
	case result := <-acquired:
		if result.lease != nil {
			result.lease.Release()
		}
		t.Fatalf("bound retry returned before probe completed: configured=%v err=%v", result.configured, result.err)
	case <-time.After(50 * time.Millisecond):
	}
	close(probeRelease)
	select {
	case result := <-acquired:
		if result.err != nil || !result.configured || result.lease == nil {
			t.Fatalf("bound retry after healthy probe: configured=%v lease=%#v err=%v", result.configured, result.lease, result.err)
		}
		result.lease.Release()
	case <-time.After(time.Second):
		t.Fatal("bound retry did not resume after healthy probe")
	}
}

func TestBoundFixedProxyKeepsCooldownAfterUnhealthyFailureProbe(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("http://127.0.0.1:18888")
	if err != nil {
		t.Fatal(err)
	}
	repository := &synchronizedEgressRepository{node: domain.Node{
		ID: 1, Name: "fixed", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy,
	}}
	manager := NewManager(repository, cipher)
	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	manager.SetFailureProber(func(_ context.Context, _ uint64) (domain.ProbeResult, error) {
		close(probeStarted)
		<-probeRelease
		return domain.ProbeResult{Status: domain.ProbeStatusUnhealthy}, nil
	})
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, 0, errors.New("connection refused"))
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("failure probe did not start")
	}

	result := make(chan error, 1)
	go func() {
		lease, _, acquireErr := manager.AcquireIfConfigured(WithEgressNode(context.Background(), 1), domain.ScopeBuild, "account-2")
		if lease != nil {
			lease.Release()
		}
		result <- acquireErr
	}()
	select {
	case err := <-result:
		t.Fatalf("bound retry returned before probe completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(probeRelease)
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "正在冷却") {
			t.Fatalf("bound retry after unhealthy probe error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bound retry did not resume after unhealthy probe")
	}
}

func TestBoundFixedProxyProbeWaitHonorsRequestCancellation(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("http://127.0.0.1:18888")
	if err != nil {
		t.Fatal(err)
	}
	repository := &synchronizedEgressRepository{node: domain.Node{
		ID: 1, Name: "fixed", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy,
	}}
	manager := NewManager(repository, cipher)
	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	manager.SetFailureProber(func(_ context.Context, _ uint64) (domain.ProbeResult, error) {
		close(probeStarted)
		<-probeRelease
		return domain.ProbeResult{Status: domain.ProbeStatusUnhealthy}, nil
	})
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, 0, errors.New("connection refused"))
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("failure probe did not start")
	}

	ctx, cancel := context.WithCancel(WithEgressNode(context.Background(), 1))
	result := make(chan error, 1)
	go func() {
		_, _, acquireErr := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "account-2")
		result <- acquireErr
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled bound retry error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled bound retry stayed blocked on probe")
	}
	close(probeRelease)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if _, err := manager.waitForFailureProbe(waitCtx, 1); err != nil {
		t.Fatal(err)
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
	if !lease.sticky || !lease.proxyPool || lease.freshTunnel {
		t.Fatalf("account-template lease flags: sticky=%v proxyPool=%v freshTunnel=%v", lease.sticky, lease.proxyPool, lease.freshTunnel)
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

func TestOnDemandClearanceRefreshesOnlyAfterRejection(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	cookies, err := cipher.Encrypt("cf_clearance=existing")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "console", Scope: domain.ScopeConsole, Enabled: true, Health: 1,
		EncryptedCloudflareCookie: cookies, UserAgent: "Chrome/existing",
	}}
	solver := &clearanceSolverStub{}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "on_demand", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})

	first, err := manager.Acquire(context.Background(), domain.ScopeConsole, "account")
	if err != nil {
		t.Fatal(err)
	}
	if solver.calls != 0 || first.CFCookies != "cf_clearance=existing" {
		t.Fatalf("initial calls=%d cookies=%q", solver.calls, first.CFCookies)
	}
	first.InvalidateClearance()
	first.Release()

	second, err := manager.Acquire(context.Background(), domain.ScopeConsole, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if solver.calls != 1 || second.CFCookies != "cf_clearance=value-1" {
		t.Fatalf("refresh calls=%d cookies=%q", solver.calls, second.CFCookies)
	}
	stored, err := cipher.Decrypt(repository.node.EncryptedCloudflareCookie)
	if err != nil || stored != "cf_clearance=value-1" {
		t.Fatalf("stored cookies=%q err=%v", stored, err)
	}
}

func TestOnDemandClearanceSkipsBackgroundRefresh(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "console", Scope: domain.ScopeConsole, Enabled: true, Health: 1}}
	solver := &clearanceSolverStub{}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "on_demand", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Nanosecond})

	if err := manager.RefreshDueClearances(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if solver.calls != 0 {
		t.Fatalf("background refresh called solver %d times", solver.calls)
	}
}

func TestOnDemandClearanceKeepsLastSuccessfulDirectSolutionPastRefreshInterval(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	solver := &clearanceSolverStub{}
	manager := NewManager(egressRepositoryTestStub{}, cipher)
	manager.solver = solver
	manager.UpdateClearanceConfig(ClearanceConfig{
		Mode: "on_demand", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com",
		Timeout: time.Second, RefreshInterval: time.Nanosecond,
	})

	initial, err := manager.Acquire(context.Background(), domain.ScopeConsole, "account")
	if err != nil {
		t.Fatal(err)
	}
	if initial.CFCookies != "" || solver.calls != 0 {
		t.Fatalf("initial lease cookies=%q calls=%d", initial.CFCookies, solver.calls)
	}
	initial.InvalidateClearance()
	initial.Release()

	refreshed, err := manager.Acquire(context.Background(), domain.ScopeConsole, "account")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.CFCookies != "cf_clearance=value-1" || solver.calls != 1 {
		t.Fatalf("refreshed lease cookies=%q calls=%d", refreshed.CFCookies, solver.calls)
	}
	refreshed.Release()

	// The interval is deliberately expired. on_demand must keep using the last
	// successful runtime-only solution until an upstream rejection invalidates it.
	reused, err := manager.Acquire(context.Background(), domain.ScopeConsole, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Release()
	if reused.CFCookies != "cf_clearance=value-1" || reused.UserAgent != "Chrome/146 test" || solver.calls != 1 {
		t.Fatalf("reused lease=%#v calls=%d", reused, solver.calls)
	}
}

func TestOnDemandClearanceReusesPeerRefreshAfterDistributedLock(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	config := ClearanceConfig{
		Mode: "on_demand", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com",
		Timeout: time.Second, RefreshInterval: time.Hour,
	}
	oldCookies, err := cipher.Encrypt("cf_clearance=old")
	if err != nil {
		t.Fatal(err)
	}
	oldRefreshedAt := time.Now().UTC().Add(-time.Minute)
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "console", Scope: domain.ScopeConsole, Enabled: true, Health: 1,
		EncryptedCloudflareCookie: oldCookies, UserAgent: "Chrome/old",
		ClearanceFingerprint: clearanceFingerprint(config, ""), ClearanceBindingFingerprint: clearanceBindingFingerprint(config, ""),
		ClearanceRefreshedAt: &oldRefreshedAt,
	}}
	solver := &clearanceSolverStub{}
	firstManager := NewManager(repository, cipher)
	firstManager.solver = solver
	firstManager.SetClearanceLock(alwaysAcquiredDistributedLock{})
	firstManager.UpdateClearanceConfig(config)
	secondManager := NewManager(repository, cipher)
	secondManager.solver = solver
	secondManager.SetClearanceLock(alwaysAcquiredDistributedLock{})
	secondManager.UpdateClearanceConfig(config)

	firstOld, err := firstManager.Acquire(context.Background(), domain.ScopeConsole, "first")
	if err != nil {
		t.Fatal(err)
	}
	secondOld, err := secondManager.Acquire(context.Background(), domain.ScopeConsole, "second")
	if err != nil {
		t.Fatal(err)
	}
	firstOld.InvalidateClearance()
	secondOld.InvalidateClearance()
	firstOld.Release()
	secondOld.Release()

	firstFresh, err := firstManager.Acquire(context.Background(), domain.ScopeConsole, "first")
	if err != nil {
		t.Fatal(err)
	}
	firstFresh.Release()
	secondFresh, err := secondManager.Acquire(context.Background(), domain.ScopeConsole, "second")
	if err != nil {
		t.Fatal(err)
	}
	defer secondFresh.Release()
	if solver.calls != 1 || secondFresh.CFCookies != "cf_clearance=value-1" {
		t.Fatalf("peer refresh was not reused: calls=%d cookies=%q", solver.calls, secondFresh.CFCookies)
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

func TestNoChallengeClearanceDoesNotBlockOrRefreshRepeatedly(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1}}
	solver := &clearanceSolverStub{noCookies: true}
	config := ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour}
	firstManager := NewManager(repository, cipher)
	firstManager.solver = solver
	firstManager.UpdateClearanceConfig(config)
	for range 2 {
		lease, acquireErr := firstManager.Acquire(context.Background(), domain.ScopeWeb, "account")
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		if lease.CFCookies != "" || lease.UserAgent != "Chrome/146 test" {
			t.Fatalf("cookie-less lease = %#v", lease)
		}
		lease.Release()
	}

	secondManager := NewManager(repository, cipher)
	secondManager.solver = solver
	secondManager.UpdateClearanceConfig(config)
	lease, err := secondManager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if solver.calls != 1 || repository.node.ClearanceRefreshedAt == nil || repository.node.UserAgent != "Chrome/146 test" {
		t.Fatalf("cookie-less clearance was not reused: calls=%d node=%#v", solver.calls, repository.node)
	}
}

func TestRejectedNoChallengeClearanceForcesRefreshWithDistributedLock(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1}}
	solver := &clearanceSolverStub{noCookies: true}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	manager.SetClearanceLock(alwaysAcquiredDistributedLock{})
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})

	first, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	first.InvalidateClearance()
	first.Release()

	second, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	if solver.calls != 2 {
		t.Fatalf("rejected cookie-less clearance reused persisted state: calls=%d", solver.calls)
	}
}

func TestBackgroundRefreshDoesNotReuseRejectedNoChallengeClearance(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1}}
	solver := &clearanceSolverStub{noCookies: true}
	manager := NewManager(repository, cipher)
	manager.solver = solver
	manager.SetClearanceLock(alwaysAcquiredDistributedLock{})
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})

	lease, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	lease.InvalidateClearance()
	lease.Release()
	if err := manager.RefreshDueClearances(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if solver.calls != 2 {
		t.Fatalf("background refresh reused rejected cookie-less clearance: calls=%d", solver.calls)
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

type synchronizedEgressRepository struct {
	egressRepositoryTestStub
	mu   sync.Mutex
	node domain.Node
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
	calls     int
	proxyURL  string
	err       error
	noCookies bool
}

type alwaysAcquiredDistributedLock struct{}

func (alwaysAcquiredDistributedLock) Acquire(context.Context, string, time.Duration) (func(), bool, error) {
	return func() {}, true, nil
}

func (s *clearanceSolverStub) Solve(_ context.Context, _ ClearanceConfig, proxyURL string) (clearanceSolution, error) {
	s.calls++
	s.proxyURL = proxyURL
	if s.err != nil {
		return clearanceSolution{}, s.err
	}
	if s.noCookies {
		return clearanceSolution{UserAgent: "Chrome/146 test"}, nil
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

func (r *synchronizedEgressRepository) ListEgressNodes(_ context.Context, scope domain.Scope, _ repository.SortQuery) ([]domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if scope != "" && r.node.Scope != scope {
		return nil, nil
	}
	return []domain.Node{r.node}, nil
}

func (r *synchronizedEgressRepository) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.node.ID != id {
		return domain.Node{}, errors.New("not found")
	}
	return r.node, nil
}

func (r *synchronizedEgressRepository) UpdateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	r.mu.Lock()
	r.node = value
	r.mu.Unlock()
	return value, nil
}

func (r *synchronizedEgressRepository) recoverTransportFailure() {
	r.mu.Lock()
	r.node.Health = 1
	r.node.FailureCount = 0
	r.node.CooldownUntil = nil
	r.node.LastError = ""
	r.mu.Unlock()
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

func TestAccountIsolatedConnectionsSeparatesDirectClientsByAccount(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{}, cipher)
	manager.UpdateAccountIsolatedConnections(true)

	firstCtx := WithAccountIdentity(context.Background(), "grok_web_1")
	first, err := manager.AcquireCredential(firstCtx, domain.ScopeWeb, accountdomain.Credential{
		ID: 1, Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	secondCtx := WithAccountIdentity(context.Background(), "grok_web_2")
	second, err := manager.AcquireCredential(secondCtx, domain.ScopeWeb, accountdomain.Credential{
		ID: 2, Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()

	if first.client == second.client {
		t.Fatal("different accounts shared one TCP client pool while account isolation was enabled")
	}

	firstAgain, err := manager.AcquireCredential(firstCtx, domain.ScopeWeb, accountdomain.Credential{
		ID: 1, Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer firstAgain.Release()
	if firstAgain.client != first.client {
		t.Fatal("same account did not reuse its isolated connection pool")
	}
}

func TestAccountIsolatedConnectionsDisabledKeepsSharedDirectPool(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{}, cipher)
	manager.UpdateAccountIsolatedConnections(false)

	first, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{
		ID: 1, Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := manager.AcquireCredential(context.Background(), domain.ScopeWeb, accountdomain.Credential{
		ID: 2, Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if first.client != second.client {
		t.Fatal("shared direct pool was split while account isolation was disabled")
	}
}

func TestAccountIsolatedBuildEnvironmentDirectUsesDedicatedFactoryAndPools(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{}, cipher)
	manager.UpdateAccountIsolatedConnections(true)
	var regularCalls, environmentCalls int
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		regularCalls++
		return &scriptedRequestClient{}, nil
	}
	manager.newBuildEnvClient = func(time.Duration) (requestClient, error) {
		environmentCalls++
		return &scriptedRequestClient{}, nil
	}

	firstCtx := WithAccountIdentity(context.Background(), "grok_build_1")
	first, configured, err := manager.AcquireBuildEnvironmentDirectIfIsolated(firstCtx, "1")
	if err != nil || !configured {
		t.Fatal(err)
	}
	defer first.Release()
	second, configured, err := manager.AcquireBuildEnvironmentDirectIfIsolated(WithAccountIdentity(context.Background(), "grok_build_2"), "2")
	if err != nil || !configured {
		t.Fatal(err)
	}
	defer second.Release()
	firstAgain, configured, err := manager.AcquireBuildEnvironmentDirectIfIsolated(firstCtx, "1")
	if err != nil || !configured {
		t.Fatal(err)
	}
	defer firstAgain.Release()

	if regularCalls != 0 || environmentCalls != 2 {
		t.Fatalf("Build client factories: regular=%d environment=%d", regularCalls, environmentCalls)
	}
	if first.client == second.client || first.client != firstAgain.client {
		t.Fatal("environment-proxy Build pools were not isolated per account and reused within account")
	}
}

func TestBuildEnvironmentDirectKeepsFallbackWhenIsolationDisabled(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	factoryCalls := 0
	manager.newBuildEnvClient = func(time.Duration) (requestClient, error) {
		factoryCalls++
		return &scriptedRequestClient{}, nil
	}

	lease, configured, err := manager.AcquireBuildEnvironmentDirectIfIsolated(
		WithAccountIdentity(context.Background(), "grok_build_1"), "1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if configured || lease != nil || factoryCalls != 0 {
		t.Fatalf("disabled isolation created a manager direct pool: configured=%t lease=%v factoryCalls=%d", configured, lease, factoryCalls)
	}
}

func TestCreateAndCacheClientRejectsStaleIsolationMode(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	manager.UpdateAccountIsolatedConnections(true)
	factoryCalls := 0
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		factoryCalls++
		return &scriptedRequestClient{}, nil
	}

	// An empty identity represents a cache key derived before isolation was
	// enabled. It must not be accepted after the mode transition, even when it
	// reaches creation after the transition's generation bump.
	_, err := manager.createAndCacheClient(
		clientCacheKey{nodeID: 0, scope: domain.ScopeBuild, fingerprint: "stale-shared"},
		0, domain.ScopeBuild, "", "", false, settingsdomain.DefaultBuildResponseHeaderTimeout, clientOptions{},
	)
	if !errors.Is(err, errClientCacheInvalidated) {
		t.Fatalf("stale isolation mode error = %v", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("stale cache key created %d clients", factoryCalls)
	}
}

func TestAccountIsolationIdentitySurvivesEnableBoundary(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	created := 0
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		created++
		return &scriptedRequestClient{}, nil
	}
	firstIdentity := isolationAccountIdentity(WithAccountIdentity(context.Background(), "account-1"), domain.ScopeBuild, "1")
	secondIdentity := isolationAccountIdentity(WithAccountIdentity(context.Background(), "account-2"), domain.ScopeBuild, "2")
	manager.UpdateAccountIsolatedConnections(true)
	first, err := manager.clientFor(0, domain.ScopeBuild, "", "", "", false, firstIdentity)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.clientFor(0, domain.ScopeBuild, "", "", "", false, secondIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if created != 2 || first.client == second.client {
		t.Fatalf("enable-boundary identities shared a pool: created=%d", created)
	}
}

func TestAccountIsolationHotUpdateEvictsOldPools(t *testing.T) {
	manager := NewManager(egressRepositoryTestStub{}, nil)
	manager.UpdateAccountIsolatedConnections(true)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{}, nil
	}
	first, err := manager.clientFor(0, domain.ScopeBuild, "", "", "", false, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.clientFor(0, domain.ScopeBuild, "", "", "", false, "account-2")
	if err != nil {
		t.Fatal(err)
	}
	manager.UpdateAccountIsolatedConnections(false)
	if first.client.(*scriptedRequestClient).closedIdle != 1 || second.client.(*scriptedRequestClient).closedIdle != 1 {
		t.Fatal("hot update did not close both isolated idle pools")
	}
	manager.clientMu.RLock()
	remaining := len(manager.clients)
	manager.clientMu.RUnlock()
	if remaining != 0 {
		t.Fatalf("stale isolated pools remain after hot update: %d", remaining)
	}

	sharedFirst, err := manager.clientFor(0, domain.ScopeBuild, "", "", "", false, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	sharedSecond, err := manager.clientFor(0, domain.ScopeBuild, "", "", "", false, "account-2")
	if err != nil {
		t.Fatal(err)
	}
	if sharedFirst.client != sharedSecond.client {
		t.Fatal("disabling isolation did not restore the shared pool")
	}
}
