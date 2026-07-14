package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

var (
	ErrAuthorizationPending = errors.New("authorization pending")
	ErrSlowDown             = errors.New("authorization polling too fast")
	ErrAuthorizationDenied  = errors.New("authorization denied")
	ErrCredentialLimit      = errors.New("credential count exceeds limit")
	ErrUnauthorized         = errors.New("upstream credential unauthorized")
)

// CredentialRefreshError 区分需要重新认证的永久 OAuth 错误与可后台退避重试的临时错误。
type CredentialRefreshError struct {
	Status     int
	Code       string
	Permanent  bool
	RetryAfter time.Duration
	Cause      error
}

func (e *CredentialRefreshError) Error() string {
	if e == nil {
		return "credential refresh failed"
	}
	if e.Code != "" {
		return "credential refresh failed: " + e.Code
	}
	if e.Cause != nil {
		return "credential refresh failed: " + e.Cause.Error()
	}
	return "credential refresh failed"
}

func (e *CredentialRefreshError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ResponseResourceRequest 表示对 Responses 资源端点的通用上游请求。
type ResponseResourceRequest struct {
	Credential     account.Credential
	Method         string
	Path           string
	Body           []byte
	Model          string
	PromptCacheKey string
	IdempotencyID  string
	Streaming      bool
	NormalizeBody  bool
	Operation      string
}

// Response 表示尚未写入下游的上游响应。
type Response struct {
	StatusCode int
	Status     string
	Header     http.Header
	Body       io.ReadCloser
	QuotaUnits int
}

// DeviceAuthorization 表示 Device OAuth 启动结果。
type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	ExpiresIn               time.Duration
}

