package gateway

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

type responseHeaderTimeoutTestError struct{}

func (responseHeaderTimeoutTestError) Error() string {
	return "http2: timeout awaiting response headers"
}

func (responseHeaderTimeoutTestError) Timeout() bool   { return true }
func (responseHeaderTimeoutTestError) Temporary() bool { return true }

func TestTransportUpstreamFailureClassifiesProviderStreamIdleTimeout(t *testing.T) {
	failure := newTransportUpstreamFailure(neterror.ErrUpstreamStreamIdleTimeout, 42, "web")
	if failure.HTTPStatus != http.StatusGatewayTimeout || failure.Code != "upstream_stream_idle_timeout" || failure.AccountScoped {
		t.Fatalf("failure = %#v", failure)
	}
	if !isRetryableTransportFailure(accountdomain.ProviderWeb, neterror.ErrUpstreamStreamIdleTimeout) {
		t.Fatal("pre-response Web idle timeout should retain bounded cross-account failover")
	}
}

func TestTransportUpstreamFailureClassifiesResponseHeaderTimeout(t *testing.T) {
	failure := newTransportUpstreamFailure(responseHeaderTimeoutTestError{}, 42, "build")
	if failure.HTTPStatus != http.StatusGatewayTimeout || failure.Code != "upstream_header_timeout" || failure.PublicMessage != "等待上游响应头超时" || failure.AuditCode() != "upstream_header_timeout" {
		t.Fatalf("failure = %#v", failure)
	}
	if stage := transportStage(responseHeaderTimeoutTestError{}); stage != "response_header_timeout" {
		t.Fatalf("stage = %q", stage)
	}
	if isRetryableTransportFailure(accountdomain.ProviderBuild, responseHeaderTimeoutTestError{}) {
		t.Fatal("a Build response-header timeout must not switch accounts")
	}
	if !isRetryableTransportFailure(accountdomain.ProviderWeb, responseHeaderTimeoutTestError{}) {
		t.Fatal("the Build-specific retry veto must not change Web failover")
	}
	if !isRetryableTransportFailure(accountdomain.ProviderBuild, errors.New("connection reset by peer")) {
		t.Fatal("ordinary pre-response transport failures must retain failover behavior")
	}
}

