package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
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

	"github.com/google/uuid"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/reasoningreplay"
)

type Config struct {
	BaseURL          string
	FallbackBaseURL  string
	ClientVersion    string
	ClientIdentifier string
	TokenAuth        string
	UserAgent        string
}

const subscriptionTierTimeout = 10 * time.Second

// Adapter 实现 Grok Build CLI Responses、模型、Billing 与 OAuth 协议。
type Adapter struct {
	cfgMu          sync.RWMutex
	cfg            Config
	http           *http.Client
	oauth          *oauthClient
	cipher         *security.Cipher
	base           http.RoundTripper
	agentID        string
	modelsMu       sync.Mutex
	modelsETags    map[uint64]string
	fallbackMarker FallbackMarker
	uploadIssuer   VideoUploadIssuer
	replay         *reasoningreplay.ReasoningReplay
	compaction     *gatewayCompactionCodec
}

func NewAdapter(cfg Config, cipher *security.Cipher) *Adapter {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, MaxIdleConns: 256, MaxIdleConnsPerHost: 128, MaxConnsPerHost: 256, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	httpClient := &http.Client{Transport: transport}
	// 官方 CLI 使用持久化机器身份。网关不采集机器指纹，改为每个后端
	// 进程生成一个随机 UUID，在进程生命周期内作为统一 Agent 身份。
	agentID := uuid.NewString()
	return &Adapter{
		cfg: cfg, http: httpClient, oauth: newOAuthClient(httpClient), cipher: cipher, base: transport,
		agentID: agentID, modelsETags: make(map[uint64]string), compaction: newGatewayCompactionCodec(cipher),
	}
}

func (a *Adapter) SetEgress(manager *infraegress.Manager) {
	if manager != nil {
		a.http.Transport = &egressTransport{manager: manager, fallback: a.base}
	}
}

// SetReasoningReplay 注入服务端推理回放缓存（可选）。
func (a *Adapter) SetReasoningReplay(replay *reasoningreplay.ReasoningReplay) {
	a.replay = replay
}

func (a *Adapter) Provider() account.Provider { return account.ProviderBuild }

