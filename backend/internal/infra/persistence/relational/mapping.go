package relational

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
)

func toAdminDomain(value adminModel) admin.Admin {
	return admin.Admin{ID: value.ID, Username: value.Username, PasswordHash: value.PasswordHash, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}

func toSessionDomain(value adminSessionModel) admin.Session {
	return admin.Session{ID: value.ID, AdminID: value.AdminID, RefreshTokenHash: value.RefreshTokenHash, ExpiresAt: value.ExpiresAt, LastUsedAt: value.LastUsedAt, CreatedAt: value.CreatedAt}
}

func toAccountDomain(value accountModel) account.Credential {
	var expiresAt time.Time
	var refreshDueAt, lastRefreshAt *time.Time
	var refreshFailures int
	var lastRefreshError string
	var authType account.AuthType
	var clientID, encryptedPrimary, encryptedRefresh string
	if value.Credential != nil {
		authType = account.AuthType(value.Credential.AuthType)
		clientID = value.Credential.ClientID
		encryptedPrimary = value.Credential.EncryptedPrimary
		encryptedRefresh = value.Credential.EncryptedRefresh
		if value.Credential.ExpiresAt != nil {
			expiresAt = *value.Credential.ExpiresAt
		}
		refreshDueAt = value.Credential.RefreshDueAt
		lastRefreshAt = value.Credential.LastRefreshAt
		refreshFailures = value.Credential.RefreshFailures
		lastRefreshError = value.Credential.LastRefreshError
	}
	var webTier account.WebTier
	var webTierSyncedAt *time.Time
	if value.WebProfile != nil {
		webTier = account.WebTier(value.WebProfile.Tier)
		webTierSyncedAt = value.WebProfile.SyncedAt
	}
	return account.Credential{
		ID: value.ID, Provider: account.Provider(value.Provider), AuthType: authType, Name: value.Name, Email: value.Email,
		UserID: value.UserID, TeamID: value.TeamID, SourceKey: value.SourceKey, OIDCClientID: clientID,
		EncryptedAccessToken: encryptedPrimary, EncryptedRefreshToken: encryptedRefresh,
		ExpiresAt: expiresAt, RefreshDueAt: refreshDueAt, LastRefreshAt: lastRefreshAt,
		RefreshFailureCount: refreshFailures, LastRefreshErrorCode: lastRefreshError,
		Enabled: value.Enabled, AuthStatus: account.AuthStatus(value.AuthStatus), Priority: value.Priority,
		MaxConcurrent: value.MaxConcurrent, MinimumRemaining: value.MinimumRemaining, FailureCount: value.FailureCount,
		CooldownUntil: value.CooldownUntil, LastError: value.LastError, LastUsedAt: value.LastUsedAt,
		ObservedModel: value.ObservedModel, ObservedModelAt: value.ObservedModelAt, WebTier: webTier, WebTierSyncedAt: webTierSyncedAt,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func fromAccountDomain(value account.Credential) accountModel {
	return accountModel{
		ID: value.ID, IdentityKey: accountIdentity(value), Provider: string(value.Provider), Name: value.Name, Email: value.Email,
		UserID: value.UserID, TeamID: value.TeamID, SourceKey: value.SourceKey,
		Enabled: value.Enabled, AuthStatus: string(value.AuthStatus), Priority: value.Priority,
		MaxConcurrent: value.MaxConcurrent, MinimumRemaining: value.MinimumRemaining, FailureCount: value.FailureCount,
		CooldownUntil: value.CooldownUntil, LastError: value.LastError, LastUsedAt: value.LastUsedAt,
		ObservedModel: value.ObservedModel, ObservedModelAt: value.ObservedModelAt,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func fromAccountCredentialDomain(value account.Credential) accountCredentialModel {
	var expiresAt *time.Time
	if !value.ExpiresAt.IsZero() {
		copy := value.ExpiresAt
		expiresAt = &copy
	}
	refreshDueAt := value.RefreshDueAt
	if refreshDueAt == nil && value.EncryptedRefreshToken != "" && !value.ExpiresAt.IsZero() {
		due := account.CredentialRefreshDueAt(value.ID, value.ExpiresAt)
		refreshDueAt = &due
	}
	authType := value.AuthType
	if authType == "" {
		if value.Provider == account.ProviderWeb || value.Provider == account.ProviderConsole {
			authType = account.AuthTypeSSO
		} else {
			authType = account.AuthTypeOAuth
		}
	}
	return accountCredentialModel{
		AccountID: value.ID, AuthType: string(authType), ClientID: value.OIDCClientID,
		EncryptedPrimary: value.EncryptedAccessToken, EncryptedRefresh: value.EncryptedRefreshToken,
		ExpiresAt: expiresAt, RefreshDueAt: refreshDueAt, LastRefreshAt: value.LastRefreshAt,
		RefreshFailures: value.RefreshFailureCount, LastRefreshError: value.LastRefreshErrorCode,
		UpdatedAt: time.Now().UTC(),
	}
}

func fromWebProfileDomain(value account.Credential) *webAccountProfileModel {
	if value.Provider != account.ProviderWeb {
		return nil
	}
	tier := value.WebTier
	if tier == "" {
		tier = account.WebTierAuto
	}
	return &webAccountProfileModel{AccountID: value.ID, Tier: string(tier), SyncedAt: value.WebTierSyncedAt}
}

func accountIdentity(value account.Credential) string {
	provider := string(value.Provider)
	var identity string
	switch {
	case strings.TrimSpace(value.UserID) != "":
		identity = strings.Join([]string{provider, "user", strings.TrimSpace(value.UserID), strings.TrimSpace(value.TeamID)}, "|")
	case strings.TrimSpace(value.Email) != "":
		identity = strings.Join([]string{provider, "email", strings.ToLower(strings.TrimSpace(value.Email)), strings.TrimSpace(value.TeamID)}, "|")
	default:
		identity = strings.Join([]string{provider, "source", strings.TrimSpace(value.SourceKey)}, "|")
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func toBillingDomain(value billingModel) account.Billing {
	var history []account.BillingHistoryEntry
	_ = json.Unmarshal([]byte(value.HistoryJSON), &history)
	return account.Billing{AccountID: value.AccountID, PlanCode: value.PlanCode, PlanName: value.PlanName, MonthlyLimit: value.MonthlyLimit, Used: value.Used, OnDemandCap: value.OnDemandCap, OnDemandUsed: value.OnDemandUsed, PrepaidBalance: value.PrepaidBalance, CreditUsagePercent: value.CreditUsagePercent, IsUnifiedBillingUser: value.IsUnifiedBillingUser, TopUpMethod: value.TopUpMethod, UsagePeriodType: value.UsagePeriodType, UsagePeriodStart: value.UsagePeriodStart, UsagePeriodEnd: value.UsagePeriodEnd, BillingPeriodStart: value.BillingPeriodStart, BillingPeriodEnd: value.BillingPeriodEnd, History: history, SyncedAt: value.SyncedAt}
}

func toModelDomain(value modelRouteModel) model.Route {
	return model.Route{ID: value.ID, PublicID: value.PublicID, Provider: account.Provider(value.Provider), UpstreamModel: value.UpstreamModel, Capability: model.Capability(value.Capability), Origin: model.Origin(value.Origin), Enabled: value.Enabled, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}

func toClientKeyDomain(value clientKeyModel, allowedModels []uint64) clientkey.Key {
	return clientkey.Key{ID: value.ID, Name: value.Name, Prefix: value.Prefix, SecretHash: value.SecretHash, EncryptedSecret: value.EncryptedSecret, Enabled: value.Enabled, ExpiresAt: value.ExpiresAt, RPMLimit: value.RPMLimit, MaxConcurrent: value.MaxConcurrent, BillingLimitUSDTicks: value.BillingLimitUSDTicks, BilledUsageUSDTicks: value.BilledUsageUSDTicks, ReservedUsageUSDTicks: value.ReservedUsageUSDTicks, AllowedModels: allowedModels, LastUsedAt: value.LastUsedAt, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}

func toAuditDomain(value requestAuditModel) audit.Record {
	return audit.Record{
		ID: value.ID, EventID: value.EventID, RequestID: value.RequestID, ClientKeyID: value.ClientKeyID, ClientKeyName: value.ClientKeyName,
		ModelRouteID: value.ModelRouteID, ModelPublicID: value.ModelPublicID, ModelUpstreamModel: value.ModelUpstreamModel,
		Provider: value.Provider, Operation: audit.Operation(value.Operation), UsageSource: audit.UsageSource(value.UsageSource),
		AccountID: value.AccountID, AccountName: value.AccountName, StatusCode: value.StatusCode, Streaming: value.Streaming,
		MediaInputImages: value.MediaInputImages, MediaOutputImages: value.MediaOutputImages, MediaOutputSeconds: value.MediaOutputSeconds,
		InputTokens: value.InputTokens, CachedInputTokens: value.CachedInputTokens, OutputTokens: value.OutputTokens,
		ReasoningTokens: value.ReasoningTokens, TotalTokens: value.TotalTokens, CostInUSDTicks: value.CostInUSDTicks,
		EstimatedCostInUSDTicks: value.EstimatedCostInUSDTicks, PricingModel: value.PricingModel, PricingVersion: value.PricingVersion,
		NumSourcesUsed: value.NumSourcesUsed, NumServerSideToolsUsed: value.NumServerSideToolsUsed,
		ContextInputTokens: value.ContextInputTokens, ContextOutputTokens: value.ContextOutputTokens, DurationMS: value.DurationMS,
		ErrorCode: value.ErrorCode, CreatedAt: value.CreatedAt,
	}
}