func TestHTTPUpstreamFailureClassifiesBuildForbiddenBodies(t *testing.T) {
	tests := []struct {
		name                   string
		status                 int
		body                   string
		accountScoped          bool
		permanentAccountDenial bool
		safetyRejection        bool
		requestScopedForbidden bool
		quotaExhausted         bool
		freeQuotaExhausted     bool
		modelQuotaExhausted    bool
		accountBlocked         bool
		spendingLimitBlocked   bool
		upstreamCode           string
	}{
		{
			name: "blocked account", status: http.StatusForbidden, body: `{"code":"unauthorized:blocked-user","error":"User is blocked"}`,
			accountScoped: true, accountBlocked: true, upstreamCode: "unauthorized:blocked-user",
		},
		{
			name: "top-level permanent chat denial", status: http.StatusForbidden, body: `{"status_code":403,"code":"permission-denied","error":"Access to the chat endpoint is denied. Please update the permissions."}`,
			accountScoped: true, permanentAccountDenial: true, upstreamCode: "permission-denied",
		},
		{
			// Web chat nested error; numeric code alone must not decide AccountBlocked.
			name: "web nested blocked-user message", body: `{"error":{"code":7,"message":"User is blocked [WKE=unauthorized:blocked-user]","details":[]}}`,
			accountScoped: true, accountBlocked: true, upstreamCode: "", // numeric JSON code is not stringified by extractors
		},
		{
			name: "forbidden without block text", body: `{"error":{"code":7,"message":"Something went wrong","details":[]}}`,
		},
		{
			name: "top-level permanent chat denial", body: `{"status_code":403,"code":"permission-denied","error":"Access to the chat endpoint is denied. Please update the permissions."}`,
			accountScoped: true, permanentAccountDenial: true, upstreamCode: "permission-denied",
		},
		{
			name: "403 spending limit", status: http.StatusForbidden, body: `{"code":"personal-team-blocked:spending-limit","error":"quota exhausted"}`,
			accountScoped: true, quotaExhausted: true, spendingLimitBlocked: true, upstreamCode: "personal-team-blocked:spending-limit",
		},
		{
			name: "402 spending limit", status: http.StatusPaymentRequired, body: `{"code":"personal-team-blocked:spending-limit","error":"quota exhausted"}`,
			accountScoped: true, quotaExhausted: true, spendingLimitBlocked: true, upstreamCode: "personal-team-blocked:spending-limit",
		},
		{
			name: "unknown policy rejection", status: http.StatusForbidden, body: `{"error":"upstream policy rejected request"}`,
			requestScopedForbidden: true,
		},
		{
			name: "free model quota", status: http.StatusForbidden, body: `{"error":"You've used all the included free usage for model grok-build"}`,
			accountScoped: true, quotaExhausted: true, freeQuotaExhausted: true, modelQuotaExhausted: true,
		},
		{
			name: "safety usage guidelines", body: `{"code":"permission-denied","error":"Content violates usage guidelines. SAFETY_CHECK_TYPE_VIOLENCE"}`,
			safetyRejection: true, upstreamCode: "permission-denied",
		},
		{
			name: "explicit policy rejection", body: `{"code":"permission-denied","error":"request rejected by policy"}`,
			requestScopedForbidden: true,
			upstreamCode:           "permission-denied",
		},
		{
			name: "bare permission-denied", body: `{"code":"permission_denied","error":"denied"}`,
			upstreamCode: "permission_denied",
		},
		{
			name: "invalid argument code", body: `{"code":"invalid-argument","error":"unsupported request field"}`,
			requestScopedForbidden: true, upstreamCode: "invalid-argument",
		},
		{
			name: "request-level access denied sentence", body: `{"code":"operation-denied","error":"Access denied because this operation is unavailable under ZDR"}`,
			requestScopedForbidden: true, upstreamCode: "operation-denied",
		},
		{
			name: "legacy 403 credit exhaustion", body: `{"code":"permission-denied","error":"You have run out of credits"}`,
			accountScoped: true, quotaExhausted: true, upstreamCode: "permission-denied",
		},
		{
			name: "nested usage balance exhaustion", body: `{"error":{"type":"billing_error","message":"Grok Build usage balance exhausted"}}`,
			accountScoped: true, quotaExhausted: true,
		},
		{
			name: "exact access denied", body: `{"code":"operation-denied","error":"Access denied."}`,
			accountScoped: true, permanentAccountDenial: true, upstreamCode: "operation-denied",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := test.status
			if status == 0 {
				status = http.StatusForbidden
			}
			failure := newHTTPUpstreamFailure(status, []byte(test.body), 42, "build")
			if failure.HTTPStatus != status || failure.AccountScoped != test.accountScoped || failure.AccountBlocked != test.accountBlocked || failure.PermanentAccountDenial != test.permanentAccountDenial || failure.SafetyRejection != test.safetyRejection || failure.RequestScopedForbidden != test.requestScopedForbidden || failure.QuotaExhausted != test.quotaExhausted || failure.FreeQuotaExhausted != test.freeQuotaExhausted || failure.ModelQuotaExhausted != test.modelQuotaExhausted || failure.SpendingLimitBlocked != test.spendingLimitBlocked || failure.UpstreamCode != test.upstreamCode {
				t.Fatalf("failure = %#v", failure)
			}
			if test.upstreamCode == "permission-denied" && (failure.ClientCredentialErrorCode() != "permission-denied" || failure.AuditCode() != "upstream_forbidden_permission_denied") {
				t.Fatalf("public=%q audit=%q", failure.ClientCredentialErrorCode(), failure.AuditCode())
			}
		})
	}
}