// CredentialMetadata 只从 Build access token 提取非敏感风险标记。
// bot_flag_source 必须是 JSON 数字 1；其他值、畸形 token 或解密失败均不标记。
func (a *Adapter) CredentialMetadata(credential account.Credential) provider.CredentialMetadata {
	if credential.Provider != account.ProviderBuild || a.cipher == nil || credential.EncryptedAccessToken == "" {
		return provider.CredentialMetadata{}
	}
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return provider.CredentialMetadata{}
	}
	value, ok := decodeJWTClaims(accessToken)["bot_flag_source"].(float64)
	return provider.CredentialMetadata{BuildBotFlagged: ok && value == 1}
}

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
	compactionRequested := false
	if request.NormalizeBody {
		if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
			body, conversationOptions, err = conversation.ConvertRequestWithOptions(body, request.Model, request.Operation)
		} else {
			var foreignCompactions int
			body, foreignCompactions, err = expandGatewayCompactionHistory(body, a.compaction, request.PromptCacheKey)
			if err != nil {
				return invalidResponsesResponse(err), nil
			}
			body, toolCompatibility, err = normalizeResponsesRequest(body, request.Model)
			if toolCompatibility != nil {
				compactionRequested = toolCompatibility.compactionRequested
				if foreignCompactions > 0 {
					toolCompatibility.addWarning("foreign_compaction_omitted")
				}
			}
		}
		if err != nil {
			if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
				return invalidConversationResponse(request.Operation, err), nil
			}
			return invalidResponsesResponse(err), nil
		}
	}
	if compactionRequested {
		body, err = prepareGatewayCompactionSample(body)
		if err != nil {
			return invalidResponsesResponse(err), nil
		}
	}
	if len(body) > 0 && request.Method == http.MethodPost {
		if !compactionRequested {
			body, err = injectPromptCacheKey(body, request.PromptCacheKey)
			if err != nil {
				err = fmt.Errorf("写入 prompt_cache_key: %w", err)
				if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
					return invalidConversationResponse(request.Operation, err), nil
				}
				return invalidResponsesResponse(err), nil
			}
		}
	}
	if compactionRequested {
		warnings := ""
		if toolCompatibility != nil {
			warnings = toolCompatibility.warningHeader()
		}
		return a.forwardGatewayCompaction(ctx, request, accessToken, body, warnings)
	}
	// 显式模式优先；auto 下仅已确认 Super 且 bot_flag_source=1 的账号默认走 XAI。
	primaryBase := a.primaryBaseURL()
	base := a.inferenceBaseForOperation(request.Credential, request.Billing, request.Method, request.Path)
	// 缓存亲和与推理回放使用不同身份。回放还必须绑定实际账号和上游平面，
	// 避免把一个账号或 Build 平面签发的 opaque reasoning 发给另一作用域。
	replayBaseBody := body
	body, replayKey := a.applyReasoningReplay(ctx, request, replayBaseBody, base)
	resp, reqURL, err := a.doResponseRequest(ctx, request, accessToken, body, base)
	if err != nil {
		return nil, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return nil, err
	}
	resp, reqURL, reasoningRecovered := a.recoverReasoningDecodeFailure(ctx, request, accessToken, body, base, resp, reqURL)
	// 仅可回退操作在当次 Build 主地址明确 403 时用等价请求探测 XAI。
	if strings.EqualFold(base, primaryBase) && shouldProbeXAIInferenceFallback(request.Credential, request.Billing, request.Method, request.Path, resp.StatusCode) {
		// 缓冲主 403 正文，备用失败时原样回放，避免二次 primary POST。
		primaryBody, primaryTruncated, readErr := provider.ReadDiagnosticBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		primaryResp := cloneBufferedResponse(resp, primaryBody, primaryTruncated)
		fallbackBase := a.fallbackBaseURL()
		if fallbackBase != "" && !strings.EqualFold(fallbackBase, base) {
			fallbackBody, fallbackReplayKey := a.applyReasoningReplay(ctx, request, replayBaseBody, fallbackBase)
			fallbackResp, fallbackURL, fallbackErr := a.doResponseRequest(ctx, request, accessToken, fallbackBody, fallbackBase)
			if fallbackErr == nil {
				fallbackErr = normalizeGzipResponse(fallbackResp)
			}
			fallbackRecovered := false
			if fallbackErr == nil {
				fallbackResp, fallbackURL, fallbackRecovered = a.recoverReasoningDecodeFailure(ctx, request, accessToken, fallbackBody, fallbackBase, fallbackResp, fallbackURL)
			}
			if fallbackErr == nil && isHTTPSuccess(fallbackResp.StatusCode) {
				a.activateBuildAPIFallback(ctx, &request.Credential)
				resp, reqURL, base, body, replayKey = fallbackResp, fallbackURL, fallbackBase, fallbackBody, fallbackReplayKey
				reasoningRecovered = reasoningRecovered || fallbackRecovered
			} else {
				if fallbackErr == nil {
					_ = fallbackResp.Body.Close()
				}
				// 保留原 primary 403 的 URL 与缓冲正文，不再次请求主地址。
				resp = primaryResp
			}
		} else {
			resp = primaryResp
		}
	}
	modelCatalogChanged := a.modelCatalogChanged(request.Credential.ID, resp.Header.Get("x-models-etag"))
	// 在协议转换前捕获上游 Responses 形态，写入/清理推理回放缓存。
	if a.shouldCaptureReplay(request, resp, replayKey) {
		resp.Body = a.replay.CaptureBody(resp.Body, request.Model, replayKey, request.Streaming, isCompactPath(request.Path))
	}
	responsesOperation := request.Operation == "" || request.Operation == conversation.OperationResponses || request.Operation == conversation.OperationCompaction
	if responsesOperation && toolCompatibility != nil {
		if warnings := toolCompatibility.warningHeader(); warnings != "" {
			resp.Header.Set("X-Grok2API-Compatibility-Warnings", warnings)
		}
	}
	if reasoningRecovered {
		appendCompatibilityWarning(resp.Header, "reasoning_encrypted_content_downgraded")
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
				return &provider.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: diagnostic.Header.Clone(), Body: io.NopCloser(bytes.NewReader(data)), UpstreamURL: reqURL, Diagnostic: diagnostic, ModelCatalogChanged: modelCatalogChanged}, nil
			}
			resp.Body = io.NopCloser(bytes.NewReader(converted))
			resp.Header.Set("Content-Length", strconv.Itoa(len(converted)))
			resp.Header.Set("Content-Type", "application/json")
			return &provider.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: resp.Body, UpstreamURL: reqURL, Diagnostic: diagnostic, ModelCatalogChanged: modelCatalogChanged}, nil
		}
	}
	return &provider.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: resp.Body, UpstreamURL: reqURL, ModelCatalogChanged: modelCatalogChanged}, nil
}

