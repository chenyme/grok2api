package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type Config struct {
	BaseURL          string
	ClientVersion    string
	ClientIdentifier string
	TokenAuth        string
	UserAgent        string
}

// Adapter 实现 Grok Build CLI Responses、模型、Billing 与 OAuth 协议。
type Adapter struct {
	cfgMu      sync.RWMutex
	cfg        Config
	http       *http.Client
	oauth      *oauthClient
	cipher     *security.Cipher
	base       http.RoundTripper
	identityMu sync.Mutex
	identities map[uint64]clientIdentity
}

type clientIdentity struct {
	agentID   string
	sessionID string
}

func NewAdapter(cfg Config, cipher *security.Cipher) *Adapter {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, MaxIdleConns: 256, MaxIdleConnsPerHost: 128, MaxConnsPerHost: 256, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	httpClient := &http.Client{Transport: transport}
	return &Adapter{cfg: cfg, http: httpClient, oauth: newOAuthClient(httpClient), cipher: cipher, base: transport, identities: make(map[uint64]clientIdentity)}
}

func (a *Adapter) SetEgress(manager *infraegress.Manager) {
	if manager != nil {
		a.http.Transport = &egressTransport{manager: manager, fallback: a.base}
	}
}

func (a *Adapter) Provider() account.Provider { return account.ProviderBuild }

func (a *Adapter) UpdateConfig(cfg Config) {
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
}

func (a *Adapter) config() Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

func (a *Adapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	accessToken, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	body := request.Body
	var toolCompatibility *responsesToolCompatibility
	var conversationOptions conversation.ResponseOptions
	if request.NormalizeBody {
		if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
			body, conversationOptions, err = conversation.ConvertRequestWithOptions(body, request.Model, request.Operation)
		} else {
			body, toolCompatibility, err = normalizeResponsesRequest(body, request.Model)
		}
		if err != nil {
			if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
				return invalidConversationResponse(request.Operation, err), nil
			}
			return invalidResponsesResponse(err), nil
		}
	}
	if len(body) > 0 && request.Method == http.MethodPost {
		body, err = injectPromptCacheKey(body, request.PromptCacheKey)
		if err != nil {
			err = fmt.Errorf("写入 prompt_cache_key: %w", err)
			if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
				return invalidConversationResponse(request.Operation, err), nil
			}
			return invalidResponsesResponse(err), nil
		}
	}
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, a.url(request.Path), bodyReader)
	if err != nil {
		return nil, err
	}
	if err := a.applyHeaders(req, request.Credential, accessToken, request.Model, request.PromptCacheKey, true); err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if request.Streaming {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Accept-Encoding", "identity")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if request.IdempotencyID != "" {
		req.Header.Set("Idempotency-Key", request.IdempotencyID)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return nil, err
	}
	responsesOperation := request.Operation == "" || request.Operation == conversation.OperationResponses
	if responsesOperation && toolCompatibility != nil {
		if warnings := toolCompatibility.warningHeader(); warnings != "" {
			resp.Header.Set("X-Grok2API-Compatibility-Warnings", warnings)
		}
	}
	if responsesOperation && toolCompatibility != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if request.Streaming {
			resp.Body = toolCompatibility.normalizeResponseStream(resp.Body)
			resp.Header.Del("Content-Length")
			resp.Header.Set("Content-Type", "text/event-stream")
		} else {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCompatibleResponseBytes+1))
			_ = resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			if len(data) > maxCompatibleResponseBytes {
				return nil, fmt.Errorf("上游兼容 Responses 响应超过 128 MiB")
			}
			converted, convertErr := toolCompatibility.normalizeResponseJSON(data)
			if convertErr != nil {
				return nil, convertErr
			}
			resp.Body = io.NopCloser(bytes.NewReader(converted))
			resp.Header.Set("Content-Length", strconv.Itoa(len(converted)))
			resp.Header.Set("Content-Type", "application/json")
		}
	}
	if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
		if request.Streaming && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body = conversation.ConvertResponseStreamWithOptions(resp.Body, request.Operation, conversationOptions)
			resp.Header.Del("Content-Length")
			resp.Header.Set("Content-Type", "text/event-stream")
		} else {
			var data []byte
			var readErr error
			var diagnosticTruncated bool
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				data, readErr = io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
			} else {
				data, diagnosticTruncated, readErr = provider.ReadDiagnosticBody(resp.Body)
			}
			_ = resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && len(data) > 64<<20 {
				return nil, fmt.Errorf("上游对话响应超过 64 MiB")
			}
			var diagnostic *provider.DiagnosticResponse
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				diagnostic = &provider.DiagnosticResponse{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: data, BodyTruncated: diagnosticTruncated}
			}
			converted, convertErr := conversation.ConvertResponseJSONWithOptions(data, request.Operation, conversationOptions)
			if convertErr != nil {
				if diagnostic == nil {
					return nil, convertErr
				}
				return &provider.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: diagnostic.Header.Clone(), Body: io.NopCloser(bytes.NewReader(data)), UpstreamURL: req.URL.String(), Diagnostic: diagnostic}, nil
			}
			resp.Body = io.NopCloser(bytes.NewReader(converted))
			resp.Header.Set("Content-Length", strconv.Itoa(len(converted)))
			resp.Header.Set("Content-Type", "application/json")
			return &provider.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: resp.Body, UpstreamURL: req.URL.String(), Diagnostic: diagnostic}, nil
		}
	}
	return &provider.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: resp.Body, UpstreamURL: req.URL.String()}, nil
}