func TestHTTPUpstreamFailureClassifiesDPoPRequirementAsSystemic(t *testing.T) {
	failure := newHTTPUpstreamFailure(http.StatusForbidden, []byte(`{"code":"unauthorized:dpop-required","error":"DPoP proof required but was not verified."}`), 42, "console")
	if failure.AccountScoped || failure.CredentialRejected || !failure.RequestScopedForbidden {
		t.Fatalf("failure = %#v", failure)
	}
	if failure.UpstreamCode != "unauthorized:dpop-required" || failure.Fingerprint != "403:unauthorized_dpop_required" {
		t.Fatalf("failure metadata = %#v", failure)
	}
}

func TestNonAccountFailureFingerprintStopsAtLimit(t *testing.T) {
	fingerprints := map[string]int{}
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusTooManyRequests,
	} {
		accountScoped := &UpstreamFailure{HTTPStatus: status, AccountScoped: true, Fingerprint: http.StatusText(status)}
		for i := 0; i < nonAccountFailureFingerprintLimit+5; i++ {
			if shouldStopForNonAccountFingerprint(fingerprints, accountScoped) {
				t.Fatalf("account-scoped %d stopped credential traversal at iteration %d", status, i)
			}
		}
	}
	if len(fingerprints) != 0 {
		t.Fatalf("account-scoped failures should not count fingerprints: %#v", fingerprints)
	}

	// 未知 403、Team 限流不计指纹，应持续换号。
	unknown403 := &UpstreamFailure{HTTPStatus: http.StatusForbidden, Fingerprint: "403:unknown"}
	teamLimit := &UpstreamFailure{HTTPStatus: http.StatusTooManyRequests, Fingerprint: "429:team_model_rate_limit"}
	for i := 0; i < nonAccountFailureFingerprintLimit+5; i++ {
		if shouldStopForNonAccountFingerprint(fingerprints, unknown403) {
			t.Fatalf("unknown 403 must not stop at iteration %d", i)
		}
		if shouldStopForNonAccountFingerprint(fingerprints, teamLimit) {
			t.Fatalf("team rate limit must not stop at iteration %d", i)
		}
	}
	if len(fingerprints) != 0 {
		t.Fatalf("excluded failure types should not count fingerprints: %#v", fingerprints)
	}

	network := &UpstreamFailure{Fingerprint: "upstream_timeout"}
	for i := 1; i < nonAccountFailureFingerprintLimit; i++ {
		if shouldStopForNonAccountFingerprint(fingerprints, network) {
			t.Fatalf("stopped early at count %d", i)
		}
	}
	if !shouldStopForNonAccountFingerprint(fingerprints, network) {
		t.Fatalf("should stop after %d non-account failures", nonAccountFailureFingerprintLimit)
	}
	if fingerprints["upstream_timeout"] != nonAccountFailureFingerprintLimit {
		t.Fatalf("fingerprint count = %d, want %d", fingerprints["upstream_timeout"], nonAccountFailureFingerprintLimit)
	}

	idleFingerprints := map[string]int{}
	idle := &UpstreamFailure{
		HTTPStatus: http.StatusGatewayTimeout, Code: "upstream_stream_idle_timeout",
		Fingerprint: "upstream_stream_idle_timeout",
	}
	if shouldStopForNonAccountFingerprint(idleFingerprints, idle) {
		t.Fatal("the first stream idle failure should allow one compensating account switch")
	}
	if !shouldStopForNonAccountFingerprint(idleFingerprints, idle) {
		t.Fatalf("stream idle failures should stop after %d attempts", streamIdleFailureFingerprintLimit)
	}
	if idleFingerprints[idle.Fingerprint] != streamIdleFailureFingerprintLimit {
		t.Fatalf("idle fingerprint count = %d", idleFingerprints[idle.Fingerprint])
	}
}

