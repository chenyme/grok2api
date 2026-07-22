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
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/reasoningreplay"
)

type Config struct {
	BaseURL               string
	FallbackBaseURL       string
	ClientVersion         string
	ClientIdentifier      string
	TokenAuth             string
	UserAgent             string
	ResponseHeaderTimeout time.Duration
}

const (
	subscriptionTierTimeout = 10 * time.Second
	buildControlTimeout     = 30 * time.Second
)

// Adapter implements the Grok Build CLI Responses, model, Billing, and OAuth protocols.
type Adapter struct {
	cfgMu          sync.RWMutex
	cfg            Config
	http           *http.Client
	oauth          *oauthClient
	cipher         *security.Cipher
	base           *buildDirectTransport
	agentID        string
	modelsMu       sync.Mutex
	modelsETags    map[uint64]string
	fallbackMarker FallbackMarker
	uploadIssuer   VideoUploadIssuer
	replay         *reasoningreplay.ReasoningReplay
	compaction     *gatewayCompactionCodec
	logger         *slog.Logger
}

func NewAdapter(cfg Config, cipher *security.Cipher) *Adapter {
	cfg.ResponseHeaderTimeout = normalizeBuildResponseHeaderTimeout(cfg.ResponseHeaderTimeout)
	transport := newBuildDirectTransport(cfg.ResponseHeaderTimeout)
	httpClient := &http.Client{Transport: transport}
	// The official CLI uses a persistent machine identity. The gateway does not collect machine fingerprints;
	// instead each backend process generates one random UUID for its lifetime as the Agent identity.
	agentID := uuid.NewString()
	return &Adapter{
		cfg: cfg, http: httpClient, oauth: newOAuthClient(httpClient), cipher: cipher, base: transport,
		agentID: agentID, modelsETags: make(map[uint64]string), compaction: newGatewayCompactionCodec(cipher), logger: slog.Default(),
	}
}

func (a *Adapter) SetLogger(logger *slog.Logger) {
	if logger != nil {
		a.logger = logger
	}
}

func (a *Adapter) SetEgress(manager *infraegress.Manager) {
	if manager != nil {
		a.http.Transport = &egressTransport{manager: manager, fallback: a.base}
	}
}

// SetReasoningReplay injects the optional server-side reasoning replay cache.
func (a *Adapter) SetReasoningReplay(replay *reasoningreplay.ReasoningReplay) {
	a.replay = replay
}

func (a *Adapter) Provider() account.Provider { return account.ProviderBuild }

// CredentialMetadata extracts only non-sensitive risk flags from a Build access token.
// bot_flag_source must be JSON number 1; other values, malformed tokens, and decryption failures are not marked.
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
	cfg.ResponseHeaderTimeout = normalizeBuildResponseHeaderTimeout(cfg.ResponseHeaderTimeout)
	a.cfgMu.Lock()
	previousTimeout := a.cfg.ResponseHeaderTimeout
	a.cfg = cfg
	a.cfgMu.Unlock()
	if previousTimeout != cfg.ResponseHeaderTimeout && a.base != nil {
		a.base.UpdateResponseHeaderTimeout(cfg.ResponseHeaderTimeout)
	}
}

type buildDirectTransport struct {
	current atomic.Pointer[http.Transport]
}

func newBuildDirectTransport(responseHeaderTimeout time.Duration) *buildDirectTransport {
	value := &buildDirectTransport{}
	value.current.Store(newBuildHTTPTransport(responseHeaderTimeout))
	return value
}

func (t *buildDirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t.current.Load().RoundTrip(request)
}

func (t *buildDirectTransport) UpdateResponseHeaderTimeout(responseHeaderTimeout time.Duration) {
	next := newBuildHTTPTransport(responseHeaderTimeout)
	previous := t.current.Swap(next)
	if previous != nil {
		previous.CloseIdleConnections()
	}
}

func newBuildHTTPTransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
		MaxIdleConns: 256, MaxIdleConnsPerHost: 128, MaxConnsPerHost: 256,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: normalizeBuildResponseHeaderTimeout(responseHeaderTimeout),
		ExpectContinueTimeout: time.Second,
	}
}