// invalidResponsesResponse 将本地协议校验错误转换为标准 OpenAI 错误响应，避免触发上游账号重试。
func invalidResponsesResponse(err error) *provider.Response {
	code := "invalid_request"
	param := ""
	message := err.Error()
	var requestErr *responsesRequestError
	if errors.As(err, &requestErr) {
		code = requestErr.Code
		param = requestErr.Param
		message = requestErr.Message
	}
	errorBody := map[string]any{"type": "invalid_request_error", "message": message, "code": code}
	if param != "" {
		errorBody["param"] = param
	}
	data, _ := json.Marshal(map[string]any{"error": errorBody})
	return &provider.Response{
		StatusCode: http.StatusBadRequest, Status: "400 Bad Request",
		Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Length": []string{strconv.Itoa(len(data))}},
		Body:   io.NopCloser(bytes.NewReader(data)),
	}
}

func invalidConversationResponse(operation string, err error) *provider.Response {
	var payload any = map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": err.Error()}}
	if operation == conversation.OperationMessages {
		payload = map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}}
	}
	data, _ := json.Marshal(payload)
	return &provider.Response{
		StatusCode: http.StatusBadRequest, Status: "400 Bad Request",
		Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Length": []string{strconv.Itoa(len(data))}},
		Body:   io.NopCloser(bytes.NewReader(data)),
	}
}

func (a *Adapter) ListModels(ctx context.Context, credential account.Credential) ([]string, error) {
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url("/models"), nil)
	if err != nil {
		return nil, err
	}
	if err := a.applyHeaders(req, credential, accessToken, "", "", false); err != nil {
		return nil, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游模型接口返回 %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID != "" {
			models = append(models, item.ID)
		}
	}
	return models, nil
}

func (a *Adapter) GetBilling(ctx context.Context, credential account.Credential) (account.Billing, error) {
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return account.Billing{}, err
	}
	monthly, err := a.getBilling(ctx, credential, accessToken, "")
	if err != nil {
		return account.Billing{}, err
	}
	monthly.AccountID = credential.ID
	if credits, creditsErr := a.getBilling(ctx, credential, accessToken, "format=credits"); creditsErr == nil {
		monthly = mergeBillingSnapshots(monthly, credits)
	}
	monthly.SyncedAt = time.Now().UTC()
	return monthly, nil
}

// mergeBillingSnapshots 合并套餐 credits 与 /usage 使用的当前限额周期，周周期优先作为恢复时间。
func mergeBillingSnapshots(monthly, credits account.Billing) account.Billing {
	if monthly.PlanCode == "" {
		monthly.PlanCode = credits.PlanCode
	}
	if monthly.PlanName == "" {
		monthly.PlanName = credits.PlanName
	}
	monthly.OnDemandCap = credits.OnDemandCap
	monthly.OnDemandUsed = credits.OnDemandUsed
	monthly.PrepaidBalance = credits.PrepaidBalance
	monthly.CreditUsagePercent = credits.CreditUsagePercent
	monthly.IsUnifiedBillingUser = credits.IsUnifiedBillingUser
	monthly.TopUpMethod = credits.TopUpMethod
	monthly.UsagePeriodType = credits.UsagePeriodType
	monthly.UsagePeriodStart = credits.UsagePeriodStart
	monthly.UsagePeriodEnd = credits.UsagePeriodEnd
	return monthly
}

func (a *Adapter) RefreshCredential(ctx context.Context, credential account.Credential) (provider.RefreshedCredential, error) {
	refreshToken, err := a.cipher.Decrypt(credential.EncryptedRefreshToken)
	if err != nil {
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "credential_decrypt_failed", Permanent: true, Cause: err}
	}
	if strings.TrimSpace(refreshToken) == "" {
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "missing_refresh_token", Permanent: true}
	}
	tokens, err := a.oauth.refresh(ctx, refreshToken)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	accessEncrypted, err := a.cipher.Encrypt(tokens.AccessToken)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	refreshEncrypted, err := a.cipher.Encrypt(tokens.RefreshToken)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	return provider.RefreshedCredential{EncryptedAccessToken: accessEncrypted, EncryptedRefreshToken: refreshEncrypted, ExpiresAt: tokens.ExpiresAt}, nil
}

func (a *Adapter) StartDeviceAuthorization(ctx context.Context) (provider.DeviceAuthorization, error) {
	return a.oauth.startDevice(ctx)
}

