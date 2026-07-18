package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type Config struct {
	BaseURL        string
	SessionBaseURL string
	TimeoutSeconds int
}

type Adapter struct {
	mu     sync.RWMutex
	cfg    Config
	egress *infraegress.Manager
	cipher *security.Cipher
}

func NewAdapter(cfg Config, egress *infraegress.Manager, cipher *security.Cipher) *Adapter {
	return &Adapter{cfg: cfg, egress: egress, cipher: cipher}
}

func (a *Adapter) Provider() account.Provider { return account.ProviderConsole }

func (a *Adapter) UpdateConfig(cfg Config) {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
}

func (a *Adapter) config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

func (a *Adapter) ModelAliases() []provider.ModelAlias { return Aliases() }

func (a *Adapter) QuotaMode(upstreamModel string) string {
	if _, ok := Resolve(upstreamModel); ok {
		return QuotaMode
	}
	return ""
}

func (a *Adapter) TierOrder(string) []account.WebTier { return nil }

func (a *Adapter) PricingModel(upstreamModel string) string { return upstreamModel }

func (a *Adapter) ListModels(context.Context, account.Credential) ([]string, error) {
	values := make([]string, 0, len(catalog))
	for _, spec := range catalog {
		values = append(values, spec.UpstreamModel)
	}
	return values, nil
}

func (a *Adapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	return parseImportedCredentials(data)
}

func (a *Adapter) MarshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	return marshalCredentials(values)
}

func (a *Adapter) SyncQuota(_ context.Context, credential account.Credential) (provider.QuotaSnapshot, error) {
	now := time.Now().UTC()
	resetAt := now.Add(DefaultQuotaWindow * time.Second)
	return provider.QuotaSnapshot{SyncedAt: now, Windows: []account.QuotaWindow{{
		AccountID: credential.ID, Mode: QuotaMode, Remaining: DefaultQuotaLimit, Total: DefaultQuotaLimit,
		WindowSeconds: DefaultQuotaWindow, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceDefault, UpdatedAt: now,
	}}}, nil
}

func (a *Adapter) SyncQuotaMode(ctx context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	if mode != QuotaMode {
		return account.QuotaWindow{}, fmt.Errorf("不支持的 Console 额度模式 %q", mode)
	}
	snapshot, err := a.SyncQuota(ctx, credential)
	if err != nil {
		return account.QuotaWindow{}, err
	}
	return snapshot.Windows[0], nil
}

func (a *Adapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	if request.Method != http.MethodPost || request.Path != "/responses" {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "Grok Console 仅支持 POST /responses"}}), nil
	}
	spec, ok := Resolve(request.Model)
	if !ok {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "Console 模型不存在"}}), nil
	}
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	body := request.Body
	var conversationOptions conversation.ResponseOptions
	if request.NormalizeBody {
		if request.Operation == conversation.OperationMessages {
			body, conversationOptions, err = conversation.ConvertRequestWithOptions(body, request.Model, request.Operation)
		} else {
			body, err = conversation.ConvertRequest(body, request.Model, request.Operation)
		}
		if err == nil {
			body, err = normalizeRequest(body, spec)
		}
		if err != nil {
			return invalidConversationResponse(request.Operation, err), nil
		}
	}
	cfg := a.config()
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	lease, err := a.egress.AcquireCredential(requestCtx, egressdomain.ScopeConsole, request.Credential)
	if err != nil {
		cancel()
		return nil, err
	}
	upstream, err := http.NewRequestWithContext(requestCtx, http.MethodPost, consoleEndpoint(cfg.BaseURL), bytes.NewReader(body))
	if err != nil {
		lease.Release()
		cancel()
		return nil, err
	}
	applyHeaders(upstream, token, lease)
	if request.Streaming {
		upstream.Header.Set("Accept", "text/event-stream")
	}
	response, err := lease.Do(upstream)
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, 0, err)
		lease.Release()
		cancel()
		return nil, err
	}
	responseBodyTruncated := false
	var rateLimit *provider.RateLimitMetadata
	if response.StatusCode == http.StatusTooManyRequests {
		responseBodyTruncated, rateLimit, err = normalizeRateLimitResponse(response)
		if err != nil {
			_ = response.Body.Close()
			lease.Release()
			cancel()
			return nil, err
		}
	}
	release := func() {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, response.StatusCode, nil)
		lease.Release()
		cancel()
	}
	if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
		if request.Streaming && response.StatusCode >= 200 && response.StatusCode < 300 {
			response.Body = conversation.ConvertResponseStreamWithOptions(response.Body, request.Operation, conversationOptions)
			response.Header.Del("Content-Length")
			response.Header.Set("Content-Type", "text/event-stream")
			result := responseResult(response, &releaseBody{ReadCloser: response.Body, release: release})
			result.RateLimit = rateLimit
			return result, nil
		}
		var data []byte
		var readErr error
		var diagnosticTruncated bool
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			data, readErr = io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
		} else {
			data, diagnosticTruncated, readErr = provider.ReadDiagnosticBody(response.Body)
			diagnosticTruncated = diagnosticTruncated || responseBodyTruncated
		}
		_ = response.Body.Close()
		release()
		if readErr != nil {
			return nil, readErr
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 && len(data) > 64<<20 {
			return nil, fmt.Errorf("Console 对话响应超过 64 MiB")
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			diagnostic := &provider.DiagnosticResponse{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header.Clone(), Body: data, BodyTruncated: diagnosticTruncated}
			converted := normalizeConversationError(data, request.Operation, response.StatusCode)
			response.Header.Set("Content-Length", strconv.Itoa(len(converted)))
			response.Header.Set("Content-Type", "application/json")
			result := responseResult(response, io.NopCloser(bytes.NewReader(converted)))
			result.Diagnostic = diagnostic
			result.RateLimit = rateLimit
			return result, nil
		}
		converted, convertErr := conversation.ConvertResponseJSONWithOptions(data, request.Operation, conversationOptions)
		if convertErr != nil {
			return nil, convertErr
		}
		response.Header.Set("Content-Length", strconv.Itoa(len(converted)))
		response.Header.Set("Content-Type", "application/json")
		result := responseResult(response, io.NopCloser(bytes.NewReader(converted)))
		result.RateLimit = rateLimit
		return result, nil
	}
	result := responseResult(response, &releaseBody{ReadCloser: response.Body, release: release})
	result.RateLimit = rateLimit
	return result, nil
}