// CredentialSeed 表示登录或导入后尚未持久化的 OAuth 凭据。
type CredentialSeed struct {
	Provider     account.Provider
	AuthType     account.AuthType
	WebTier      account.WebTier
	Name         string
	Email        string
	UserID       string
	TeamID       string
	SourceKey    string
	OIDCClientID string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

type QuotaSnapshot struct {
	Tier     account.WebTier
	Windows  []account.QuotaWindow
	SyncedAt time.Time
}

type ImageGenerationRequest struct {
	Credential     account.Credential
	Model          string
	Prompt         string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	ResponseFormat string
	Streaming      bool
}

type ImageInput struct {
	Filename string
	MIMEType string
	Data     []byte
}

type ImageEditRequest struct {
	Credential     account.Credential
	Model          string
	Prompt         string
	ImageURLs      []string
	Count          int
	Resolution     string
	ResponseFormat string
}

type VideoRequest struct {
	Credential    account.Credential
	Prompt        string
	Duration      int
	AspectRatio   string
	Resolution    string
	ReferenceURLs []string
	Progress      func(int)
}

type VideoResult struct {
	URL         string
	ContentType string
}

// RefreshedCredential 表示 OAuth 刷新后的旋转凭据。
type RefreshedCredential struct {
	EncryptedAccessToken  string
	EncryptedRefreshToken string
	ExpiresAt             time.Time
}

// Adapter 只定义 Provider 身份；具体能力通过小接口按需注册。
type Adapter interface {
	Provider() account.Provider
}

type ResponseAdapter interface {
	Adapter
	ForwardResponse(ctx context.Context, request ResponseResourceRequest) (*Response, error)
}

type ModelCatalogAdapter interface {
	Adapter
	ListModels(ctx context.Context, credential account.Credential) ([]string, error)
}

type BillingAdapter interface {
	Adapter
	GetBilling(ctx context.Context, credential account.Credential) (account.Billing, error)
}

type CredentialRefreshAdapter interface {
	Adapter
	RefreshCredential(ctx context.Context, credential account.Credential) (RefreshedCredential, error)
}

type DeviceOAuthAdapter interface {
	Adapter
	StartDeviceAuthorization(ctx context.Context) (DeviceAuthorization, error)
	PollDeviceAuthorization(ctx context.Context, deviceCode string) (CredentialSeed, error)
}

type CredentialCodecAdapter interface {
	Adapter
	ParseImportedCredentials(data []byte) ([]CredentialSeed, error)
	MarshalCredentials(values []CredentialSeed) ([]byte, error)
}

type BuildCredentialConverter interface {
	Adapter
	ConvertToBuild(ctx context.Context, credential account.Credential) (CredentialSeed, error)
}

type QuotaAdapter interface {
	Adapter
	SyncQuota(ctx context.Context, credential account.Credential) (QuotaSnapshot, error)
	SyncQuotaMode(ctx context.Context, credential account.Credential, mode string) (account.QuotaWindow, error)
}

type ImageAdapter interface {
	Adapter
	GenerateImage(ctx context.Context, request ImageGenerationRequest) (*Response, error)
	EditImage(ctx context.Context, request ImageEditRequest) (*Response, error)
}

// ImageAssetStore 将生成图片归档为可由后端稳定读取的本地资源。
type ImageAssetStore interface {
	SaveImage(ctx context.Context, data []byte) (media.Asset, error)
	PublicImageURL(id string) string
}

type VideoAdapter interface {
	Adapter
	GenerateVideo(ctx context.Context, request VideoRequest) (VideoResult, error)
}

type RoutingMetadataAdapter interface {
	Adapter
	QuotaMode(upstreamModel string) string
	TierOrder(upstreamModel string) []account.WebTier
}

// ModelAlias 将隐藏兼容模型名解析到唯一公开路由，并可固定推理强度。
type ModelAlias struct {
	Alias           string
	PublicModel     string
	Provider        account.Provider
	UpstreamModel   string
	ReasoningEffort string
}

type ModelAliasAdapter interface {
	Adapter
	ModelAliases() []ModelAlias
}

// PricingMetadataAdapter 将 Provider 私有模型标识映射到公开计费模型。
type PricingMetadataAdapter interface {
	Adapter
	PricingModel(upstreamModel string) string
}

// Registry 保存已启用 Provider Adapter，不创建未实现来源的占位对象。
type Registry struct {
	adapters    map[account.Provider]Adapter
	definitions map[account.Provider]Definition
	aliases     map[string]ModelAlias
	issues      []error
}

func NewRegistry(adapters ...Adapter) *Registry {
	registry := &Registry{
		adapters:    make(map[account.Provider]Adapter, len(adapters)),
		definitions: make(map[account.Provider]Definition, len(adapters)),
		aliases:     make(map[string]ModelAlias),
	}
	for _, adapter := range adapters {
		if adapter == nil {
			registry.issues = append(registry.issues, errors.New("Provider Adapter 不能为空"))
			continue
		}
		providerValue := adapter.Provider()
		if !providerValue.IsValid() {
			registry.issues = append(registry.issues, fmt.Errorf("Provider Adapter 身份 %q 无效", providerValue))
			continue
		}
		if _, exists := registry.adapters[providerValue]; exists {
			registry.issues = append(registry.issues, fmt.Errorf("Provider %s 重复注册", providerValue))
			continue
		}
		registry.adapters[providerValue] = adapter
		if source, ok := adapter.(DefinitionAdapter); ok {
			registry.definitions[providerValue] = source.Definition().Clone()
		}
		if source, ok := adapter.(ModelAliasAdapter); ok {
			for _, value := range source.ModelAliases() {
				if value.Alias == "" || value.PublicModel == "" {
					continue
				}
				if value.Provider != providerValue {
					registry.issues = append(registry.issues, fmt.Errorf("Provider %s 的模型别名 %q 指向了 %s", providerValue, value.Alias, value.Provider))
					continue
				}
				if !modeldomain.IsCanonicalPublicID(value.Provider, value.PublicModel) {
					registry.issues = append(registry.issues, fmt.Errorf("Provider %s 的模型别名 %q 目标 %q 不是规范内部路由 ID", providerValue, value.Alias, value.PublicModel))
					continue
				}
				if existing, exists := registry.aliases[value.Alias]; exists {
					if existing != value {
						registry.issues = append(registry.issues, fmt.Errorf("模型别名 %q 重复注册", value.Alias))
					}
					continue
				}
				registry.aliases[value.Alias] = value
			}
		}
	}
	return registry
}

// Get 返回已注册的 Provider Adapter。
func (r *Registry) Get(value account.Provider) (Adapter, bool) {
	adapter, ok := r.adapters[value]
	return adapter, ok
}

// ResolveModelAlias 返回隐藏兼容模型名对应的规范内部路由。
func (r *Registry) ResolveModelAlias(value string) (ModelAlias, bool) {
	result, ok := r.aliases[value]
	return result, ok
}

// Definition 返回生产 Adapter 声明的稳定能力描述。
func (r *Registry) Definition(value account.Provider) (Definition, bool) {
	definition, ok := r.definitions[value]
	return definition.Clone(), ok
}

// Providers 返回按固定渠道顺序注册且具备能力描述的 Provider。
func (r *Registry) Providers() []account.Provider {
	values := make([]account.Provider, 0, len(r.definitions))
	for _, value := range account.Providers() {
		if _, ok := r.definitions[value]; ok {
			values = append(values, value)
		}
	}
	return values
}

// Validate 检查生产注册表的定义与实际小接口实现是否一致。
func (r *Registry) Validate() error {
	if r == nil {
		return errors.New("Provider Registry 不能为空")
	}
	if len(r.issues) > 0 {
		return errors.Join(r.issues...)
	}
	for _, value := range account.Providers() {
		adapter, registered := r.adapters[value]
		definition, described := r.definitions[value]
		if !registered || !described {
			return fmt.Errorf("Provider %s 未完整注册 Adapter 与 Definition", value)
		}
		if definition.Provider != value {
			return fmt.Errorf("Provider %s 的 Definition 身份不一致", value)
		}
		if err := definition.Validate(); err != nil {
			return err
		}
		if definition.Conversation.Responses || definition.Conversation.ChatCompletions || definition.Conversation.Messages {
			if _, ok := adapter.(ResponseAdapter); !ok {
				return fmt.Errorf("Provider %s 声明对话能力但未实现适配器", value)
			}
		}
		if _, ok := adapter.(ModelCatalogAdapter); !ok {
			return fmt.Errorf("Provider %s 未实现模型目录适配器", value)
		}
		switch definition.Quota {
		case QuotaBilling:
			if _, ok := adapter.(BillingAdapter); !ok {
				return fmt.Errorf("Provider %s 声明 Billing 额度但未实现适配器", value)
			}
		case QuotaRemoteWindow, QuotaLocalWindow:
			if _, ok := adapter.(QuotaAdapter); !ok {
				return fmt.Errorf("Provider %s 声明窗口额度但未实现适配器", value)
			}
		}
		if definition.Credential.Import {
			if _, ok := adapter.(CredentialCodecAdapter); !ok {
				return fmt.Errorf("Provider %s 声明凭据导入但未实现适配器", value)
			}
		}
		if definition.Credential.Refresh {
			if _, ok := adapter.(CredentialRefreshAdapter); !ok {
				return fmt.Errorf("Provider %s 声明凭据刷新但未实现适配器", value)
			}
		}
		if definition.Credential.DeviceOAuth {
			if _, ok := adapter.(DeviceOAuthAdapter); !ok {
				return fmt.Errorf("Provider %s 声明 Device OAuth 但未实现适配器", value)
			}
		}
		if definition.Media.ImageGeneration || definition.Media.ImageEdit {
			if _, ok := adapter.(ImageAdapter); !ok {
				return fmt.Errorf("Provider %s 声明图像能力但未实现适配器", value)
			}
		}
		if definition.Media.VideoGeneration {
			if _, ok := adapter.(VideoAdapter); !ok {
				return fmt.Errorf("Provider %s 声明视频能力但未实现适配器", value)
			}
		}
	}
	return nil
}

func (r *Registry) SupportsStoredResponses(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Conversation.StoredResponses
}

func (r *Registry) SupportsConversation(value account.Provider, operation string) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Conversation.Supports(operation)
}

