package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"

	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// DialVoiceWebSocket opens an authenticated Console websocket for realtime or STT streaming.
func (a *Adapter) DialVoiceWebSocket(ctx context.Context, request provider.VoiceWebSocketRequest) (provider.VoiceWebSocketConn, func(), error) {
	pathValue := strings.TrimSpace(request.Path)
	if pathValue == "" || strings.Contains(pathValue, "://") || strings.Contains(pathValue, "..") {
		return nil, nil, invalidConsoleVoiceError("voice websocket path 无效")
	}
	if !strings.HasPrefix(pathValue, "/") {
		pathValue = "/" + pathValue
	}
	switch pathValue {
	case "/realtime", "/stt":
	default:
		return nil, nil, invalidConsoleVoiceError("不支持的 voice websocket path")
	}

	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, nil, err
	}
	requestCtx, cancel := context.WithCancel(ctx)
	lease, err := a.egress.AcquireCredential(requestCtx, egressdomain.ScopeConsole, request.Credential)
	if err != nil {
		cancel()
		return nil, nil, err
	}

	cleanup := func() {
		lease.Release()
		cancel()
	}

	endpoint, err := a.voiceWebSocketEndpoint(pathValue, request.Model)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	proofEndpoint, err := voiceWebSocketProofEndpoint(endpoint)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		session, cacheKey, sessionErr := a.dpop.get(requestCtx, a, request.Credential, token, lease)
		if sessionErr != nil {
			cleanup()
			return nil, nil, sessionErr
		}

		// The websocket dialer needs wss://, while DPoP protects the underlying
		// HTTP Upgrade request and Console validates htu against https://. Signing
		// the websocket URI itself produces a valid JWT with the wrong htu and the
		// server rejects an otherwise healthy session with 401.
		httpReq, requestErr := http.NewRequestWithContext(requestCtx, http.MethodGet, proofEndpoint, nil)
		if requestErr != nil {
			cleanup()
			return nil, nil, requestErr
		}
		applyBrowserHeaders(httpReq, token, lease)
		httpReq.Header.Set("Accept", "*/*")
		httpReq.Header.Set("Sec-Fetch-Mode", "websocket")
		httpReq.Header.Set("Sec-Fetch-Dest", "empty")
		httpReq.Header.Set("Sec-Fetch-Site", "same-origin")
		httpReq.Header.Set("Cache-Control", "no-cache")
		httpReq.Header.Set("Pragma", "no-cache")
		if requestErr := applyDPoPAuthorization(httpReq, session); requestErr != nil {
			cleanup()
			return nil, nil, requestErr
		}

		headers := fhttp.Header{}
		for key, values := range httpReq.Header {
			for _, value := range values {
				headers.Add(key, value)
			}
		}

		connection, response, dialErr := lease.DialWebSocket(requestCtx, endpoint, headers, 30*time.Second)
		if dialErr == nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if connection == nil {
				cleanup()
				return nil, nil, errors.New("Console voice websocket 连接为空")
			}
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, http.StatusSwitchingProtocols, nil)
			return connection, cleanup, nil
		}
		if response == nil {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, 0, dialErr)
			cleanup()
			return nil, nil, fmt.Errorf("拨号 Console voice websocket 失败: %w", dialErr)
		}
		status := response.StatusCode
		var body []byte
		if response.Body != nil {
			body, _, _ = provider.ReadDiagnosticBody(response.Body)
			_ = response.Body.Close()
		}
		if status == http.StatusUnauthorized {
			a.dpop.invalidate(cacheKey, session.accessToken)
			if attempt == 0 {
				continue
			}
		}
		dpopRequired := status == http.StatusForbidden && provider.IsDPoPProofRequiredBody(body)
		if status == http.StatusForbidden && shouldInvalidateConsoleClearance(body) {
			lease.InvalidateClearance()
		}
		if !dpopRequired {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, status, nil)
		}
		cleanup()
		retryAfter := parseConsoleRetryAfterHeader(response.Header.Get("Retry-After"), time.Now().UTC())
		return nil, nil, newConsoleMediaUpstreamError(status, body, retryAfter)
	}
	cleanup()
	return nil, nil, errors.New("Console voice websocket DPoP 重试状态无效")
}

func (a *Adapter) voiceWebSocketEndpoint(pathValue, modelName string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(a.config().BaseURL), "/")
	if base == "" {
		return "", errors.New("Console BaseURL 未配置")
	}
	endpoint, err := url.Parse(consoleV1Endpoint(base, pathValue))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(endpoint.Scheme) {
	case "https":
		endpoint.Scheme = "wss"
	case "http":
		endpoint.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("不支持的 Console voice websocket scheme: %s", endpoint.Scheme)
	}
	values := endpoint.Query()
	if modelName = strings.TrimSpace(modelName); modelName != "" {
		values.Set("model", modelName)
	}
	endpoint.RawQuery = values.Encode()
	return endpoint.String(), nil
}

func voiceWebSocketProofEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Host == "" {
		return "", errors.New("Console voice websocket endpoint 无效")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	case "https", "http":
	default:
		return "", fmt.Errorf("不支持的 Console voice websocket scheme: %s", parsed.Scheme)
	}
	return parsed.String(), nil
}

// Ensure bogdanfinn websocket.Conn satisfies the provider contract at compile time.
var _ provider.VoiceWebSocketConn = (*websocket.Conn)(nil)
