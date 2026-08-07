package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// UpstreamFailure 保存可安全暴露给下游和审计的上游失败分类，不包含响应正文或凭据。
type UpstreamFailure struct {
	HTTPStatus             int
	Code                   string
	PublicMessage          string
	UpstreamCode           string
	AccountID              uint64
	AccountName            string
	AccountScoped          bool
	AccountBlocked         bool
	PermanentAccountDenial bool
	QuotaExhausted         bool
	FreeQuotaExhausted     bool
	ModelQuotaExhausted    bool
	// SpendingLimitBlocked 表示付费账号被 spending-limit 永久阻断（402 personal-team-blocked:spending-limit），
	// 账单周期内不会自动恢复，适合直接标 reauthRequired 出池。
	SpendingLimitBlocked bool
	// SafetyRejection marks a request-level content safety denial. It must not
	// refresh OAuth, retry, switch accounts, cool down, or invalidate credentials.
	SafetyRejection bool
	// RequestScopedForbidden marks a deterministic request/policy rejection such
	// as invalid arguments, content policy, or a ZDR-gated operation. Retrying the
	// same request with another credential cannot fix it.
	RequestScopedForbidden bool
	CredentialRejected     bool
	Fingerprint            string
	RetryAfter             time.Duration
	Cause                  error
}

func (e *UpstreamFailure) Error() string {
	if e == nil {
		return "上游请求失败"
	}
	if e.UpstreamCode != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.UpstreamCode)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Cause)
	}
	return e.Code
}

func (e *UpstreamFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *UpstreamFailure) AuditCode() string {
	if e == nil {
		return "upstream_error"
	}
	if suffix := normalizeFailureCode(e.UpstreamCode); suffix != "" {
		return truncateFailureCode(e.Code + "_" + suffix)
	}
	return truncateFailureCode(e.Code)
}

// ClientCredentialErrorCode 返回允许暴露给客户端的账号类上游错误码。
// HTTP 状态和错误文案仍由传输层统一脱敏；这里只放行稳定、无凭据内容的机器码。
func (e *UpstreamFailure) ClientCredentialErrorCode() string {
	if e == nil {
		return "upstream_unavailable"
	}
	return clientCredentialErrorCode(e.HTTPStatus, e.UpstreamCode)
}

// ClientCredentialErrorCodeFromBody 从账号类上游错误正文中提取允许公开的机器码。
// 用于上游响应已直接交给传输层、尚未构造 UpstreamFailure 的路径。
func ClientCredentialErrorCodeFromBody(status int, body []byte) string {
	upstreamCode, _, _ := extractUpstreamErrorMetadata(body)
	return clientCredentialErrorCode(status, upstreamCode)
}

func clientCredentialErrorCode(status int, upstreamCode string) string {
	if status == http.StatusForbidden && normalizeFailureCode(upstreamCode) == "permission_denied" {
		return "permission-denied"
	}
	return "upstream_unavailable"
}

