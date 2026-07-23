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
	ErrBirthDateAlreadySet  = errors.New("upstream birth date is already set")
)

// HTTPStatusError preserves the upstream status when a streaming or asynchronous Provider cannot return a Response.
type HTTPStatusError interface {
	error
	HTTPStatusCode() int
}

// ErrorHTTPStatus extracts the upstream HTTP status from a Provider error chain.
func ErrorHTTPStatus(err error) (int, bool) {
	var statusError HTTPStatusError
	if !errors.As(err, &statusError) {
		return 0, false
	}
	status := statusError.HTTPStatusCode()
	return status, status > 0
}

// MediaPostProcessingStage identifies a local processing stage that failed after media generation.
type MediaPostProcessingStage string

const (
	MediaPostProcessingDownload MediaPostProcessingStage = "download"
	MediaPostProcessingStorage  MediaPostProcessingStage = "storage"
)

// MediaPostProcessingError indicates that upstream media was created but download or storage failed.
// These errors must not trigger generation on another account or reduce the generating account's health.
type MediaPostProcessingError struct {
	Stage MediaPostProcessingStage
	Cause error
}

func (e *MediaPostProcessingError) Error() string {
	if e == nil || e.Cause == nil {
		return "media post-processing failed"
	}
	return fmt.Sprintf("media post-processing %s failed: %v", e.Stage, e.Cause)
}

func (e *MediaPostProcessingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewMediaPostProcessingError marks a download or storage error as non-retryable across accounts.
func NewMediaPostProcessingError(stage MediaPostProcessingStage, cause error) error {
	if cause == nil {
		return nil
	}
	return &MediaPostProcessingError{Stage: stage, Cause: cause}
}

// IsMediaPostProcessingError reports whether an error occurred during local processing after media generation.
func IsMediaPostProcessingError(err error) bool {
	var target *MediaPostProcessingError
	return errors.As(err, &target)
}

// CredentialRefreshError distinguishes permanent OAuth errors requiring reauthorization from temporary errors that can retry with backoff.
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

// ResponseResourceRequest describes a common upstream request to a Responses resource endpoint.
type ResponseResourceRequest struct {
	Credential account.Credential
	// Billing is used only to determine XAI eligibility in Build auto mode; nil means the account tier is unknown.
	Billing        *account.Billing
	Method         string
	Path           string
	Body           []byte
	Model          string
	PromptCacheKey string
	// ReasoningReplayKey comes only from explicit client session identity; soft cache identity must not replay ciphertext.
	ReasoningReplayKey string
	// AllowClientToolCacheRoute allows the Build native cache route to supplement existing client tools.
	// This is a protocol compatibility signal, not a client authentication result.
	AllowClientToolCacheRoute bool
	// GrokTurnIndex is the explicit Grok Shell client turn; it is validated before Build egress and never fabricated by the server.
	GrokTurnIndex string
	IdempotencyID string
	Streaming     bool
	NormalizeBody bool
	Operation     string
}

// Response represents an upstream response that has not yet been written downstream.
type Response struct {
	StatusCode  int
	Status      string
	Header      http.Header
	Body        io.ReadCloser
	QuotaUnits  int
	UpstreamURL string
	Diagnostic  *DiagnosticResponse
	// RecoveredPrimaryFailure records a primary-plane failure hidden by a successful Provider fallback.
	RecoveredPrimaryFailure *DiagnosticResponse
	RateLimit               *RateLimitMetadata
	// ModelCatalogChanged indicates that the model catalog ETag in an inference response differs from
	// the ETag from the account's most recent successful /models sync.
	ModelCatalogChanged bool
}

const (
	RateLimitScopeRPS = "rps"
	RateLimitScopeRPM = "rpm"
)

// RateLimitMetadata contains transient rate-limit metadata that is safe to propagate from upstream.
type RateLimitMetadata struct {
	Scope      string
	TeamID     string
	Model      string
	Actual     int
	Limit      int
	RetryAfter time.Duration
}

const MaxDiagnosticBodyBytes = 64 << 10

// DiagnosticResponse retains a size-limited failure response before Provider conversion.
type DiagnosticResponse struct {
	StatusCode    int
	Status        string
	Header        http.Header
	Body          []byte
	BodyTruncated bool
}

// ReadDiagnosticBody reads up to the diagnostic body limit and reports whether upstream content was truncated.
func ReadDiagnosticBody(body io.Reader) ([]byte, bool, error) {
	if body == nil {
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, MaxDiagnosticBodyBytes+1))
	if len(data) <= MaxDiagnosticBodyBytes {
		return data, false, err
	}
	return data[:MaxDiagnosticBodyBytes], true, err
}