func (a *Adapter) shouldCaptureReplay(request provider.ResponseResourceRequest, resp *http.Response, replayKey string) bool {
	if a.replay == nil || !a.replay.Enabled() || resp == nil {
		return false
	}
	if request.Method != http.MethodPost || strings.TrimSpace(replayKey) == "" {
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	return true
}

func (a *Adapter) applyReasoningReplay(ctx context.Context, request provider.ResponseResourceRequest, body []byte, base string) ([]byte, string) {
	if a.replay == nil || !a.replay.Enabled() || request.Method != http.MethodPost {
		return body, ""
	}
	key := a.scopedReasoningReplayKey(request, base)
	if key == "" {
		return body, ""
	}
	if isCompactPath(request.Path) {
		// compact 不注入历史，但成功后仍需用同一作用域清理旧 replay。
		return body, key
	}
	return a.replay.Apply(ctx, request.Model, key, body), key
}

func (a *Adapter) scopedReasoningReplayKey(request provider.ResponseResourceRequest, base string) string {
	seed := strings.TrimSpace(request.ReasoningReplayKey)
	if seed == "" || request.Credential.ID == 0 {
		return ""
	}
	plane := "build"
	if fallback := a.fallbackBaseURL(); fallback != "" && strings.EqualFold(strings.TrimRight(base, "/"), fallback) {
		plane = "xai"
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("grok2api:reasoning-replay:v2:%s:%d:%s", seed, request.Credential.ID, plane)))
	return hex.EncodeToString(digest[:])
}

func isCompactPath(path string) bool {
	return strings.Contains(strings.ToLower(path), "compact")
}

func (a *Adapter) doResponseRequest(ctx context.Context, request provider.ResponseResourceRequest, accessToken string, body []byte, base string) (*http.Response, string, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	requestCtx := infraegress.WithCredential(ctx, request.Credential)
	req, err := http.NewRequestWithContext(requestCtx, request.Method, a.urlWithBase(base, request.Path), bodyReader)
	if err != nil {
		return nil, "", err
	}
	if err := a.applyHeaders(req, request.Credential, accessToken, request.Model, request.PromptCacheKey, true); err != nil {
		return nil, "", err
	}
	applyGrokTurnIndexHeader(req, request.GrokTurnIndex)
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
		return nil, "", err
	}
	return resp, req.URL.String(), nil
}

// applyGrokTurnIndexHeader 只在请求已有稳定 Grok session 时透传真实客户端轮次。
func applyGrokTurnIndexHeader(request *http.Request, value string) {
	if request.Header.Get("x-grok-session-id") == "" {
		return
	}
	if turnIndex := normalizeGrokTurnIndex(value); turnIndex != "" {
		request.Header.Set("x-grok-turn-idx", turnIndex)
	}
}