func newHTTPUpstreamFailure(status int, body []byte, accountID uint64, accountName string) *UpstreamFailure {
	upstreamCode, upstreamType, upstreamMessage := extractUpstreamErrorMetadata(body)
	failure := &UpstreamFailure{
		HTTPStatus: status, Code: "upstream_error", PublicMessage: "上游服务返回错误",
		UpstreamCode: upstreamCode, AccountID: accountID, AccountName: accountName,
	}
	if status < 400 || status > 599 {
		failure.HTTPStatus = http.StatusBadGateway
	}
	metadataText := strings.ToLower(strings.Join([]string{upstreamCode, upstreamType, upstreamMessage}, " "))
	switch status {
	case http.StatusUnauthorized:
		failure.Code = "upstream_unauthorized"
		failure.PublicMessage = "上游账号认证失败"
		failure.AccountScoped = true
		failure.CredentialRejected = true
		failure.AccountBlocked = isDefinitiveAccountBlock(metadataText)
	case http.StatusPaymentRequired:
		failure.Code = "upstream_payment_required"
		failure.PublicMessage = "上游账号额度不足"
		failure.AccountScoped = true
		failure.QuotaExhausted = true
		// spending-limit is account-scoped, but its paid/free recovery kind depends on
		// the selected account's billing snapshot and must be decided by the selector.
		failure.FreeQuotaExhausted = isFreeQuotaExhaustion(metadataText)
		failure.SpendingLimitBlocked = isPaidQuotaExhaustion(metadataText)
	case http.StatusForbidden:
		failure.Code = "upstream_forbidden"
		failure.PublicMessage = "上游拒绝了该请求"
		// Console's DPoP requirement is an upstream auth-scheme rollout, not a
		// property of the selected SSO account. Rotating accounts or browser
		// egress cannot make the same Bearer-anonymous request valid.
		if isDPoPProofRequired(upstreamCode) {
			failure.RequestScopedForbidden = true
			break
		}
		// Safety denials are request-scoped: inspect both structured metadata and the raw body
		// so SAFETY_CHECK_TYPE_* markers still match when they only appear in nested text.
		if isSafetyRejection(metadataText) || isSafetyRejection(string(body)) {
			failure.SafetyRejection = true
			break
		}
		if isRequestScopedForbidden(upstreamCode, metadataText) {
			failure.RequestScopedForbidden = true
			break
		}
		failure.AccountBlocked = isDefinitiveAccountBlock(metadataText)
		// Some upstream envelopes expose the human-readable denial through a
		// nested error/message field that is only retained in the normalized
		// metadata text. Classify the complete metadata, not one extractor slot.
		failure.PermanentAccountDenial = isPermanentAccountDenial(strings.ToLower(upstreamMessage)) || strings.Contains(metadataText, "access to the chat endpoint is denied")
		failure.ModelQuotaExhausted = isModelQuotaExhaustion(metadataText)
		failure.FreeQuotaExhausted = failure.ModelQuotaExhausted || isFreeQuotaExhaustion(metadataText)
		failure.QuotaExhausted = failure.FreeQuotaExhausted || isCreditQuotaExhaustion(metadataText)
		failure.SpendingLimitBlocked = isPaidQuotaExhaustion(metadataText)
		failure.CredentialRejected = !failure.QuotaExhausted && containsAny(metadataText, "authentication", "unauthorized", "invalid token", "token expired")
		failure.AccountScoped = failure.AccountBlocked || failure.PermanentAccountDenial || failure.QuotaExhausted || failure.CredentialRejected || isAccountScopedForbidden(metadataText)
	case http.StatusTooManyRequests:
		failure.Code = "upstream_rate_limited"
		failure.PublicMessage = "上游请求频率受限"
		failure.AccountScoped = true
		// Subscription-level free usage and explicit per-model free usage are
		// distinct so the gateway can preserve their different recovery scopes.
		failure.FreeQuotaExhausted = isFreeQuotaExhaustion(metadataText)
		failure.ModelQuotaExhausted = isModelQuotaExhaustion(metadataText)
		failure.QuotaExhausted = failure.FreeQuotaExhausted || isPaidQuotaExhaustion(metadataText)
	default:
		failure.Code = "upstream_server_error"
		failure.PublicMessage = "上游服务暂时异常"
	}
	fingerprintPart := normalizeFailureCode(firstNonEmptyFailure(upstreamCode, upstreamType, upstreamMessage))
	if fingerprintPart == "" {
		fingerprintPart = "unknown"
	}
	failure.Fingerprint = fmt.Sprintf("%d:%s", status, fingerprintPart)
	return failure
}

func newTransportUpstreamFailure(err error, accountID uint64, accountName string) *UpstreamFailure {
	code, message := "upstream_network_error", "连接上游服务失败"
	status := http.StatusBadGateway
	if neterrorpkg.IsResponseHeaderTimeout(err) {
		status, code, message = http.StatusGatewayTimeout, "upstream_header_timeout", "等待上游响应头超时"
	} else if neterrorpkg.IsUpstreamStreamIdleTimeout(err) {
		status, code, message = http.StatusGatewayTimeout, "upstream_stream_idle_timeout", "上游流式响应长时间无数据"
	} else if errors.Is(err, context.DeadlineExceeded) {
		code, message = "upstream_timeout", "上游服务响应超时"
	}
	return &UpstreamFailure{
		HTTPStatus: status, Code: code, PublicMessage: message,
		AccountID: accountID, AccountName: accountName, Fingerprint: code, Cause: err,
	}
}