func TestHTTPUpstreamFailureLeavesPaymentRecoveryKindToBilling(t *testing.T) {
	failure := newHTTPUpstreamFailure(http.StatusPaymentRequired, []byte(`{
		"code":"personal-team-blocked:spending-limit",
		"error":"You have run out of credits"
	}`), 42, "build")
	if !failure.AccountScoped || !failure.QuotaExhausted || failure.FreeQuotaExhausted || failure.UpstreamCode != "personal-team-blocked:spending-limit" {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestRetryableResponseHonorsUpstreamRetryVeto(t *testing.T) {
	response := &provider.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"X-Should-Retry": {"false"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"invalid request history"}`)),
	}
	if isRetryableResponse(response, accountdomain.ProviderBuild) {
		t.Fatal("x-should-retry:false 必须禁止换账号重试")
	}
	response.Header.Set("X-Should-Retry", "true")
	if !isRetryableResponse(response, accountdomain.ProviderBuild) {
		t.Fatal("x-should-retry:true 不应覆盖现有状态码重试策略")
	}
	response.Header.Set("X-Should-Retry", "unknown")
	if !isRetryableResponse(response, accountdomain.ProviderBuild) {
		t.Fatal("未知 x-should-retry 值应按未提供处理")
	}
}

func TestPaymentRequiredAlwaysRetriesDespiteUpstreamVeto(t *testing.T) {
	response := &provider.Response{
		StatusCode: http.StatusPaymentRequired,
		Header:     http.Header{"X-Should-Retry": {"false"}},
		Body:       io.NopCloser(strings.NewReader(`{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits"}`)),
	}
	if !isRetryableResponse(response, accountdomain.ProviderBuild) {
		t.Fatal("402 spending-limit must force account rotation even when X-Should-Retry is false")
	}
	if isRetryableResponse(response, accountdomain.ProviderWeb) {
		t.Fatal("non-Build 402 must continue honoring X-Should-Retry:false")
	}
}

func TestBuildForbiddenAlwaysEntersAccountFailureHandling(t *testing.T) {
	response := &provider.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"X-Should-Retry": {"false"}},
		Body:       io.NopCloser(strings.NewReader(`{"code":"permission-denied"}`)),
	}
	if !isRetryableResponse(response, accountdomain.ProviderBuild) {
		t.Fatal("Build 403 must enter account failure handling even when X-Should-Retry is false")
	}
	if isRetryableResponse(response, accountdomain.ProviderWeb) {
		t.Fatal("non-Build 403 must continue honoring X-Should-Retry:false")
	}
}

func TestBuildForbiddenReauthPolicyMatchesExactErrorCodes(t *testing.T) {
	service := &Service{}
	service.UpdateBuildForbiddenReauthPolicy(true, []string{"permission-denied", "team-access-denied"})

	for _, code := range []string{"permission-denied", "permission_denied", "TEAM-ACCESS-DENIED"} {
		failure := &UpstreamFailure{HTTPStatus: http.StatusForbidden, UpstreamCode: code, AccountScoped: true}
		if !service.shouldInvalidateBuildForbidden(failure) {
			t.Fatalf("configured code %q did not match", code)
		}
	}
	for _, failure := range []*UpstreamFailure{
		{HTTPStatus: http.StatusForbidden, UpstreamCode: "permission-denied"},
		{HTTPStatus: http.StatusForbidden, UpstreamCode: "permission-denied", AccountScoped: true, RequestScopedForbidden: true},
		{HTTPStatus: http.StatusForbidden, UpstreamCode: "unconfigured-denial", AccountScoped: true},
		{HTTPStatus: http.StatusUnauthorized, UpstreamCode: "permission-denied", AccountScoped: true},
		{HTTPStatus: http.StatusInternalServerError, UpstreamCode: "permission-denied", AccountScoped: true},
	} {
		if service.shouldInvalidateBuildForbidden(failure) {
			t.Fatalf("unconfigured or ineligible failure matched: %#v", failure)
		}
	}

	service.UpdateBuildForbiddenReauthPolicy(false, []string{"permission-denied"})
	if service.shouldInvalidateBuildForbidden(&UpstreamFailure{HTTPStatus: http.StatusForbidden, UpstreamCode: "permission-denied", AccountScoped: true}) {
		t.Fatal("disabled policy matched an error code")
	}
}