// DeviceAuthorization represents the result of starting Device OAuth.
type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	ExpiresIn               time.Duration
}

// CredentialSeed represents an OAuth credential not yet persisted after login or import.
type CredentialSeed struct {
	Provider                account.Provider
	AuthType                account.AuthType
	WebTier                 account.WebTier
	Name                    string
	Email                   string
	UserID                  string
	TeamID                  string
	SourceKey               string
	OIDCClientID            string
	AccessToken             string
	RefreshToken            string
	CloudflareCookies       string
	ExpiresAt               time.Time
	WebNSFWEnabledAt        *time.Time
	WebTermsAcceptedAt      *time.Time
	WebTermsAcceptedVersion int
	WebBirthDateSetAt       *time.Time
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
	PartialImages  int
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
	Size           string
	AspectRatio    string
	Resolution     string
	ResponseFormat string
	Streaming      bool
	PartialImages  int
}

type VideoRequest struct {
	Credential account.Credential
	// Billing is used only to determine XAI eligibility in Build auto mode; nil means the account tier is unknown.
	Billing *account.Billing
	// JobID binds the local video job to XAI ZDR upload tickets and result assets.
	JobID         string
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
	// A non-empty AssetID means the result is stored as a local media asset; content reads must use MediaObjectStorage.
	AssetID string
}

// RefreshedCredential represents rotated credentials returned by an OAuth refresh.
type RefreshedCredential struct {
	EncryptedAccessToken  string
	EncryptedRefreshToken string
	ExpiresAt             time.Time
}

// Adapter defines only Provider identity; concrete capabilities are registered through small interfaces as needed.
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