func normalizeConversationError(data []byte, operation string, status int) []byte {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(data, &envelope) == nil && len(bytes.TrimSpace(envelope.Error)) > 0 && string(bytes.TrimSpace(envelope.Error)) != "null" {
		if converted, err := conversation.ConvertResponseJSON(data, operation); err == nil {
			return converted
		}
	}
	message := strings.TrimSpace(envelope.Message)
	if message == "" {
		message = strings.TrimSpace(string(data))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if len(message) > 4096 {
		message = message[:4096]
	}
	errorType := conversationErrorType(status, operation)
	if operation == conversation.OperationMessages {
		result, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": errorType, "message": message}})
		return result
	}
	result, _ := json.Marshal(map[string]any{"error": map[string]any{"type": errorType, "message": message}})
	return result
}

func conversationErrorType(status int, operation string) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		if operation == conversation.OperationMessages {
			return "overloaded_error"
		}
	}
	if operation == conversation.OperationMessages {
		return "api_error"
	}
	return "server_error"
}

func consoleEndpoint(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/responses"
	}
	return baseURL + "/v1/responses"
}

func normalizeRateLimitResponse(response *http.Response) (bool, *provider.RateLimitMetadata, error) {
	data, truncated, err := provider.ReadDiagnosticBody(response.Body)
	if err != nil {
		return truncated, nil, err
	}
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(data))
	response.ContentLength = int64(len(data))
	response.Header.Set("Content-Length", strconv.Itoa(len(data)))
	metadata := parseConsoleRateLimitMetadata(data)
	if headerValue := response.Header.Get("Retry-After"); headerValue != "" {
		if metadata != nil {
			if retryAfter := parseConsoleRetryAfterHeader(headerValue, time.Now().UTC()); retryAfter > 0 {
				metadata.RetryAfter = retryAfter
			}
		}
	} else {
		retryAfter := consoleRetryAfter(data)
		if metadata != nil {
			retryAfter = metadata.RetryAfter
		}
		if retryAfter > 0 {
			response.Header.Set("Retry-After", strconv.FormatInt(int64(retryAfter/time.Second), 10))
		}
	}
	return truncated, metadata, nil
}

func responseResult(response *http.Response, body io.ReadCloser) *provider.Response {
	upstreamURL := ""
	if response.Request != nil && response.Request.URL != nil {
		upstreamURL = response.Request.URL.String()
	}
	return &provider.Response{
		StatusCode: response.StatusCode, Status: response.Status, Header: response.Header.Clone(), Body: body, QuotaUnits: 1, UpstreamURL: upstreamURL,
	}
}

func jsonProviderResponse(status int, value any) *provider.Response {
	data, _ := json.Marshal(value)
	return &provider.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Length": []string{strconv.Itoa(len(data))}},
		Body:   io.NopCloser(bytes.NewReader(data)),
	}
}

func invalidConversationResponse(operation string, err error) *provider.Response {
	if operation == conversation.OperationMessages {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}})
	}
	return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": err.Error()}})
}

type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
