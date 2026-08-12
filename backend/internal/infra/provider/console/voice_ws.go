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

// DialVoiceWebSocket opens an authenticated Console websocket for realtime/TTS/STT streaming.
func (a *Adapter) DialVoiceWebSocket(ctx context.Context, request provider.VoiceWebSocketRequest) (provider.VoiceWebSocketConn, func(), error) {
	pathValue := strings.TrimSpace(request.Path)
	if pathValue == "" || strings.Contains(pathValue, "://") || strings.Contains(pathValue, "..") {
		return nil, nil, invalidConsoleVoiceError("voice websocket path 无效")
	}
	if !strings.HasPrefix(pathValue, "/") {
		pathValue = "/" + pathValue
	}
	switch pathValue {
	case "/realtime", "/tts", "/stt":
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
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, 0, nil)
		lease.Release()
		cancel()
	}

	endpoint, err := a.voiceWebSocketEndpoint(pathValue, request.Model, request.Query)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	session, cacheKey, err := a.dpop.get(requestCtx, a, request.Credential, token, lease)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	applyBrowserHeaders(httpReq, token, lease)
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Sec-Fetch-Mode", "websocket")
	httpReq.Header.Set("Sec-Fetch-Dest", "empty")
	httpReq.Header.Set("Sec-Fetch-Site", "same-origin")
	httpReq.Header.Set("Cache-Control", "no-cache")
	httpReq.Header.Set("Pragma", "no-cache")
	if err := applyDPoPAuthorization(httpReq, session); err != nil {
		cleanup()
		return nil, nil, err
	}

	headers := fhttp.Header{}
	for key, values := range httpReq.Header {
		for _, value := range values {
			headers.Add(key, value)
		}
	}

	connection, response, err := lease.DialWebSocket(requestCtx, endpoint, headers, 30*time.Second)
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			a.dpop.invalidate(cacheKey, session.accessToken)
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, 0, err)
		lease.Release()
		cancel()
		return nil, nil, fmt.Errorf("拨号 Console voice websocket 失败: %w", err)
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if connection == nil {
		cleanup()
		return nil, nil, errors.New("Console voice websocket 连接为空")
	}
	return connection, cleanup, nil
}

func (a *Adapter) voiceWebSocketEndpoint(pathValue, modelName, query string) (string, error) {
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
	if extra := strings.TrimSpace(query); extra != "" {
		extraValues, err := url.ParseQuery(strings.TrimPrefix(extra, "?"))
		if err != nil {
			return "", invalidConsoleVoiceError("voice websocket query 无效")
		}
		for key, items := range extraValues {
			for _, item := range items {
				values.Add(key, item)
			}
		}
	}
	if modelName = strings.TrimSpace(modelName); modelName != "" && values.Get("model") == "" {
		values.Set("model", modelName)
	}
	endpoint.RawQuery = values.Encode()
	return endpoint.String(), nil
}

// Ensure bogdanfinn websocket.Conn satisfies the provider contract at compile time.
var _ provider.VoiceWebSocketConn = (*websocket.Conn)(nil)