func normalizeBuildResponseHeaderTimeout(value time.Duration) time.Duration {
	if value <= 0 {
		return settingsdomain.DefaultBuildResponseHeaderTimeout
	}
	return value
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
	cacheRoute := buildPromptCacheRoute{}
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
	if request.Operation == conversation.OperationMessages && conversationOptions.AnthropicWebSearch {
		request.ReasoningReplayKey = ""
	}
	if compactionRequested {
		body, err = prepareGatewayCompactionSample(body)
		if err != nil {
			return invalidResponsesResponse(err), nil
		}
	}
	if len(body) > 0 && request.Method == http.MethodPost {
		if !compactionRequested {
			allowClientTools := request.AllowClientToolCacheRoute || (account.RoutingCandidate{Credential: request.Credential, Billing: request.Billing}).IsKnownFreeBuild()
			body, cacheRoute, err = prepareBuildPromptCacheRoute(body, request.Operation, request.Model, request.PromptCacheKey, allowClientTools)
			if err != nil {
				err = fmt.Errorf("准备 Build prompt cache 路由: %w", err)
				if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
					return invalidConversationResponse(request.Operation, err), nil
				}
				return invalidResponsesResponse(err), nil
			}
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
	// Explicit mode wins; in auto mode only confirmed Super accounts with bot_flag_source=1 default to XAI.
	primaryBase := a.primaryBaseURL()
	base := a.inferenceBaseForOperation(request.Credential, request.Billing, request.Method, request.Path)
	// Cache affinity and reasoning replay use separate identities. Replay is also bound to the actual account and upstream plane,
	// preventing opaque reasoning issued for one account or Build plane from reaching another scope.
	replayBaseBody := body
	body, replayKey := a.applyReasoningReplay(ctx, request, replayBaseBody, base)
	resp, reqURL, err := a.doResponseRequest(ctx, request, accessToken, body, base)
	if err != nil {
		return nil, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return nil, err
	}
	resp, reqURL, reasoningRecovery := a.recoverReasoningDecodeFailure(ctx, request, accessToken, body, base, replayKey, resp, reqURL)
	// Only eligible operations probe XAI with an equivalent request after the Build primary explicitly returns 403.
	if strings.EqualFold(base, primaryBase) && shouldProbeXAIInferenceFallback(request.Credential, request.Billing, request.Method, request.Path, resp.StatusCode) {
		// Buffer the primary 403 body and replay it unchanged if fallback fails; never issue a second primary POST.
		primaryBody, primaryTruncated, readErr := provider.ReadDiagnosticBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		primaryResp := cloneBufferedResponse(resp, primaryBody, primaryTruncated)
		fallbackBase := a.fallbackBaseURL()
		if fallbackBase != "" && !strings.EqualFold(fallbackBase, base) {
			fallbackBody, fallbackReplayKey := a.applyReasoningReplay(ctx, request, replayBaseBody, fallbackBase)
			fallbackCtx := infraegress.WithPhysicalCallStage(ctx, "plane_fallback")
			fallbackResp, fallbackURL, fallbackErr := a.doResponseRequest(fallbackCtx, request, accessToken, fallbackBody, fallbackBase)
			if fallbackErr == nil {
				fallbackErr = normalizeGzipResponse(fallbackResp)
			}
			fallbackRecovery := reasoningRecoveryOutcome{}
			if fallbackErr == nil {
				fallbackResp, fallbackURL, fallbackRecovery = a.recoverReasoningDecodeFailure(ctx, request, accessToken, fallbackBody, fallbackBase, fallbackReplayKey, fallbackResp, fallbackURL)
			}
			if fallbackErr == nil && isHTTPSuccess(fallbackResp.StatusCode) {
				a.activateBuildAPIFallback(ctx, &request.Credential)
				resp, reqURL, base, body, replayKey = fallbackResp, fallbackURL, fallbackBase, fallbackBody, fallbackReplayKey
				reasoningRecovery = reasoningRecovery.merge(fallbackRecovery)
			} else {
				if fallbackErr == nil {
					_ = fallbackResp.Body.Close()
				}
				// Preserve the original primary 403 URL and buffered body without requesting the primary again.
				resp = primaryResp
			}
		} else {
			resp = primaryResp
		}
	}
	modelCatalogChanged := a.modelCatalogChanged(request.Credential.ID, resp.Header.Get("x-models-etag"))
	// Capture or clear reasoning replay in the upstream Responses shape before protocol conversion.
	if a.shouldCaptureReplay(request, resp, replayKey) {
		resp.Body = a.replay.CaptureBody(resp.Body, request.Model, replayKey, request.Streaming, isCompactPath(request.Path))
	}
	// Replay must read the raw upstream output. Hide xAI native search subcalls from downstream clients
	// only after capture wrapping has completed.
	if isHTTPSuccess(resp.StatusCode) {
		if err := filterBuildPromptCacheResponse(resp, request.Streaming, cacheRoute); err != nil {
			return nil, err
		}
	}
	responsesOperation := request.Operation == "" || request.Operation == conversation.OperationResponses || request.Operation == conversation.OperationCompaction
	if responsesOperation && toolCompatibility != nil {
		if warnings := toolCompatibility.warningHeader(); warnings != "" {
			resp.Header.Set("X-Grok2API-Compatibility-Warnings", warnings)
		}
	}
	reasoningRecovery.appendWarnings(resp.Header)
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
		// compact does not inject history, but a successful request still clears old replay in the same scope.
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
	plane := "build"
	if fallback := a.fallbackBaseURL(); fallback != "" && strings.EqualFold(strings.TrimRight(base, "/"), fallback) {
		plane = "xai"
	}
	requestCtx = infraegress.WithPhysicalCallPlane(requestCtx, plane)
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

// applyGrokTurnIndexHeader forwards a real client turn only when the request has a stable Grok session.
func applyGrokTurnIndexHeader(request *http.Request, value string) {
	if request.Header.Get("x-grok-session-id") == "" {
		return
	}
	if turnIndex := normalizeGrokTurnIndex(value); turnIndex != "" {
		request.Header.Set("x-grok-turn-idx", turnIndex)
	}
}

// normalizeGrokTurnIndex accepts only non-negative decimal u64 values generated by an official client.
// Empty or invalid values are omitted; the gateway never fabricates turns from history, tool loops, or compaction.
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

// invalidResponsesResponse converts local protocol validation errors to a standard OpenAI error response,
// avoiding an upstream account retry.
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
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	// Always request the model catalog from the Build primary; do not preemptively switch to XAI because 1.5 or Super entitlement is absent.
	// NormalizeAccountModelCapabilities fills 1.5 locally from Billing paid or BuildSuperEntitled.
	models, status, err := a.listModelsAt(ctx, credential, accessToken, a.primaryBaseURL())
	if err != nil {
		return nil, err
	}
	if models != nil {
		return models, nil
	}
	return nil, fmt.Errorf("上游模型接口返回 %d", status)
}

// NormalizeAccountModelCapabilities normalizes 1.5 video entitlement from Super (Billing paid or BuildSuperEntitled).
// Super always includes grok-imagine-video-1.5; Free and Unknown remove it exactly. BuildAPIFallback is ignored.
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
		// After a process restart there is no in-memory catalog baseline. Let the Gateway perform one account-level
		// /models sync; recordModelsETag establishes the baseline after success.
		return true
	}
	return current != etag
}

