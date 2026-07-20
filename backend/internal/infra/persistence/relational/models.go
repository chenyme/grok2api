package relational

import "time"

type adminModel struct {
	ID           uint64    `gorm:"primaryKey;autoIncrement"`
	Username     string    `gorm:"size:100;uniqueIndex;not null;check:chk_admins_username,length(trim(username)) BETWEEN 1 AND 100"`
	PasswordHash string    `gorm:"size:255;not null"`
	CreatedAt    time.Time `gorm:"not null"`
	UpdatedAt    time.Time `gorm:"not null"`
}

func (adminModel) TableName() string { return "admins" }

type adminSessionModel struct {
	ID               uint64    `gorm:"primaryKey;autoIncrement"`
	AdminID          uint64    `gorm:"not null"`
	RefreshTokenHash string    `gorm:"size:64;uniqueIndex;not null;check:chk_admin_sessions_token_hash,length(refresh_token_hash) = 64"`
	ExpiresAt        time.Time `gorm:"not null"`
	LastUsedAt       *time.Time
	CreatedAt        time.Time   `gorm:"not null"`
	Admin            *adminModel `gorm:"foreignKey:AdminID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (adminSessionModel) TableName() string { return "admin_sessions" }

type accountModel struct {
	ID          uint64 `gorm:"primaryKey;autoIncrement"`
	IdentityKey string `gorm:"size:64;uniqueIndex;not null;check:chk_accounts_identity_key,length(identity_key) = 64"`
	Provider    string `gorm:"size:32;not null;check:chk_accounts_provider,provider IN ('grok_build','grok_web','grok_console');index:idx_accounts_provider_source,priority:1"`
	Name        string `gorm:"size:160;not null;check:chk_accounts_name,length(trim(name)) BETWEEN 1 AND 160"`
	Email       string `gorm:"size:255;check:chk_accounts_email,length(email) <= 255"`
	UserID      string `gorm:"size:255;check:chk_accounts_user_id,length(user_id) <= 255"`
	TeamID      string `gorm:"size:255;check:chk_accounts_team_id,length(team_id) <= 255"`
	SourceKey   string `gorm:"size:512;not null;check:chk_accounts_source_key,length(trim(source_key)) BETWEEN 1 AND 512;index:idx_accounts_provider_source,priority:2"`
	Enabled     bool   `gorm:"not null"`
	AuthStatus  string `gorm:"size:32;not null;check:chk_accounts_auth_status,auth_status IN ('active','reauthRequired')"`
	// ReauthMarkedAt 进入 reauthRequired 的时刻；active 时为 NULL。
	ReauthMarkedAt   *time.Time
	Priority         int     `gorm:"not null;default:1"`
	MaxConcurrent    int     `gorm:"not null;default:8;check:chk_accounts_max_concurrent,max_concurrent BETWEEN 1 AND 256"`
	MinimumRemaining float64 `gorm:"not null;check:chk_accounts_minimum_remaining,minimum_remaining >= 0"`
	FailureCount     int     `gorm:"not null;check:chk_accounts_failure_count,failure_count >= 0"`
	CooldownUntil    *time.Time
	LastError        string `gorm:"size:512;check:chk_accounts_last_error,length(last_error) <= 512"`
	LastUsedAt       *time.Time
	ObservedModel    string `gorm:"size:255;check:chk_accounts_observed_model,length(observed_model) <= 255"`
	ObservedModelAt  *time.Time
	// BuildAPIFallback 仅对 grok_build 有意义：XAI 推理回退标记；其他 Provider 保持 false。
	BuildAPIFallback bool `gorm:"not null;default:false"`
	// BuildRouteMode 仅控制 grok_build 推理地址；其它 Provider 固定 auto。
	BuildRouteMode string `gorm:"size:16;not null;default:auto;check:chk_accounts_build_route_mode,build_route_mode IN ('auto','build','xai')"`
	// BuildSuperEntitled 仅对 grok_build 有意义：管理员确认的 Super/1.5 entitlement；其他 Provider 保持 false。
	BuildSuperEntitled bool                    `gorm:"not null;default:false"`
	CreatedAt          time.Time               `gorm:"not null"`
	UpdatedAt          time.Time               `gorm:"not null"`
	Credential         *accountCredentialModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
	WebProfile         *webAccountProfileModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (accountModel) TableName() string { return "provider_accounts" }

type accountCredentialModel struct {
	AccountID                 uint64 `gorm:"primaryKey"`
	AuthType                  string `gorm:"size:16;not null;check:chk_account_credentials_auth_type,auth_type IN ('oauth','sso')"`
	ClientID                  string `gorm:"size:255;check:chk_account_credentials_client_id,length(client_id) <= 255"`
	EncryptedPrimary          string `gorm:"type:text;not null;default:'';check:chk_account_credentials_secret,((auth_type = 'oauth' AND (encrypted_primary <> '' OR encrypted_refresh <> '')) OR (auth_type = 'sso' AND encrypted_primary <> '' AND encrypted_refresh = '')) AND length(encrypted_primary) <= 65536 AND length(encrypted_refresh) <= 65536"`
	EncryptedRefresh          string `gorm:"type:text;not null;default:''"`
	EncryptedCloudflareCookie string `gorm:"type:text;not null;default:'';check:chk_account_credentials_cf_cookie,length(encrypted_cloudflare_cookie) <= 65536"`
	ExpiresAt                 *time.Time
	RefreshDueAt              *time.Time
	LastRefreshAt             *time.Time
	RefreshFailures           int           `gorm:"not null;default:0;check:chk_account_credentials_refresh_failures,refresh_failures >= 0"`
	LastRefreshError          string        `gorm:"size:100;not null;default:'';check:chk_account_credentials_refresh_error,length(last_refresh_error) <= 100"`
	RefreshPermanent          bool          `gorm:"not null;default:false"`
	UpdatedAt                 time.Time     `gorm:"not null"`
	Account                   *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (accountCredentialModel) TableName() string { return "account_credentials" }

type accountProviderLinkModel struct {
	WebAccountID   uint64        `gorm:"primaryKey"`
	BuildAccountID uint64        `gorm:"uniqueIndex;not null;check:chk_account_provider_links_distinct,web_account_id <> build_account_id"`
	CreatedAt      time.Time     `gorm:"not null"`
	WebAccount     *accountModel `gorm:"foreignKey:WebAccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
	BuildAccount   *accountModel `gorm:"foreignKey:BuildAccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (accountProviderLinkModel) TableName() string { return "account_provider_links" }

type webConsoleAccountLinkModel struct {
	WebAccountID     uint64        `gorm:"primaryKey"`
	ConsoleAccountID uint64        `gorm:"uniqueIndex;not null;check:chk_web_console_account_links_distinct,web_account_id <> console_account_id"`
	CreatedAt        time.Time     `gorm:"not null"`
	WebAccount       *accountModel `gorm:"foreignKey:WebAccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
	ConsoleAccount   *accountModel `gorm:"foreignKey:ConsoleAccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (webConsoleAccountLinkModel) TableName() string { return "web_console_account_links" }

type webAccountProfileModel struct {
	AccountID            uint64 `gorm:"primaryKey"`
	Tier                 string `gorm:"size:16;not null;check:chk_web_account_profiles_tier,tier IN ('auto','basic','super','heavy')"`
	SyncedAt             *time.Time
	NSFWEnabledAt        *time.Time
	TermsAcceptedAt      *time.Time
	TermsAcceptedVersion int `gorm:"not null;default:0"`
	BirthDateSetAt       *time.Time
	EgressIdentity       string        `gorm:"size:128;not null;default:'';check:chk_web_account_profiles_egress_identity,length(egress_identity) <= 128"`
	Account              *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (webAccountProfileModel) TableName() string { return "web_account_profiles" }

type quotaWindowModel struct {
	AccountID     uint64  `gorm:"primaryKey"`
	Mode          string  `gorm:"size:64;primaryKey;not null;check:chk_account_quota_windows_mode,length(trim(mode)) BETWEEN 1 AND 64"`
	Remaining     int     `gorm:"not null;check:chk_account_quota_windows_remaining,remaining >= 0"`
	Total         int     `gorm:"not null;check:chk_account_quota_windows_total,total >= 0"`
	UsagePercent  float64 `gorm:"not null;default:0;check:chk_account_quota_windows_usage_percent,usage_percent >= 0 AND usage_percent <= 100"`
	BreakdownJSON string  `gorm:"type:text;not null;default:'[]';check:chk_account_quota_windows_breakdown,length(breakdown_json) <= 8192"`
	WindowSeconds int     `gorm:"not null;check:chk_account_quota_windows_window,window_seconds >= 0"`
	ResetAt       *time.Time
	SyncedAt      *time.Time
	Source        string        `gorm:"size:16;not null;check:chk_account_quota_windows_source,source IN ('default','estimated','upstream')"`
	UpdatedAt     time.Time     `gorm:"not null"`
	Account       *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (quotaWindowModel) TableName() string { return "account_quota_windows" }

type billingModel struct {
	AccountID            uint64  `gorm:"primaryKey"`
	PlanCode             string  `gorm:"size:100;check:chk_billing_plan_code,length(plan_code) <= 100"`
	PlanName             string  `gorm:"size:160;check:chk_billing_plan_name,length(plan_name) <= 160"`
	MonthlyLimit         float64 `gorm:"not null;default:0;check:chk_billing_monthly_limit,monthly_limit >= 0"`
	Used                 float64 `gorm:"not null;default:0;check:chk_billing_used,used >= 0"`
	OnDemandCap          float64 `gorm:"not null;default:0;check:chk_billing_on_demand_cap,on_demand_cap >= 0"`
	OnDemandUsed         float64 `gorm:"not null;default:0;check:chk_billing_on_demand_used,on_demand_used >= 0"`
	PrepaidBalance       float64 `gorm:"not null;default:0;check:chk_billing_prepaid_balance,prepaid_balance >= 0"`
	CreditUsagePercent   float64 `gorm:"not null;default:0;check:chk_billing_credit_usage_percent,credit_usage_percent >= 0"`
	IsUnifiedBillingUser bool    `gorm:"not null;default:false"`
	OnDemandEnabled      *bool
	TopUpMethod          string        `gorm:"size:100;check:chk_billing_top_up_method,length(top_up_method) <= 100"`
	UsagePeriodType      string        `gorm:"size:100;check:chk_billing_usage_period_type,length(usage_period_type) <= 100"`
	UsagePeriodStart     string        `gorm:"size:64;check:chk_billing_usage_period_start,length(usage_period_start) <= 64"`
	UsagePeriodEnd       string        `gorm:"size:64;check:chk_billing_usage_period_end,length(usage_period_end) <= 64"`
	BillingPeriodStart   string        `gorm:"size:64;check:chk_billing_period_start,length(billing_period_start) <= 64"`
	BillingPeriodEnd     string        `gorm:"size:64;check:chk_billing_period_end,length(billing_period_end) <= 64"`
	HistoryJSON          string        `gorm:"type:text;not null;default:'[]';check:chk_billing_history_json_length,length(history_json) <= 1048576"`
	SyncedAt             time.Time     `gorm:"not null"`
	Account              *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (billingModel) TableName() string { return "account_billing_snapshots" }

type quotaRecoveryModel struct {
	AccountID       uint64 `gorm:"primaryKey"`
	Kind            string `gorm:"size:16;not null;check:chk_quota_recovery_kind,kind IN ('free','paid')"`
	Status          string `gorm:"size:32;not null;check:chk_quota_recovery_status,status IN ('exhausted','probing')"`
	ConfirmedUsed   int64  `gorm:"not null;default:0;check:chk_quota_recovery_used,confirmed_used >= 0"`
	ConfirmedLimit  int64  `gorm:"not null;default:0;check:chk_quota_recovery_limit,confirmed_limit >= 0"`
	ExhaustedAt     *time.Time
	NextProbeAt     *time.Time
	LastConfirmedAt *time.Time
	UpdatedAt       time.Time     `gorm:"not null"`
	Account         *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (quotaRecoveryModel) TableName() string { return "account_quota_recovery" }

type modelRouteModel struct {
	ID            uint64    `gorm:"primaryKey;autoIncrement"`
	PublicID      string    `gorm:"size:255;uniqueIndex;not null;check:chk_model_routes_public_id,length(trim(public_id)) BETWEEN 1 AND 255"`
	Provider      string    `gorm:"size:32;uniqueIndex:uidx_provider_upstream;not null;check:chk_model_routes_provider,provider IN ('grok_build','grok_web','grok_console')"`
	UpstreamModel string    `gorm:"size:255;uniqueIndex:uidx_provider_upstream;not null;check:chk_model_routes_upstream_model,length(trim(upstream_model)) BETWEEN 1 AND 255"`
	Capability    string    `gorm:"size:32;not null;check:chk_model_routes_capability,capability IN ('responses','chat','image','image_edit','video')"`
	Origin        string    `gorm:"size:32;not null;default:discovered;check:chk_model_routes_origin,origin IN ('catalog','discovered','manual')"`
	Enabled       bool      `gorm:"not null"`
	CreatedAt     time.Time `gorm:"not null"`
	UpdatedAt     time.Time `gorm:"not null"`
}

func (modelRouteModel) TableName() string { return "model_routes" }

// modelRouteAliasModel 保留升级或人工重命名前的公开模型 ID，使外部客户端可以平滑迁移。
type modelRouteAliasModel struct {
	Alias        string           `gorm:"size:255;primaryKey;check:chk_model_route_aliases_alias,length(trim(alias)) BETWEEN 1 AND 255"`
	ModelRouteID uint64           `gorm:"not null;index"`
	CreatedAt    time.Time        `gorm:"not null"`
	ModelRoute   *modelRouteModel `gorm:"foreignKey:ModelRouteID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (modelRouteAliasModel) TableName() string { return "model_route_aliases" }

type modelRouteAccountModel struct {
	ModelRouteID uint64           `gorm:"primaryKey"`
	AccountID    uint64           `gorm:"primaryKey"`
	ModelRoute   *modelRouteModel `gorm:"foreignKey:ModelRouteID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
	Account      *accountModel    `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (modelRouteAccountModel) TableName() string { return "model_route_accounts" }

type accountModelCapabilityModel struct {
	AccountID     uint64        `gorm:"primaryKey"`
	UpstreamModel string        `gorm:"size:255;primaryKey;not null;check:chk_account_model_capabilities_model,length(trim(upstream_model)) BETWEEN 1 AND 255"`
	Account       *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (accountModelCapabilityModel) TableName() string { return "account_model_capabilities" }

type accountModelSyncStateModel struct {
	AccountID     uint64    `gorm:"primaryKey"`
	LastAttemptAt time.Time `gorm:"not null"`
	LastSuccessAt *time.Time
	LastError     string        `gorm:"size:512;check:chk_account_model_sync_states_error,length(last_error) <= 512"`
	Account       *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (accountModelSyncStateModel) TableName() string { return "account_model_sync_states" }

type accountModelQuotaBlockModel struct {
	AccountID     uint64        `gorm:"primaryKey"`
	UpstreamModel string        `gorm:"size:255;primaryKey;not null;check:chk_account_model_quota_blocks_model,length(trim(upstream_model)) BETWEEN 1 AND 255"`
	Reason        string        `gorm:"size:100;not null;check:chk_account_model_quota_blocks_reason,length(trim(reason)) BETWEEN 1 AND 100"`
	CooldownUntil time.Time     `gorm:"not null"`
	UpdatedAt     time.Time     `gorm:"not null"`
	Account       *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (accountModelQuotaBlockModel) TableName() string { return "account_model_quota_blocks" }

type clientKeyModel struct {
	ID                    uint64 `gorm:"primaryKey;autoIncrement"`
	Name                  string `gorm:"size:160;not null;check:chk_client_keys_name,length(trim(name)) BETWEEN 1 AND 160"`
	Prefix                string `gorm:"size:32;uniqueIndex;not null;check:chk_client_keys_prefix,length(prefix) BETWEEN 1 AND 32"`
	SecretHash            string `gorm:"size:64;not null;check:chk_client_keys_secret_hash,length(secret_hash) = 64"`
	EncryptedSecret       string `gorm:"type:text;not null;check:chk_client_keys_encrypted_secret,length(trim(encrypted_secret)) BETWEEN 1 AND 4096"`
	Enabled               bool   `gorm:"not null"`
	ExpiresAt             *time.Time
	RPMLimit              int   `gorm:"not null;default:120;check:chk_client_keys_rpm,rpm_limit BETWEEN 0 AND 100000"`
	MaxConcurrent         int   `gorm:"not null;default:8;check:chk_client_keys_max_concurrent,max_concurrent BETWEEN 0 AND 1024"`
	BillingLimitUSDTicks  int64 `gorm:"not null;default:0;check:chk_client_keys_billing_limit,billing_limit_usd_ticks BETWEEN 0 AND 9000000000000000"`
	BilledUsageUSDTicks   int64 `gorm:"not null;default:0;check:chk_client_keys_billed_usage,billed_usage_usd_ticks >= 0"`
	ReservedUsageUSDTicks int64 `gorm:"not null;default:0;check:chk_client_keys_reserved_usage,reserved_usage_usd_ticks >= 0"`
	LastUsedAt            *time.Time
	CreatedAt             time.Time `gorm:"not null"`
	UpdatedAt             time.Time `gorm:"not null"`
}

func (clientKeyModel) TableName() string { return "client_keys" }

type clientKeyModelPermission struct {
	ClientKeyID  uint64           `gorm:"primaryKey"`
	ModelRouteID uint64           `gorm:"primaryKey"`
	ClientKey    *clientKeyModel  `gorm:"foreignKey:ClientKeyID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
	ModelRoute   *modelRouteModel `gorm:"foreignKey:ModelRouteID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (clientKeyModelPermission) TableName() string { return "client_key_models" }

type billingReservationModel struct {
	EventID     string          `gorm:"size:64;primaryKey;check:chk_billing_reservations_event_id,length(event_id) BETWEEN 16 AND 64"`
	ClientKeyID uint64          `gorm:"not null;check:chk_billing_reservations_client_key_id,client_key_id > 0"`
	Amount      int64           `gorm:"not null;check:chk_billing_reservations_amount,amount > 0"`
	ExpiresAt   time.Time       `gorm:"not null"`
	CreatedAt   time.Time       `gorm:"not null"`
	ClientKey   *clientKeyModel `gorm:"foreignKey:ClientKeyID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (billingReservationModel) TableName() string { return "billing_reservations" }

type requestAuditModel struct {
	ID                      uint64    `gorm:"primaryKey;autoIncrement"`
	EventID                 string    `gorm:"size:64;check:chk_request_audits_event_id,event_id = '' OR length(event_id) BETWEEN 16 AND 64"`
	RequestID               string    `gorm:"size:64;not null;check:chk_request_audits_request_id,length(request_id) BETWEEN 1 AND 64"`
	ClientKeyID             uint64    `gorm:"not null;check:chk_request_audits_client_key_id,client_key_id > 0"`
	ClientKeyName           string    `gorm:"size:160;check:chk_request_audits_client_key_name,length(client_key_name) <= 160"`
	ModelRouteID            uint64    `gorm:"not null;check:chk_request_audits_model_route_id,model_route_id > 0"`
	ModelPublicID           string    `gorm:"size:255;check:chk_request_audits_model_public_id,length(model_public_id) <= 255"`
	ModelUpstreamModel      string    `gorm:"size:255;check:chk_request_audits_model_upstream_model,length(model_upstream_model) <= 255"`
	Provider                string    `gorm:"size:32;not null;check:chk_request_audits_provider,provider IN ('grok_build','grok_web','grok_console')"`
	Operation               string    `gorm:"size:32;not null;check:chk_request_audits_operation,operation IN ('responses','compaction','chat','messages','image','image_edit','video')"`
	UsageSource             string    `gorm:"size:16;not null;check:chk_request_audits_usage_source,usage_source IN ('upstream','estimated','none')"`
	AccountID               *uint64   `gorm:"check:chk_request_audits_account_id,account_id IS NULL OR account_id > 0"`
	AccountName             string    `gorm:"size:160;check:chk_request_audits_account_name,length(account_name) <= 160"`
	EgressNodeID            *uint64   `gorm:"check:chk_request_audits_egress_node_id,egress_node_id IS NULL OR egress_node_id > 0"`
	EgressNodeName          string    `gorm:"size:160;not null;default:'';check:chk_request_audits_egress_node_name,length(egress_node_name) <= 160"`
	EgressScope             string    `gorm:"size:32;not null;default:'';check:chk_request_audits_egress_scope,egress_scope IN ('','grok_build','grok_web','grok_console','grok_web_asset')"`
	EgressMode              string    `gorm:"size:16;not null;default:'';check:chk_request_audits_egress_mode,egress_mode IN ('','direct','proxy')"`
	StatusCode              int       `gorm:"not null;check:chk_request_audits_status_code,status_code BETWEEN 100 AND 599"`
	Streaming               bool      `gorm:"not null;default:false"`
	MediaInputImages        int64     `gorm:"not null;default:0"`
	MediaOutputImages       int64     `gorm:"not null;default:0"`
	MediaOutputSeconds      int64     `gorm:"not null;default:0"`
	InputTokens             int64     `gorm:"not null;default:0;check:chk_request_audits_metrics,media_input_images >= 0 AND media_output_images >= 0 AND media_output_seconds >= 0 AND input_tokens >= 0 AND cached_input_tokens >= 0 AND output_tokens >= 0 AND reasoning_tokens >= 0 AND total_tokens >= 0 AND cost_in_usd_ticks >= 0 AND estimated_cost_in_usd_ticks >= 0 AND num_sources_used >= 0 AND num_server_side_tools_used >= 0 AND context_input_tokens >= 0 AND context_output_tokens >= 0 AND duration_ms >= 0"`
	CachedInputTokens       int64     `gorm:"not null;default:0"`
	OutputTokens            int64     `gorm:"not null;default:0"`
	ReasoningTokens         int64     `gorm:"not null;default:0"`
	TotalTokens             int64     `gorm:"not null;default:0"`
	CostInUSDTicks          int64     `gorm:"not null;default:0"`
	EstimatedCostInUSDTicks int64     `gorm:"not null;default:0"`
	PricingModel            string    `gorm:"size:100;check:chk_request_audits_pricing_model,length(pricing_model) <= 100"`
	PricingVersion          string    `gorm:"size:20;check:chk_request_audits_pricing_version,length(pricing_version) <= 20"`
	NumSourcesUsed          int64     `gorm:"not null;default:0"`
	NumServerSideToolsUsed  int64     `gorm:"not null;default:0"`
	ContextInputTokens      int64     `gorm:"not null;default:0"`
	ContextOutputTokens     int64     `gorm:"not null;default:0"`
	DurationMS              int64     `gorm:"not null;default:0"`
	ErrorCode               string    `gorm:"size:100;check:chk_request_audits_error_code,length(error_code) <= 100"`
	AttemptCount            int       `gorm:"not null;default:0;check:chk_request_audits_attempt_count,attempt_count >= 0"`
	CreatedAt               time.Time `gorm:"not null"`
}

func (requestAuditModel) TableName() string { return "request_audits" }

type requestAuditAttemptModel struct {
	ID                    uint64             `gorm:"primaryKey;autoIncrement"`
	AuditID               uint64             `gorm:"not null;uniqueIndex:uidx_audit_attempt_number;check:chk_request_audit_attempts_audit_id,audit_id > 0"`
	Number                int                `gorm:"not null;uniqueIndex:uidx_audit_attempt_number;check:chk_request_audit_attempts_number,number > 0"`
	Source                string             `gorm:"size:32;not null;check:chk_request_audit_attempts_source,source IN ('upstream_http','gateway_transport','credential')"`
	Stage                 string             `gorm:"size:64;not null;check:chk_request_audit_attempts_stage,length(trim(stage)) BETWEEN 1 AND 64"`
	AccountID             *uint64            `gorm:"check:chk_request_audit_attempts_account_id,account_id IS NULL OR account_id > 0"`
	AccountName           string             `gorm:"size:160;not null;default:'';check:chk_request_audit_attempts_account_name,length(account_name) <= 160"`
	Method                string             `gorm:"size:16;not null;default:'';check:chk_request_audit_attempts_method,length(method) <= 16"`
	RequestPath           string             `gorm:"type:text;not null;default:'';check:chk_request_audit_attempts_request_path,length(request_path) <= 2048"`
	UpstreamURL           string             `gorm:"type:text;not null;default:'';check:chk_request_audit_attempts_upstream_url,length(upstream_url) <= 4096"`
	StartedAt             time.Time          `gorm:"not null"`
	DurationMS            int64              `gorm:"not null;default:0;check:chk_request_audit_attempts_duration,duration_ms >= 0"`
	UpstreamStatusCode    *int               `gorm:"check:chk_request_audit_attempts_status,upstream_status_code IS NULL OR upstream_status_code BETWEEN 100 AND 599"`
	UpstreamStatus        string             `gorm:"size:128;not null;default:'';check:chk_request_audit_attempts_status_text,length(upstream_status) <= 128"`
	ResponseHeadersJSON   string             `gorm:"type:text;not null;default:'{}';check:chk_request_audit_attempts_headers,length(response_headers_json) <= 32768"`
	ResponseBody          []byte             `gorm:"check:chk_request_audit_attempts_body,length(response_body) <= 65536"`
	ResponseBodyTruncated bool               `gorm:"not null;default:false"`
	TransportError        string             `gorm:"type:text;not null;default:'';check:chk_request_audit_attempts_transport_error,length(transport_error) <= 2048"`
	ErrorChainJSON        string             `gorm:"type:text;not null;default:'[]';check:chk_request_audit_attempts_error_chain,length(error_chain_json) <= 32768"`
	Audit                 *requestAuditModel `gorm:"foreignKey:AuditID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (requestAuditAttemptModel) TableName() string { return "request_audit_attempts" }

type responseOwnershipModel struct {
	ResponseID         string          `gorm:"size:255;primaryKey;check:chk_response_ownership_id,length(response_id) BETWEEN 1 AND 255"`
	AccountID          uint64          `gorm:"not null"`
	ClientKeyID        uint64          `gorm:"not null"`
	Provider           string          `gorm:"size:32;not null;check:chk_response_ownership_provider,provider IN ('grok_build','grok_web','grok_console')"`
	PromptCacheKey     string          `gorm:"size:64;not null;default:'';check:chk_response_ownership_cache_key,length(prompt_cache_key) <= 64"`
	ReasoningReplayKey string          `gorm:"size:64;not null;default:'';check:chk_response_ownership_replay_key,length(reasoning_replay_key) <= 64"`
	ExpiresAt          time.Time       `gorm:"not null;check:chk_response_ownership_expiry,expires_at > created_at"`
	CreatedAt          time.Time       `gorm:"not null"`
	UpdatedAt          time.Time       `gorm:"not null"`
	Account            *accountModel   `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
	ClientKey          *clientKeyModel `gorm:"foreignKey:ClientKeyID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (responseOwnershipModel) TableName() string { return "response_ownership" }

type webResponseStateModel struct {
	ResponseID               string        `gorm:"size:255;primaryKey;check:chk_web_response_states_id,length(response_id) BETWEEN 1 AND 255"`
	AccountID                uint64        `gorm:"not null;check:chk_web_response_states_account_id,account_id > 0"`
	ConversationID           string        `gorm:"size:255;not null;check:chk_web_response_states_conversation,length(trim(conversation_id)) BETWEEN 1 AND 255"`
	UpstreamParentResponseID string        `gorm:"size:255;not null;check:chk_web_response_states_parent,length(trim(upstream_parent_response_id)) BETWEEN 1 AND 255"`
	ResponseJSON             string        `gorm:"type:text;not null;check:chk_web_response_states_json,length(response_json) <= 16777216"`
	Status                   string        `gorm:"size:32;not null;check:chk_web_response_states_status,status IN ('in_progress','completed','failed','cancelled')"`
	ExpiresAt                time.Time     `gorm:"not null;check:chk_web_response_states_expiry,expires_at > created_at"`
	CreatedAt                time.Time     `gorm:"not null"`
	UpdatedAt                time.Time     `gorm:"not null"`
	Account                  *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (webResponseStateModel) TableName() string { return "web_response_states" }

type mediaJobModel struct {
	ID              string  `gorm:"size:64;primaryKey;check:chk_media_jobs_id,length(id) BETWEEN 1 AND 64"`
	RequestID       string  `gorm:"size:64;not null;check:chk_media_jobs_request_id,length(request_id) BETWEEN 1 AND 64"`
	ClientKeyID     uint64  `gorm:"not null;check:chk_media_jobs_client_key_id,client_key_id > 0"`
	ClientKeyName   string  `gorm:"size:160;not null;default:'';check:chk_media_jobs_client_key_name,length(client_key_name) <= 160"`
	AccountID       *uint64 `gorm:"check:chk_media_jobs_account_id,account_id IS NULL OR account_id > 0"`
	AccountName     string  `gorm:"size:160;not null;default:'';check:chk_media_jobs_account_name,length(account_name) <= 160"`
	EgressNodeID    *uint64 `gorm:"check:chk_media_jobs_egress_node_id,egress_node_id IS NULL OR egress_node_id > 0"`
	EgressNodeName  string  `gorm:"size:160;not null;default:'';check:chk_media_jobs_egress_node_name,length(egress_node_name) <= 160"`
	EgressScope     string  `gorm:"size:32;not null;default:'';check:chk_media_jobs_egress_scope,egress_scope IN ('','grok_web','grok_build')"`
	EgressMode      string  `gorm:"size:16;not null;default:'';check:chk_media_jobs_egress_mode,egress_mode IN ('','direct','proxy')"`
	Provider        string  `gorm:"size:32;not null;check:chk_media_jobs_provider,provider IN ('grok_web','grok_build')"`
	Model           string  `gorm:"size:255;not null;check:chk_media_jobs_model,length(trim(model)) BETWEEN 1 AND 255"`
	ModelRouteID    uint64  `gorm:"not null;check:chk_media_jobs_model_route_id,model_route_id > 0"`
	UpstreamModel   string  `gorm:"size:255;not null;check:chk_media_jobs_upstream_model,length(trim(upstream_model)) BETWEEN 1 AND 255"`
	Prompt          string  `gorm:"type:text;not null;check:chk_media_jobs_prompt,length(prompt) BETWEEN 0 AND 100000"`
	Seconds         int     `gorm:"not null;check:chk_media_jobs_seconds,seconds BETWEEN 1 AND 15"`
	Size            string  `gorm:"size:32;not null;check:chk_media_jobs_size,length(trim(size)) BETWEEN 1 AND 32"`
	Quality         string  `gorm:"size:32;not null;check:chk_media_jobs_quality,length(trim(quality)) BETWEEN 1 AND 32"`
	Status          string  `gorm:"size:32;not null;check:chk_media_jobs_status,status IN ('queued','in_progress','completed','failed')"`
	Progress        int     `gorm:"not null;check:chk_media_jobs_progress,progress BETWEEN 0 AND 100"`
	InputJSON       string  `gorm:"type:text;not null;default:'{}';check:chk_media_jobs_input_json,length(input_json) <= 1048576"`
	UpstreamURL     string  `gorm:"type:text;not null;default:'';check:chk_media_jobs_upstream_url,length(upstream_url) <= 8192"`
	ResultAssetID   string  `gorm:"size:64;not null;default:'';check:chk_media_jobs_result_asset_id,result_asset_id = '' OR length(trim(result_asset_id)) BETWEEN 16 AND 64"`
	ContentType     string  `gorm:"size:128;not null;default:'';check:chk_media_jobs_content_type,length(content_type) <= 128"`
	ErrorCode       string  `gorm:"size:100;not null;default:'';check:chk_media_jobs_error_code,length(error_code) <= 100"`
	ErrorMessage    string  `gorm:"size:512;not null;default:'';check:chk_media_jobs_error_message,length(error_message) <= 512"`
	LeaseUntil      *time.Time
	ClaimToken      string    `gorm:"size:64;not null;default:'';check:chk_media_jobs_claim_token,claim_token = '' OR length(claim_token) BETWEEN 16 AND 64"`
	CreatedAt       time.Time `gorm:"not null"`
	UpdatedAt       time.Time `gorm:"not null"`
	CompletedAt     *time.Time
	UsageRecordedAt *time.Time
	Account         *accountModel   `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL"`
	ClientKey       *clientKeyModel `gorm:"foreignKey:ClientKeyID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`
}

func (mediaJobModel) TableName() string { return "media_jobs" }

// MaxVideoAssetBytes 是本地视频对象与上传接收的安全体积上限（256 MiB）。
const MaxVideoAssetBytes = 256 << 20

type mediaAssetModel struct {
	ID         string    `gorm:"size:64;primaryKey;check:chk_media_assets_id,length(trim(id)) BETWEEN 16 AND 64"`
	Kind       string    `gorm:"size:16;not null;check:chk_media_assets_kind,kind IN ('image','video')"`
	StorageKey string    `gorm:"size:512;not null;uniqueIndex;check:chk_media_assets_storage_key,length(trim(storage_key)) BETWEEN 1 AND 512"`
	MIMEType   string    `gorm:"size:64;not null;check:chk_media_assets_mime,mime_type IN ('image/jpeg','image/png','image/webp','image/gif','video/mp4','video/webm','video/quicktime')"`
	SizeBytes  int64     `gorm:"not null;check:chk_media_assets_size,size_bytes > 0 AND size_bytes <= 268435456"`
	SHA256     string    `gorm:"size:64;not null;check:chk_media_assets_sha,length(sha256) = 64"`
	CreatedAt  time.Time `gorm:"not null"`
}

func (mediaAssetModel) TableName() string { return "media_assets" }

// mediaUploadTicketModel 保存上游视频 PUT 票据的不可逆摘要与一次性消费状态。
// 明文 token 永不落库；校验时对路径 token 再哈希比对。
type mediaUploadTicketModel struct {
	TokenHash   string    `gorm:"size:64;primaryKey;check:chk_media_upload_tickets_token_hash,length(token_hash) = 64"`
	AssetID     string    `gorm:"size:64;not null;uniqueIndex;check:chk_media_upload_tickets_asset_id,length(trim(asset_id)) BETWEEN 16 AND 64"`
	JobID       string    `gorm:"size:64;not null;index;check:chk_media_upload_tickets_job_id,length(trim(job_id)) BETWEEN 1 AND 64"`
	MaxBytes    int64     `gorm:"not null;check:chk_media_upload_tickets_max_bytes,max_bytes > 0 AND max_bytes <= 268435456"`
	AllowedMIME string    `gorm:"size:128;not null;default:'video/mp4';check:chk_media_upload_tickets_mime,length(trim(allowed_mime)) BETWEEN 1 AND 128"`
	ExpiresAt   time.Time `gorm:"not null"`
	ConsumedAt  *time.Time
	CreatedAt   time.Time `gorm:"not null"`
}

func (mediaUploadTicketModel) TableName() string { return "media_upload_tickets" }

type runtimeSettingsModel struct {
	Key       string    `gorm:"size:64;primaryKey;check:chk_runtime_settings_key,length(trim(key)) BETWEEN 1 AND 64"`
	ValueJSON string    `gorm:"type:text;not null;check:chk_runtime_settings_json_length,length(value_json) <= 1048576"`
	Revision  uint64    `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

func (runtimeSettingsModel) TableName() string { return "runtime_settings" }

type egressNodeModel struct {
	ID                          uint64 `gorm:"primaryKey;autoIncrement"`
	Name                        string `gorm:"size:160;not null;check:chk_egress_nodes_name,length(trim(name)) BETWEEN 1 AND 160"`
	Scope                       string `gorm:"size:32;not null;check:chk_egress_nodes_specific_scope,scope IN ('grok_build','grok_web','grok_console','grok_web_asset')"`
	Enabled                     bool   `gorm:"not null;default:true"`
	EncryptedProxyURL           string `gorm:"type:text;not null;default:'';check:chk_egress_nodes_proxy_url,length(encrypted_proxy_url) <= 65536"`
	UserAgent                   string `gorm:"size:512;not null;default:'';check:chk_egress_nodes_user_agent,length(user_agent) <= 512"`
	EncryptedCloudflareCookie   string `gorm:"type:text;not null;default:'';check:chk_egress_nodes_cf_cookie,length(encrypted_cloudflare_cookie) <= 65536"`
	ClearanceRefreshedAt        *time.Time
	ClearanceFingerprint        string  `gorm:"size:64;not null;default:'';check:chk_egress_nodes_clearance_fingerprint,length(clearance_fingerprint) IN (0, 64)"`
	ClearanceBindingFingerprint string  `gorm:"size:64;not null;default:'';check:chk_egress_nodes_clearance_binding_fingerprint,length(clearance_binding_fingerprint) IN (0, 64)"`
	Health                      float64 `gorm:"not null;default:1;check:chk_egress_nodes_health,health >= 0 AND health <= 1"`
	FailureCount                int     `gorm:"not null;default:0;check:chk_egress_nodes_failures,failure_count >= 0"`
	CooldownUntil               *time.Time
	LastError                   string    `gorm:"size:512;check:chk_egress_nodes_last_error,length(last_error) <= 512"`
	CreatedAt                   time.Time `gorm:"not null"`
	UpdatedAt                   time.Time `gorm:"not null"`
}

func (egressNodeModel) TableName() string { return "egress_nodes" }