// normalizeGrokTurnIndex 只接受官方客户端生成的非负十进制 u64。
// 空值或非法值直接省略，避免网关根据历史、工具循环或 compact 结果伪造轮次。
func normalizeGrokTurnIndex(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 20 {
		return ""
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return ""
		}
	}
	if _, err := strconv.ParseUint(value, 10, 64); err != nil {
		return ""
	}
	return value
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
	// 模型目录始终请求 Build 主地址；不得因目录缺 1.5 或 Super entitlement 预切 XAI。
	// 1.5 能力由 NormalizeAccountModelCapabilities 按 Billing paid / BuildSuperEntitled 本地补齐。
	models, status, err := a.listModelsAt(ctx, credential, accessToken, a.primaryBaseURL())
	if err != nil {
		return nil, err
	}
	if models != nil {
		return models, nil
	}
	return nil, fmt.Errorf("上游模型接口返回 %d", status)
}

// NormalizeAccountModelCapabilities 按 Super（Billing paid 或 BuildSuperEntitled）归一化 1.5 视频资格。
// Super 确保包含 grok-imagine-video-1.5；Free/Unknown 精确移除。不读取 BuildAPIFallback。
func (a *Adapter) NormalizeAccountModelCapabilities(models []string, billing *account.Billing, credential account.Credential) []string {
	super := account.IsBuildSuper(credential, billing)
	result := make([]string, 0, len(models)+1)
	seen := make(map[string]struct{}, len(models)+1)
	hasVideo15 := false
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		if model == buildVideoModel {
			if !super {
				continue
			}
			hasVideo15 = true
		}
		seen[model] = struct{}{}
		result = append(result, model)
	}
	if super && !hasVideo15 {
		result = append(result, buildVideoModel)
	}
	return result
}

func (a *Adapter) listModelsAt(ctx context.Context, credential account.Credential, accessToken, base string) ([]string, int, error) {
	requestCtx := infraegress.WithCredential(ctx, credential)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, a.urlWithBase(base, "/models"), nil)
	if err != nil {
		return nil, 0, err
	}
	if err := a.applyHeaders(req, credential, accessToken, "", "", false); err != nil {
		return nil, 0, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, resp.StatusCode, err
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID != "" {
			models = append(models, item.ID)
		}
	}
	a.recordModelsETag(credential.ID, resp.Header.Get("ETag"))
	return models, resp.StatusCode, nil
}

func (a *Adapter) recordModelsETag(accountID uint64, etag string) {
	etag = strings.TrimSpace(etag)
	if etag == "" {
		return
	}
	a.modelsMu.Lock()
	if a.modelsETags == nil {
		a.modelsETags = make(map[uint64]string)
	}
	a.modelsETags[accountID] = etag
	a.modelsMu.Unlock()
}

func (a *Adapter) modelCatalogChanged(accountID uint64, etag string) bool {
	etag = strings.TrimSpace(etag)
	if etag == "" {
		return false
	}
	a.modelsMu.Lock()
	defer a.modelsMu.Unlock()
	if a.modelsETags == nil {
		a.modelsETags = make(map[uint64]string)
	}
	current := a.modelsETags[accountID]
	if current == "" {
		// 进程重启后内存中没有目录基线。让 Gateway 补一次账号级
		// /models 同步；同步成功后 recordModelsETag 会建立基线。
		return true
	}
	return current != etag
}

func (a *Adapter) GetBilling(ctx context.Context, credential account.Credential) (account.Billing, error) {
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return account.Billing{}, err
	}
	billing, err := a.getBilling(ctx, credential, accessToken, "format=credits")
	if err != nil {
		return account.Billing{}, err
	}
	// 周额度在 0% 使用时无法区分 Free 与刚开通的付费套餐。官方 CLI
	// 使用 /user?include=subscription 获取实时订阅等级；失败时再退回 JWT tier。
	if tier, tierErr := a.getSubscriptionTier(ctx, credential, accessToken); tierErr == nil && tier != "" {
		billing.PlanName = tier
	} else if billing.PlanCode == "" && billing.PlanName == "" {
		billing.PlanName = subscriptionTierFromJWT(accessToken)
	}
	billing.AccountID = credential.ID
	billing.SyncedAt = time.Now().UTC()
	return billing, nil
}