// AccountModelCapabilityNormalizer is optional and normalizes account model capabilities from Billing and credential entitlement.
// Without it, model sync writes the upstream catalog unchanged; nil billing means Unknown with no snapshot.
// credential is used for Build Super entitlement; default Providers may ignore it.
type AccountModelCapabilityNormalizer interface {
	Adapter
	NormalizeAccountModelCapabilities(models []string, billing *account.Billing, credential account.Credential) []string
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

// CredentialMetadata contains non-sensitive display data safely derived from a stored credential.
// Raw tokens and complete JWT claims must never be exposed through this structure.
type CredentialMetadata struct {
	BuildBotFlagged bool
}

type CredentialMetadataAdapter interface {
	Adapter
	CredentialMetadata(credential account.Credential) CredentialMetadata
}

// AccountIdentity contains non-sensitive account identity metadata confirmed by upstream.
// Email is for display only; cross-Provider automatic linking uses stable UserID only.
type AccountIdentity struct {
	Email  string
	UserID string
	TeamID string
}

type AccountIdentityAdapter interface {
	Adapter
	SyncAccountIdentity(ctx context.Context, credential account.Credential) (AccountIdentity, error)
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

// WebAccountSettingsAdapter defines upstream profile-setting capabilities for Grok Web SSO accounts.
// This capability belongs only to the Web Provider; Build and Console must not emulate it through generic account logic.
type WebAccountSettingsAdapter interface {
	Adapter
	AcceptTerms(ctx context.Context, credential account.Credential) error
	SetBirthDate(ctx context.Context, credential account.Credential, birthDate time.Time) error
	EnableNSFW(ctx context.Context, credential account.Credential) error
}

// ImageGenerationAdapter defines an optional Provider image-generation capability.
type ImageGenerationAdapter interface {
	Adapter
	GenerateImage(ctx context.Context, request ImageGenerationRequest) (*Response, error)
}

// ImageEditAdapter defines an optional Provider image-editing capability.
type ImageEditAdapter interface {
	Adapter
	EditImage(ctx context.Context, request ImageEditRequest) (*Response, error)
}

// ImageAssetStore archives generated images as local resources that the backend can read reliably.
type ImageAssetStore interface {
	SaveImage(ctx context.Context, data []byte) (media.Asset, error)
	PublicImageURL(id string) string
}

type VideoAdapter interface {
	Adapter
	GenerateVideo(ctx context.Context, request VideoRequest) (VideoResult, error)
}

// VideoContentDownloader reads completed video content using the credential that created the task.
// Callers must verify task ownership first.
type VideoContentDownloader interface {
	VideoAdapter
	DownloadVideo(ctx context.Context, credential account.Credential, rawURL string) (io.ReadCloser, string, int64, error)
}

type RoutingMetadataAdapter interface {
	Adapter
	QuotaMode(upstreamModel string) string
	TierOrder(upstreamModel string) []account.WebTier
}

// ModelAlias resolves a hidden compatibility model name to one public route and can fix reasoning effort.
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

// PricingMetadataAdapter maps Provider-private model identifiers to public billing models.
type PricingMetadataAdapter interface {
	Adapter
	PricingModel(upstreamModel string) string
}

// Registry stores enabled Provider Adapters and does not create placeholders for unsupported sources.
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

// Get returns a registered Provider Adapter.
func (r *Registry) Get(value account.Provider) (Adapter, bool) {
	adapter, ok := r.adapters[value]
	return adapter, ok
}

// ResolveModelAlias returns the canonical internal route for a hidden compatibility model name.
func (r *Registry) ResolveModelAlias(value string) (ModelAlias, bool) {
	result, ok := r.aliases[value]
	return result, ok
}

// Definition returns the stable capability declaration from a production Adapter.
func (r *Registry) Definition(value account.Provider) (Definition, bool) {
	definition, ok := r.definitions[value]
	return definition.Clone(), ok
}

// Providers returns registered Providers in fixed channel order with capability definitions.
func (r *Registry) Providers() []account.Provider {
	values := make([]account.Provider, 0, len(r.definitions))
	for _, value := range account.Providers() {
		if _, ok := r.definitions[value]; ok {
			values = append(values, value)
		}
	}
	return values
}

// Validate checks that production registry definitions match their implemented capability interfaces.
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
		if definition.Media.ImageGeneration {
			if _, ok := adapter.(ImageGenerationAdapter); !ok {
				return fmt.Errorf("Provider %s 声明图像生成能力但未实现适配器", value)
			}
		}
		if definition.Media.ImageEdit {
			if _, ok := adapter.(ImageEditAdapter); !ok {
				return fmt.Errorf("Provider %s 声明图像编辑能力但未实现适配器", value)
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

// CredentialMetadata returns derived credential metadata safe for admin display.
func (r *Registry) CredentialMetadata(credential account.Credential) CredentialMetadata {
	if r == nil {
		return CredentialMetadata{}
	}
	adapter, ok := r.adapters[credential.Provider]
	if !ok {
		return CredentialMetadata{}
	}
	inspector, ok := adapter.(CredentialMetadataAdapter)
	if !ok {
		return CredentialMetadata{}
	}
	return inspector.CredentialMetadata(credential)
}

func (r *Registry) AccountIdentity(value account.Provider) (AccountIdentityAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(AccountIdentityAdapter)
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

// WebAccountSettings returns the Grok Web-specific account profile settings capability.
func (r *Registry) WebAccountSettings() (WebAccountSettingsAdapter, bool) {
	adapter, ok := r.Get(account.ProviderWeb)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(WebAccountSettingsAdapter)
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

// ImageGeneration returns the image-generation capability registered by the Provider.
func (r *Registry) ImageGeneration(value account.Provider) (ImageGenerationAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(ImageGenerationAdapter)
	return result, ok
}

// ImageEdit returns the image-editing capability registered by the Provider.
func (r *Registry) ImageEdit(value account.Provider) (ImageEditAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(ImageEditAdapter)
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