func newCredentialUpstreamFailure(err error, accountID uint64, accountName string) *UpstreamFailure {
	return &UpstreamFailure{
		HTTPStatus: http.StatusBadGateway, Code: "upstream_credential_unavailable", PublicMessage: "上游账号凭据不可用",
		AccountID: accountID, AccountName: accountName, AccountScoped: true, Cause: err,
	}
}

func extractUpstreamErrorMetadata(body []byte) (string, string, string) {
	return provider.ExtractUpstreamErrorMetadata(body)
}

func isAccountScopedForbidden(text string) bool {
	// Do not match bare "permission" / permission-denied alone: those codes are shared by
	// request-level policy denials. Account scope requires quota/billing/auth wording.
	return containsAny(text,
		"quota", "billing", "subscription", "entitlement",
		"unauthorized", "authentication", "invalid token", "token expired",
		"usage-exhausted", "insufficient", "spending-limit", "spending limit",
		"run out of credits", "out of credits", "usage balance exhausted", "usage limit reached",
	)
}

// isPermanentAccountDenial requires explicit account/model permission text.
// A bare permission-denied code without access-denied wording is not enough:
// content safety rejections and other policy 403s share that code.
func isPermanentAccountDenial(text string) bool {
	return provider.IsPermanentAccountDenial(text)
}

// isSafetyRejection identifies request-level content safety denials that must
// be returned to the client without account rotation or invalidation.
func isSafetyRejection(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, "content violates usage guidelines") ||
		strings.Contains(lower, "safety_check_type_")
}

// isRequestScopedForbidden recognizes only high-confidence request-level
// failures. Unknown 403s deliberately remain unclassified so the gateway can
// try the next credential without penalizing the current one.
func isRequestScopedForbidden(upstreamCode, text string) bool {
	switch normalizeFailureCode(upstreamCode) {
	case "invalid_argument", "invalid_arguments", "invalid_parameter", "invalid_parameters",
		"invalid_request", "bad_request", "invalid_params":
		return true
	}
	return containsAny(text,
		"request rejected by policy", "policy rejected request",
		"content policy", "content moderation",
		"zero data retention", "zdr-blocked", "zdr blocked", "zdr-gated", "zdr gated",
		"operation is unavailable under zdr", "operation unavailable under zdr",
	)
}

func isDPoPProofRequired(upstreamCode string) bool {
	return provider.IsDPoPProofRequiredText(upstreamCode)
}

func isDefinitiveAccountBlock(text string) bool {
	return provider.IsDefinitiveAccountBlockText(text)
}

func isCreditQuotaExhaustion(text string) bool {
	return isPaidQuotaExhaustion(text) || containsAny(text,
		"run out of credits", "out of credits", "usage balance exhausted", "usage limit reached",
	)
}

func isPaidQuotaExhaustion(text string) bool {
	return strings.Contains(text, "personal-team-blocked:spending-limit")
}

func isFreeQuotaExhaustion(text string) bool {
	return provider.ContainsAny(text, "subscription:free-usage-exhausted", "used all the included free usage for model")
}

func isModelQuotaExhaustion(text string) bool {
	return strings.Contains(text, "used all the included free usage for model")
}

func containsAny(text string, signals ...string) bool {
	return provider.ContainsAny(text, signals...)
}

func firstNonEmptyFailure(values ...string) string {
	return provider.FirstNonEmptyFailure(values...)
}

func normalizeFailureCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, current := range value {
		switch {
		case unicode.IsLetter(current), unicode.IsDigit(current):
			builder.WriteRune(current)
		case current == '-', current == '_', current == '.', current == ':':
			builder.WriteByte('_')
		}
		if builder.Len() >= 48 {
			break
		}
	}
	return strings.Trim(builder.String(), "_")
}

func truncateFailureCode(value string) string {
	if len(value) <= 100 {
		return value
	}
	return value[:100]
}