func (a *Adapter) RefreshCredential(ctx context.Context, credential account.Credential) (provider.RefreshedCredential, error) {
	refreshToken, err := a.cipher.Decrypt(credential.EncryptedRefreshToken)
	if err != nil {
		// 解密失败通常是本地 encryption key 临时/不匹配，属于可恢复故障；
		// 不得标记为 permanent（否则密钥恢复后手动/批量刷新永远不会重试）。
		// 真正的 OAuth 永久失败（如 invalid_grant）由 oauth.refresh 返回 Permanent=true。
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "credential_decrypt_failed", Permanent: false, Cause: err}
	}
	if strings.TrimSpace(refreshToken) == "" {
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "missing_refresh_token", Permanent: true}
	}
	refreshCtx := infraegress.WithCredential(ctx, credential)
	tokens, err := a.oauth.refresh(refreshCtx, refreshToken)
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
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-XAI-Token-Auth", cfg.TokenAuth)
	req.Header.Set("x-grok-client-version", cfg.ClientVersion)
	req.Header.Set("x-grok-client-identifier", cfg.ClientIdentifier)
	req.Header.Set("x-grok-client-mode", "headless")

	if trace {
		requestID := uuid.NewString()
		// 对齐 CPA：仅在存在稳定 session 时设置 x-grok-conv-id / session-id。
		// 禁止每请求随机 UUID，否则会打散 xAI 服务器亲和，导致 cached_tokens 长期为 0。
		sessionID, err := grokSessionID(promptCacheKey)
		if err != nil {
			return err
		}
		req.Header.Set("x-authenticateresponse", "authenticate-response")
		req.Header.Set("x-grok-agent-id", a.agentID)
		if sessionID != "" {
			req.Header.Set("x-grok-session-id", sessionID)
			req.Header.Set("x-grok-conv-id", sessionID)
		}
		req.Header.Set("x-grok-req-id", requestID)
		// 网关无法从无状态 API 请求可靠恢复 CLI prompt index；该字段在
		// 官方协议中可选，因此不伪造 x-grok-turn-idx。
		if credential.UserID != "" {
			req.Header.Set("x-grok-user-id", credential.UserID)
		}
		traceID, traceErr := randomHex(16)
		if traceErr != nil {
			return traceErr
		}
		spanID, spanErr := randomHex(8)
		if spanErr != nil {
			return spanErr
		}
		req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	} else {
		if credential.UserID != "" {
			req.Header.Set("x-userid", credential.UserID)
		}
		if credential.Email != "" {
			req.Header.Set("x-email", credential.Email)
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", cfg.UserAgent)
	if model != "" {
		req.Header.Set("x-grok-model-override", model)
	}
	return nil
}

// grokSessionID 将稳定会话键转为上游 x-grok-conv-id。
// 空键返回空串（对齐 CPA grok_build_stays_stateless_without_session），绝不每请求随机生成。
func grokSessionID(promptCacheKey string) (string, error) {
	key := strings.TrimSpace(promptCacheKey)
	if key == "" {
		return "", nil
	}
	if parsed, err := uuid.Parse(key); err == nil {
		return parsed.String(), nil
	}
	return uuid.NewHash(sha256.New(), uuid.NameSpaceURL, []byte("grok2api:session:"+key), 8).String(), nil
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
	requestCtx := infraegress.WithCredential(ctx, credential)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
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

func (a *Adapter) getSubscriptionTier(ctx context.Context, credential account.Credential, accessToken string) (string, error) {
	endpoint := a.url("/user") + "?include=subscription"
	requestCtx, cancel := context.WithTimeout(infraegress.WithCredential(ctx, credential), subscriptionTierTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if err := a.applyHeaders(req, credential, accessToken, "", "", false); err != nil {
		return "", err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("上游订阅接口返回 %d", resp.StatusCode)
	}
	return parseSubscriptionTier(body)
}