func (a *Adapter) PollDeviceAuthorization(ctx context.Context, deviceCode string) (provider.CredentialSeed, error) {
	tokens, err := a.oauth.pollDevice(ctx, deviceCode)
	if err != nil {
		return provider.CredentialSeed{}, err
	}
	claims := decodeJWTClaims(firstNonEmpty(tokens.IDToken, tokens.AccessToken))
	userID := stringClaim(claims, "sub")
	email := stringClaim(claims, "email")
	return provider.CredentialSeed{Name: firstNonEmpty(email, userID, "Grok Build account"), Email: email, UserID: userID, TeamID: stringClaim(claims, "team_id"), OIDCClientID: defaultOAuthClientID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, ExpiresAt: tokens.ExpiresAt}, nil
}

func (a *Adapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	return parseImportedCredentials(data)
}

func (a *Adapter) MarshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	return marshalCredentials(values)
}

func (a *Adapter) applyHeaders(req *http.Request, credential account.Credential, accessToken, model, promptCacheKey string, trace bool) error {
	cfg := a.config()
	identity, err := a.clientIdentity(credential.ID)
	if err != nil {
		return err
	}
	requestID, err := randomHex(16)
	if err != nil {
		return err
	}
	conversationID := strings.TrimSpace(promptCacheKey)
	if conversationID == "" {
		conversationID, err = randomHex(16)
		if err != nil {
			return err
		}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-XAI-Token-Auth", cfg.TokenAuth)
	req.Header.Set("x-grok-client-version", cfg.ClientVersion)
	req.Header.Set("x-grok-client-identifier", cfg.ClientIdentifier)
	req.Header.Set("x-grok-client-surface", "tui")
	req.Header.Set("x-grok-client-name", cfg.ClientIdentifier)
	req.Header.Set("x-grok-agent-id", identity.agentID)
	req.Header.Set("x-grok-session-id", identity.sessionID)
	req.Header.Set("x-grok-conv-id", conversationID)
	req.Header.Set("x-grok-req-id", requestID)
	req.Header.Set("x-grok-conversation-id", conversationID)
	req.Header.Set("x-grok-session-id-legacy", identity.sessionID)
	req.Header.Set("x-grok-request-id", requestID)
	if credential.UserID != "" {
		req.Header.Set("x-userid", credential.UserID)
	}
	if trace {
		traceID, traceErr := randomHex(16)
		if traceErr != nil {
			return traceErr
		}
		spanID, spanErr := randomHex(8)
		if spanErr != nil {
			return spanErr
		}
		req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
		req.Header.Set("tracestate", "")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", cfg.UserAgent)
	if model != "" {
		req.Header.Set("x-grok-model-override", model)
	}
	return nil
}

func (a *Adapter) clientIdentity(accountID uint64) (clientIdentity, error) {
	a.identityMu.Lock()
	defer a.identityMu.Unlock()
	if value, ok := a.identities[accountID]; ok {
		return value, nil
	}
	agentID, err := randomHex(16)
	if err != nil {
		return clientIdentity{}, err
	}
	sessionID, err := randomUUID()
	if err != nil {
		return clientIdentity{}, err
	}
	value := clientIdentity{agentID: agentID, sessionID: sessionID}
	a.identities[accountID] = value
	return value, nil
}

func injectPromptCacheKey(body []byte, clientKey string) ([]byte, error) {
	key := strings.TrimSpace(clientKey)
	if key == "" {
		return body, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		payload = make(map[string]json.RawMessage)
	}
	payload["prompt_cache_key"] = mustJSON(key)
	return json.Marshal(payload)
}

func randomHex(bytesLength int) (string, error) {
	value := make([]byte, bytesLength)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func randomUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value)
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:], nil
}

func normalizeGzipResponse(response *http.Response) error {
	if response == nil || response.Body == nil || !strings.EqualFold(strings.TrimSpace(response.Header.Get("Content-Encoding")), "gzip") {
		return nil
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		_ = response.Body.Close()
		return err
	}
	response.Body = &gzipResponseBody{Reader: reader, source: response.Body}
	response.Header.Del("Content-Encoding")
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	return nil
}

type gzipResponseBody struct {
	*gzip.Reader
	source io.Closer
}

func (b *gzipResponseBody) Close() error {
	readerErr := b.Reader.Close()
	sourceErr := b.source.Close()
	if readerErr != nil {
		return readerErr
	}
	return sourceErr
}

func (a *Adapter) url(path string) string {
	return strings.TrimRight(a.config().BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (a *Adapter) getBilling(ctx context.Context, credential account.Credential, accessToken, query string) (account.Billing, error) {
	endpoint := a.url("/billing")
	if query != "" {
		endpoint += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return account.Billing{}, err
	}
	if err := a.applyHeaders(req, credential, accessToken, "", "", false); err != nil {
		return account.Billing{}, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return account.Billing{}, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return account.Billing{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return account.Billing{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return account.Billing{}, fmt.Errorf("上游 Billing 接口返回 %d", resp.StatusCode)
	}
	return parseBilling(body)
}