func (r *Registry) SupportsResponseCompaction(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Conversation.Compact
}

func (r *Registry) SupportsCredentialRefresh(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Credential.Refresh
}

func (r *Registry) QuotaKind(value account.Provider) (QuotaKind, bool) {
	definition, ok := r.Definition(value)
	if !ok {
		return "", false
	}
	return definition.Quota, true
}

func (r *Registry) UsageKind(value account.Provider) (UsageKind, bool) {
	definition, ok := r.Definition(value)
	if !ok {
		return "", false
	}
	return definition.Inference.Usage, true
}

func (r *Registry) RetryForbiddenAsEgress(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Inference.RetryForbiddenAsEgress
}

func (r *Registry) Responses(value account.Provider) (ResponseAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(ResponseAdapter)
	return result, ok
}

func (r *Registry) Models(value account.Provider) (ModelCatalogAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(ModelCatalogAdapter)
	return result, ok
}

func (r *Registry) Billing(value account.Provider) (BillingAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(BillingAdapter)
	return result, ok
}

func (r *Registry) CredentialRefresh(value account.Provider) (CredentialRefreshAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(CredentialRefreshAdapter)
	return result, ok
}

func (r *Registry) DeviceOAuth(value account.Provider) (DeviceOAuthAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(DeviceOAuthAdapter)
	return result, ok
}

func (r *Registry) CredentialCodec(value account.Provider) (CredentialCodecAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(CredentialCodecAdapter)
	return result, ok
}

func (r *Registry) BuildConverter(value account.Provider) (BuildCredentialConverter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(BuildCredentialConverter)
	return result, ok
}

func (r *Registry) Quota(value account.Provider) (QuotaAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(QuotaAdapter)
	return result, ok
}

func (r *Registry) QuotaMode(value account.Provider, upstreamModel string) string {
	adapter, ok := r.Get(value)
	if !ok {
		return ""
	}
	metadata, ok := adapter.(RoutingMetadataAdapter)
	if !ok {
		return ""
	}
	return metadata.QuotaMode(upstreamModel)
}

func (r *Registry) TierOrder(value account.Provider, upstreamModel string) []account.WebTier {
	adapter, ok := r.Get(value)
	if !ok {
		return nil
	}
	metadata, ok := adapter.(RoutingMetadataAdapter)
	if !ok {
		return nil
	}
	return metadata.TierOrder(upstreamModel)
}

func (r *Registry) PricingModel(value account.Provider, upstreamModel string) string {
	adapter, ok := r.Get(value)
	if !ok {
		return upstreamModel
	}
	metadata, ok := adapter.(PricingMetadataAdapter)
	if !ok {
		return upstreamModel
	}
	if model := metadata.PricingModel(upstreamModel); model != "" {
		return model
	}
	return upstreamModel
}

func (r *Registry) Images(value account.Provider) (ImageAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(ImageAdapter)
	return result, ok
}

func (r *Registry) Videos(value account.Provider) (VideoAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(VideoAdapter)
	return result, ok
}