func TestHTTPUpstreamFailureClassifiesSubscriptionFreeUsageAsAccountQuota(t *testing.T) {
	body := `{"code":"subscription:free-usage-exhausted","error":"tokens (actual/limit): 10/10"}`
	failure := newHTTPUpstreamFailure(http.StatusTooManyRequests, []byte(body), 7, "build")
	if !failure.AccountScoped || !failure.FreeQuotaExhausted || failure.ModelQuotaExhausted || !failure.QuotaExhausted {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestHTTPUpstreamFailureKeepsExplicitModelFreeUsageScoped(t *testing.T) {
	body := `{"error":"You've used all the included free usage for model grok-build"}`
	failure := newHTTPUpstreamFailure(http.StatusTooManyRequests, []byte(body), 7, "build")
	if !failure.AccountScoped || !failure.FreeQuotaExhausted || !failure.ModelQuotaExhausted || !failure.QuotaExhausted {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestBuildRateLimitForcesAccountFailoverDespiteRetryVeto(t *testing.T) {
	response := &provider.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"X-Should-Retry": {"false"}},
		Body:       io.NopCloser(strings.NewReader(`{"code":"subscription:free-usage-exhausted"}`)),
	}
	if !isRetryableResponse(response, accountdomain.ProviderBuild) {
		t.Fatal("Build free-usage 429 must force account rotation even when X-Should-Retry is false")
	}
	if isRetryableResponse(response, accountdomain.ProviderWeb) {
		t.Fatal("non-Build 429 must continue honoring X-Should-Retry:false")
	}
}

func TestBuildForbiddenReauthPolicyIgnoresSafetyRejection(t *testing.T) {
	service := &Service{}
	service.UpdateBuildForbiddenReauthPolicy(true, []string{"permission-denied"})
	failure := &UpstreamFailure{HTTPStatus: http.StatusForbidden, UpstreamCode: "permission-denied", AccountScoped: true, SafetyRejection: true}
	if service.shouldInvalidateBuildForbidden(failure) {
		t.Fatal("safety rejection must not match permission-denied invalidation policy")
	}
}

func TestTerminalRequestForbiddenRequiresExplicitRequestSignal(t *testing.T) {
	bare := &UpstreamFailure{HTTPStatus: http.StatusForbidden, UpstreamCode: "permission-denied"}
	if isTerminalRequestForbidden(accountdomain.ProviderBuild, bare) {
		t.Fatal("bare permission-denied must remain on credential traversal")
	}

	requestScoped := &UpstreamFailure{HTTPStatus: http.StatusForbidden, UpstreamCode: "invalid-argument", RequestScopedForbidden: true}
	if !isTerminalRequestForbidden(accountdomain.ProviderBuild, requestScoped) {
		t.Fatal("explicit Build request rejection must be terminal")
	}
	for _, providerValue := range []accountdomain.Provider{accountdomain.ProviderWeb, accountdomain.ProviderConsole} {
		if isTerminalRequestForbidden(providerValue, requestScoped) {
			t.Fatalf("%s request classification must retain existing egress recovery", providerValue)
		}
	}

	dpopRequired := &UpstreamFailure{HTTPStatus: http.StatusForbidden, UpstreamCode: "unauthorized:dpop-required", RequestScopedForbidden: true}
	if !isTerminalRequestForbidden(accountdomain.ProviderConsole, dpopRequired) {
		t.Fatal("Console DPoP auth-scheme requirement must be terminal")
	}
	if isTerminalRequestForbidden(accountdomain.ProviderWeb, dpopRequired) {
		t.Fatal("Console-specific DPoP classification must not change Web recovery")
	}

	safety := &UpstreamFailure{HTTPStatus: http.StatusForbidden, UpstreamCode: "permission-denied", SafetyRejection: true}
	for _, providerValue := range []accountdomain.Provider{accountdomain.ProviderBuild, accountdomain.ProviderWeb, accountdomain.ProviderConsole} {
		if !isTerminalRequestForbidden(providerValue, safety) {
			t.Fatalf("%s safety rejection must remain terminal", providerValue)
		}
	}
}
