package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

const consoleCluster = "https://us-east-1.api.x.ai"

func buildConsolePayload(body []byte, spec ModelSpec, requestedStreaming bool) ([]byte, bool, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, fmt.Errorf("解析 Console 请求: %w", err)
	}
	payload["model"] = spec.ProtocolModel
	payload["max_output_tokens"] = 2_000_000
	payload["store"] = false
	payload["include"] = []string{"reasoning.encrypted_content"}
	streaming := requestedStreaming
	if value, ok := payload["stream"].(bool); ok {
		streaming = streaming || value
	}
	payload["stream"] = streaming
	effort := spec.ReasoningEffort
	if effort == "" {
		if reasoning, ok := payload["reasoning"].(map[string]any); ok {
			if value, ok := reasoning["effort"].(string); ok {
				switch value {
				case "minimal":
					effort = "low"
				case "none", "low", "medium", "high", "xhigh":
					effort = value
				}
			}
		}
	}
	if effort == "" {
		effort = "medium"
	}
	payload["reasoning"] = map[string]any{"effort": effort}
	payload["tools"] = []map[string]any{
		{"type": "web_search", "enable_image_understanding": true},
		{"type": "x_search", "enable_video_understanding": true},
	}
	payload["tool_choice"] = "auto"
	data, err := json.Marshal(payload)
	return data, streaming, err
}

func buildConsoleHeaders(token string, lease *infraegress.Lease) http.Header {
	headers := buildHeaders(token, lease, "application/json")
	headers.Set("Authorization", "Bearer "+token)
	cookies := []string{"sso=" + token, "sso-rw=" + token}
	if value := strings.TrimSpace(lease.CFCookies); value != "" {
		cookies = append(cookies, value)
	}
	headers.Set("Cookie", strings.Join(cookies, "; "))
	headers.Set("Origin", "https://console.x.ai")
	headers.Set("Referer", "https://console.x.ai/")
	headers.Set("x-cluster", consoleCluster)
	return headers
}

func (a *Adapter) forwardConsoleResponse(ctx context.Context, request provider.ResponseResourceRequest, spec ModelSpec) (*provider.Response, error) {
	body := request.Body
	if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
		converted, err := conversation.ConvertRequest(body, request.Model, request.Operation)
		if err != nil {
			return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"}}), nil
		}
		body = converted
	}
	payload, streaming, err := buildConsolePayload(body, spec, request.Streaming)
	if err != nil {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"}}), nil
	}
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	lease, err := a.egress.Acquire(ctx, domainegress.ScopeWeb, fmt.Sprintf("%d", request.Credential.ID))
	if err != nil {
		return nil, err
	}
	cfg := a.config()
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.ChatTimeoutSeconds)*time.Second)
	upstreamRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, strings.TrimRight(cfg.ConsoleBaseURL, "/")+"/v1/responses", bytes.NewReader(payload))
	if err != nil {
		cancel()
		lease.Release()
		return nil, err
	}
	upstreamRequest.Header = buildConsoleHeaders(token, lease)
	if streaming {
		upstreamRequest.Header.Set("Accept", "text/event-stream")
	} else {
		upstreamRequest.Header.Set("Accept", "application/json")
	}
	upstream, err := lease.Do(upstreamRequest)
	if err != nil {
		cancel()
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		lease.Release()
		return nil, err
	}
	upstream.Body = &cancelBody{ReadCloser: upstream.Body, cancel: cancel}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		return &provider.Response{StatusCode: upstream.StatusCode, Status: upstream.Status, Header: upstream.Header.Clone(), Body: &releaseBody{ReadCloser: upstream.Body, release: func() {
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, upstream.StatusCode, nil)
			lease.Release()
		}}}, nil
	}

	if streaming {
		body := upstream.Body
		if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
			body = conversation.ConvertResponseStream(body, request.Operation)
		}
		return &provider.Response{StatusCode: upstream.StatusCode, Status: upstream.Status, Header: streamHeaders(), Body: &releaseBody{ReadCloser: body, release: func() {
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, upstream.StatusCode, nil)
			lease.Release()
		}}}, nil
	}

	data, readErr := io.ReadAll(io.LimitReader(upstream.Body, (64<<20)+1))
	_ = upstream.Body.Close()
	if readErr != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, readErr)
		lease.Release()
		return nil, readErr
	}
	if len(data) > 64<<20 {
		lease.Release()
		return nil, fmt.Errorf("Console 上游响应超过 64 MiB")
	}
	if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
		data, err = conversation.ConvertResponseJSON(data, request.Operation)
		if err != nil {
			lease.Release()
			return nil, err
		}
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, upstream.StatusCode, nil)
	lease.Release()
	headers := jsonHeaders()
	headers.Set("Content-Length", strconv.Itoa(len(data)))
	return &provider.Response{StatusCode: upstream.StatusCode, Status: upstream.Status, Header: headers, Body: io.NopCloser(bytes.NewReader(data))}, nil
}
