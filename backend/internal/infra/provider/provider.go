package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// RetryAfterError preserves a safe upstream retry delay when an adapter cannot
// return a Response, for example when a WebSocket handshake is rejected.
type RetryAfterError interface {
	error
	RetryAfterDuration() time.Duration
}

// RequestScopedError marks an upstream rejection that retrying with another
// account or egress cannot resolve.
type RequestScopedError interface {
	error
	RequestScopedFailure() bool
}

// PublicMessageError exposes a deliberately sanitized message that may cross
// the public API boundary. Provider errors must opt in; arbitrary Error()
// strings can contain upstream response bodies, tokens, cookies, or request
// diagnostics and therefore are never returned to clients by default.
type PublicMessageError interface {
	error
	PublicErrorMessage() string
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

// VideoStage identifies which phase of an asynchronous video job failed.
type VideoStage string

const (
	// VideoStagePrepare is local work performed before the create request is sent.
	// It is not account-failover eligible because retrying deterministic local
	// validation or configuration failures against another credential is useless.
	VideoStagePrepare VideoStage = "prepare"
	// VideoStageCreate means the upstream explicitly rejected the create request.
	// Account failover is safe only for the retryable 4xx statuses selected by
	// the gateway; 5xx responses remain indeterminate because work may already
	// have been accepted before the server failed.
	VideoStageCreate VideoStage = "create"
	// VideoStageSubmitted means the create request may have reached upstream but
	// no usable job identifier was obtained. Retrying could duplicate work.
	VideoStageSubmitted VideoStage = "submitted"
	VideoStagePoll      VideoStage = "poll"
)

// VideoStageError records the asynchronous video phase without treating every
// create-path error as safe for account failover.
type VideoStageError struct {
	Stage  VideoStage
	Status int
	Err    error
}

func (e *VideoStageError) Error() string {
	if e == nil {
		return "video request failed"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Status > 0 {
		return fmt.Sprintf("video %s failed with status %d", e.Stage, e.Status)
	}
	return fmt.Sprintf("video %s failed", e.Stage)
}

func (e *VideoStageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *VideoStageError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	if e.Status > 0 {
		return e.Status
	}
	return ErrorHTTPStatusOrZero(e.Err)
}

// ErrorHTTPStatusOrZero extracts an upstream status or returns 0.
func ErrorHTTPStatusOrZero(err error) int {
	status, ok := ErrorHTTPStatus(err)
	if !ok {
		return 0
	}
	return status
}

// VideoErrorStage reports the video phase for an error chain.
func VideoErrorStage(err error) (VideoStage, bool) {
	var stageErr *VideoStageError
	if !errors.As(err, &stageErr) || stageErr == nil || stageErr.Stage == "" {
		return "", false
	}
	return stageErr.Stage, true
}

// VideoCreateFailureStage distinguishes an explicit upstream rejection from an
// indeterminate POST result. Explicit 4xx responses (including the 401
// sentinel) are rejections; transport errors and 5xx responses remain
// submitted because the upstream may already have accepted the job.
func VideoCreateFailureStage(err error) VideoStage {
	if errors.Is(err, ErrUnauthorized) {
		return VideoStageCreate
	}
	if status, ok := ErrorHTTPStatus(err); ok && status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return VideoStageCreate
	}
	return VideoStageSubmitted
}

// WrapVideoStage annotates err with the video phase and optional HTTP status.
func WrapVideoStage(stage VideoStage, status int, err error) error {
	if err == nil {
		return nil
	}
	var existing *VideoStageError
	if errors.As(err, &existing) {
		return err
	}
	if status <= 0 {
		status = ErrorHTTPStatusOrZero(err)
	}
	return &VideoStageError{Stage: stage, Status: status, Err: err}
}

// ErrorRetryAfter extracts a positive retry delay from an error chain.
func ErrorRetryAfter(err error) time.Duration {
	var retryError RetryAfterError
	if !errors.As(err, &retryError) {
		return 0
	}
	return max(0, retryError.RetryAfterDuration())
}

// IsRequestScopedError reports whether the Provider has positively classified
// the failure as request-scoped.
func IsRequestScopedError(err error) bool {
	var requestError RequestScopedError
	return errors.As(err, &requestError) && requestError.RequestScopedFailure()
}

// ErrorPublicMessage extracts a message that the Provider has explicitly
// classified as safe for clients.
func ErrorPublicMessage(err error) (string, bool) {
	var publicError PublicMessageError
	if !errors.As(err, &publicError) {
		return "", false
	}
	message := strings.TrimSpace(publicError.PublicErrorMessage())
	return message, message != ""
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
	Status  int
	Code    string
	Message string
	// Response is a bounded, redacted representation of the upstream OAuth
	// response. It is diagnostic only and must never contain credentials.
	Response   string
	Permanent  bool
	RetryAfter time.Duration
	Cause      error
}

func (e *CredentialRefreshError) Error() string {
	if e == nil {
		return "credential refresh failed"
	}
	if e.Code != "" {
		if e.Message != "" {
			return "credential refresh failed: " + e.Code + ": " + e.Message
		}
		return "credential refresh failed: " + e.Code
	}
	if e.Message != "" {
		return "credential refresh failed: " + e.Message
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
	// ForcedEgressNodeID is set only by administrator quality probes. It lets a
	// healthy credential test a quarantined node without changing its binding.
	ForcedEgressNodeID uint64
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

// QuotaGroupSnapshot is an authoritative snapshot for a group of quota modes
// returned by one upstream request. Modes lists the complete local scope so
// callers can atomically remove products that the upstream explicitly reports
// as unavailable without touching unrelated quota windows.
type QuotaGroupSnapshot struct {
	Group    string
	Modes    []string
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
	Quality        string
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
	Quality        string
	ResponseFormat string
	Streaming      bool
	PartialImages  int
}

// VideoOperation selects the official xAI video endpoint family.
type VideoOperation = media.VideoOperation

const (
	VideoOperationGenerate = media.VideoOperationGenerate
	VideoOperationEdit     = media.VideoOperationEdit
	VideoOperationExtend   = media.VideoOperationExtend
)

// ConsoleVideoMaxReferenceImages and ConsoleVideoMaxReferenceDurationSeconds
// describe the Console reference-to-video contract enforced by the upstream.
// They are shared by admission control and the Console adapter so invalid
// asynchronous jobs are rejected before enqueueing without weakening the
// adapter's final request-boundary validation.
const (
	ConsoleVideoMaxReferenceImages          = 7
	ConsoleVideoMaxReferenceDurationSeconds = 10
)

type VideoRequest struct {
	Credential account.Credential
	// Billing is used only to determine XAI eligibility in Build auto mode; nil means the account tier is unknown.
	Billing *account.Billing
	// JobID binds the local video job to XAI ZDR upload tickets and result assets.
	JobID string
	// Model is the selected upstream video model when the Provider supports more than one.
	Model string
	// Operation defaults to generate when empty.
	Operation   VideoOperation
	Prompt      string
	Duration    int
	AspectRatio string
	Resolution  string
	// ImageURL is the optional first-frame image (official "image" field).
	ImageURL string
	// ReferenceURLs are style/content references (official "reference_images").
	// A single reference must stay in reference_images and must not be coerced to image.
	// Official docs forbid combining image with reference_images.
	ReferenceURLs []string
	// ReferenceAudios are preset voice_ids for reference-to-video (official "reference_audios").
	// At most 3 entries; may be used alone or with reference_images.
	ReferenceAudios []string
	// VideoURL is required for edit/extend (official "video" field).
	VideoURL string
	Progress func(int)
}

type VideoResult struct {
	URL         string
	ContentType string
	// A non-empty AssetID means the result is stored as a local media asset; content reads must use MediaObjectStorage.
	AssetID string
}

type TTSOutputFormat struct {
	Codec      string
	SampleRate int
	BitRate    int
}

type TTSRequest struct {
	Credential               account.Credential
	Model                    string
	Text                     string
	VoiceID                  string
	Language                 string
	OutputFormat             TTSOutputFormat
	Speed                    float64
	OptimizeStreamingLatency int
	TextNormalization        bool
	WithTimestamps           bool
}

type TTSTimestampSpan struct {
	Start float64
	End   float64
}

type TTSTimestamps struct {
	GraphChars []string
	GraphTimes []TTSTimestampSpan
}

type TTSResult struct {
	Audio        []byte
	ContentType  string
	Duration     float64
	Base64Audio  string
	Timestamps   *TTSTimestamps
	JSONEnvelope bool
}

type STTRequest struct {
	Credential   account.Credential
	Model        string
	FileName     string
	FileMIME     string
	FileData     []byte
	URL          string
	AudioFormat  string
	SampleRate   string
	Language     string
	Format       bool
	Multichannel bool
	Channels     int
	Diarize      bool
	KeyTerms     []string
	FillerWords  bool
	VADThreshold *float64
}

type STTWord struct {
	Text    string
	Start   float64
	End     float64
	Speaker *int
}

type STTChannel struct {
	Index int
	Text  string
	Words []STTWord
}

type STTResult struct {
	Text     string
	Language string
	Duration float64
	Words    []STTWord
	Channels []STTChannel
	RawJSON  []byte
}

type VoiceInfo struct {
	VoiceID  string
	Name     string
	Language string
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
	// BuildBotFlagInspected is true only when the Build token was successfully
	// decrypted and decoded. False means the risk source is unknown, not clean.
	BuildBotFlagInspected bool
	// BuildBotFlagged is true when BuildBotFlagSource is 1 or 2.
	BuildBotFlagged bool
	// BuildBotFlagSource is the numeric bot_flag_source/bfs claim (1 or 2), or 0 when unset.
	BuildBotFlagSource int
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

// QuotaGroupAdapter is optional. It is used when one upstream endpoint returns
// several related quota products as one authoritative response.
type QuotaGroupAdapter interface {
	Adapter
	SyncQuotaGroup(ctx context.Context, credential account.Credential, group string) (QuotaGroupSnapshot, error)
}

// QuotaRefreshMetadataAdapter maps a model to an internal refresh group. The
// group is scheduling metadata, not a routable quota mode.
type QuotaRefreshMetadataAdapter interface {
	Adapter
	QuotaRefreshGroup(upstreamModel string) string
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

// TTSAdapter synthesizes speech audio for text prompts.
type TTSAdapter interface {
	Adapter
	SynthesizeSpeech(ctx context.Context, request TTSRequest) (TTSResult, error)
	ListTTSVoices(ctx context.Context, credential account.Credential) ([]VoiceInfo, error)
	GetTTSVoice(ctx context.Context, credential account.Credential, voiceID string) (VoiceInfo, error)
}

// STTAdapter transcribes audio into text.
type STTAdapter interface {
	Adapter
	TranscribeSpeech(ctx context.Context, request STTRequest) (STTResult, error)
}

// VoiceWebSocketConn is a minimal duplex websocket used by voice streaming proxies.
type VoiceWebSocketConn interface {
	ReadMessage() (messageType int, data []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetReadLimit(limit int64)
	Close() error
}

// VoiceWebSocketRequest dials an upstream voice websocket with provider auth.
type VoiceWebSocketRequest struct {
	Credential account.Credential
	// Path is a v1-relative path such as /realtime or /stt.
	Path  string
	Model string
}

// VoiceWebSocketAdapter dials official voice websocket endpoints with account auth.
type VoiceWebSocketAdapter interface {
	Adapter
	DialVoiceWebSocket(ctx context.Context, request VoiceWebSocketRequest) (VoiceWebSocketConn, func(), error)
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
		if definition.Media.TTS {
			if _, ok := adapter.(TTSAdapter); !ok {
				return fmt.Errorf("Provider %s 声明语音合成能力但未实现适配器", value)
			}
		}
		if definition.Media.STT {
			if _, ok := adapter.(STTAdapter); !ok {
				return fmt.Errorf("Provider %s 声明语音识别能力但未实现适配器", value)
			}
		}
		if definition.Media.Realtime {
			if _, ok := adapter.(VoiceWebSocketAdapter); !ok {
				return fmt.Errorf("Provider %s 声明实时语音能力但未实现 WebSocket 适配器", value)
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

func (r *Registry) QuotaGroup(value account.Provider) (QuotaGroupAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(QuotaGroupAdapter)
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

func (r *Registry) QuotaRefreshGroup(value account.Provider, upstreamModel string) string {
	adapter, ok := r.Get(value)
	if !ok {
		return ""
	}
	metadata, ok := adapter.(QuotaRefreshMetadataAdapter)
	if !ok {
		return ""
	}
	return metadata.QuotaRefreshGroup(upstreamModel)
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

func (r *Registry) TTS(value account.Provider) (TTSAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(TTSAdapter)
	return result, ok
}

func (r *Registry) STT(value account.Provider) (STTAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(STTAdapter)
	return result, ok
}

func (r *Registry) VoiceWebSocket(value account.Provider) (VoiceWebSocketAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(VoiceWebSocketAdapter)
	return result, ok
}

// CredentialRejection 表示上游响应或错误是否构成「凭据被拒」的稳定判定。
// 与网关 UpstreamFailure 的 CredentialRejected / PermanentAccountDenial / SpendingLimitBlocked 分类保持一致，
// 供 account.Service 等非网关路径复用同一套失效收敛语义。
type CredentialRejection struct {
	// Rejected 表示该响应/错误应被认定为凭据级失效（需标 reauthRequired）。
	Rejected bool
	// PermanentAccountDenial 表示上游明确拒绝该账号访问聊天端点（非凭据本身失效）。
	// Build 账号此类拒绝按现有网关逻辑是 model-scoped，不应标 reauth；仅 Rejected 为真时才标。
	// 管理端 detect 路径同样仅持久化模型阻断，避免把仍可用于其他模型的账号移出号池。
	PermanentAccountDenial bool
	// SpendingLimitBlocked 表示付费账号被 spending-limit 永久阻断（402/403 personal-team-blocked:spending-limit），
	// 由调用方写入额度恢复状态，不应误判为 OAuth 凭据失效。
	SpendingLimitBlocked bool
	// QuotaExhausted 表示请求被账号级或模型级额度限制拒绝。
	QuotaExhausted bool
	// FreeQuotaExhausted 表示免费额度已经耗尽。
	FreeQuotaExhausted bool
	// ModelQuotaExhausted 表示额度限制只针对当前模型。
	ModelQuotaExhausted bool
}

// ClassifyCredentialRejection 按上游 HTTP 状态码与错误体判定凭据是否被拒。
// status 为上游 HTTP 状态；body 为响应正文（可为 nil）；err 为 Provider 返回的错误（可为 nil）。
func ClassifyCredentialRejection(status int, body []byte, err error) CredentialRejection {
	var result CredentialRejection
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			result.Rejected = true
			return result
		}
		if httpStatus, ok := ErrorHTTPStatus(err); ok && httpStatus == http.StatusUnauthorized {
			result.Rejected = true
			return result
		}
	}
	switch status {
	case http.StatusUnauthorized:
		result.Rejected = true
	case http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		upstreamCode, upstreamType, upstreamMessage := ExtractUpstreamErrorMetadata(body)
		metadataText := strings.ToLower(strings.Join([]string{upstreamCode, upstreamType, upstreamMessage}, " "))
		result.SpendingLimitBlocked = strings.Contains(metadataText, "personal-team-blocked:spending-limit")
		result.ModelQuotaExhausted = strings.Contains(metadataText, "used all the included free usage for model")
		result.FreeQuotaExhausted = result.ModelQuotaExhausted || strings.Contains(metadataText, "subscription:free-usage-exhausted")
		creditExhausted := ContainsAny(metadataText, "run out of credits", "out of credits", "usage balance exhausted", "usage limit reached")
		result.QuotaExhausted = status == http.StatusPaymentRequired || result.SpendingLimitBlocked || result.FreeQuotaExhausted || creditExhausted
		permanentDenial := IsPermanentAccountDenial(metadataText)
		result.PermanentAccountDenial = permanentDenial
		if status == http.StatusForbidden {
			result.Rejected = !result.QuotaExhausted && !permanentDenial && ContainsAny(metadataText,
				"authentication", "unauthorized", "invalid token", "token expired")
		}
	}
	return result
}

// ExtractUpstreamErrorMetadata 从上游错误响应正文中提取 code/type/message 三元组。
func ExtractUpstreamErrorMetadata(body []byte) (string, string, string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return "", "", strings.TrimSpace(string(body))
	}
	root, ok := payload.(map[string]any)
	if !ok {
		return "", "", ""
	}
	if nested, ok := root["error"].(map[string]any); ok {
		code := FirstNonEmptyFailure(firstStringValue(nested, "code", "error_code"), firstStringValue(root, "code", "error_code"))
		errorType := FirstNonEmptyFailure(firstStringValue(nested, "type", "error_type"), firstStringValue(root, "type", "error_type"))
		message := FirstNonEmptyFailure(firstStringValue(nested, "message", "error"), firstStringValue(root, "message"))
		return code, errorType, message
	}
	message := FirstNonEmptyFailure(firstStringValue(root, "error"), firstStringValue(root, "message"))
	return firstStringValue(root, "code", "error_code"), firstStringValue(root, "type", "error_type"), message
}

// IsPermanentAccountDenial 判定 403 是否为「账号被永久拒绝访问聊天端点」。
func IsPermanentAccountDenial(text string) bool {
	if strings.Contains(text, "access to the chat endpoint is denied") {
		return true
	}
	return strings.Trim(strings.TrimSpace(text), " .!\t\r\n") == "access denied"
}

// ContainsAny 报告 text 是否包含任意一个 signal 子串。
func ContainsAny(text string, signals ...string) bool {
	for _, signal := range signals {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func firstStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			if s, ok := value.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// FirstNonEmptyFailure 返回第一个非空白字符串。
func FirstNonEmptyFailure(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