func (a *Adapter) GetBilling(ctx context.Context, credential account.Credential) (account.Billing, error) {
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return account.Billing{}, err
	}
	billing, err := a.getBilling(ctx, credential, accessToken, "format=credits")
	if err != nil {
		return account.Billing{}, err
	}
	// A weekly quota at 0% usage cannot distinguish Free from a newly activated paid plan.
	// The official CLI uses /user?include=subscription for the live subscription tier, then falls back to the JWT tier.
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
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	refreshToken, err := a.cipher.Decrypt(credential.EncryptedRefreshToken)
	if err != nil {
		// Decryption failures are usually temporary or mismatched local encryption keys and are recoverable;
		// do not mark them permanent, or manual/batch refresh will never retry after the key is fixed.
		// True permanent OAuth failures such as invalid_grant are returned by oauth.refresh with Permanent=true.
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
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	return a.oauth.startDevice(ctx)
}

func (a *Adapter) PollDeviceAuthorization(ctx context.Context, deviceCode string) (provider.CredentialSeed, error) {
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
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
		// Set x-grok-conv-id and session-id only when a stable session exists.
		// Never generate a random UUID per request; it breaks xAI session affinity and keeps cached_tokens at zero.
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
		// The gateway cannot reliably recover the CLI prompt index from a stateless API request.
		// The field is optional in the official protocol, so do not fabricate x-grok-turn-idx.
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

// grokSessionID converts a stable session key to the upstream x-grok-conv-id.
// An empty key returns an empty string; stateless requests never receive a random ID.
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
