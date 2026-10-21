package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/browserheaders"
)

// The Console usage panel lives on grok.com/dashboard and is served by the
// Cursor (WorkOS) identity layer. It needs a WorkOS session, which one GET of
// the dashboard login route exchanges for the SSO. Inference never needs it.
const (
	workosCookieSessionToken = "WorkosCursorSessionToken"
	workosCookieXAIOrigin    = "WorkosCursorXaiOrigin"
	workosCookieID           = "workos_id"

	workosLoginPath       = "/dashboard/api/auth/login"
	workosUsagePath       = "/dashboard/api/dashboard/v2/usage-status?scope=user"
	workosUsageRedirect   = "/dashboard/usage"
	workosMaxRedirects    = 12
	workosBodyLimit       = 1 << 20
	workosSessionMaxBytes = 16 << 10
)

var (
	// errConsoleWorkOSUnlinked marks an account whose xAI identity has never
	// been linked to Cursor: the login lands on Cursor's hosted sign-in page
	// and the session reports cursorMerged=false. Such accounts have no Console
	// credit pool, so retrying is pointless.
	errConsoleWorkOSUnlinked = fmt.Errorf("%w: %w: Grok 账号未与 Cursor 关联（cursorMerged=false），无 Console 额度", provider.ErrUnauthorized, provider.ErrConsoleUnlinked)
	errConsoleWorkOSMissing  = errors.New("Console WorkOS 会话换取未返回完整 Cookie")
	// errCloudflareRejected marks an HTML Cloudflare block or challenge page:
	// an egress failure shared by every account on that exit.
	errCloudflareRejected = errors.New("cloudflare rejected the egress")
)

// workosSession is the cached cookie triple. All three are required: any
// subset makes usage-status answer 401.
type workosSession struct {
	SessionToken string `json:"session_token"`
	XAIOrigin    string `json:"xai_origin"`
	WorkOSID     string `json:"workos_id"`
}

func (s workosSession) complete() bool {
	return strings.TrimSpace(s.SessionToken) != "" && strings.TrimSpace(s.XAIOrigin) != "" && strings.TrimSpace(s.WorkOSID) != ""
}

func (s workosSession) cookieHeader() string {
	return workosCookieSessionToken + "=" + s.SessionToken + "; " + workosCookieXAIOrigin + "=" + s.XAIOrigin + "; " + workosCookieID + "=" + s.WorkOSID
}

func (a *Adapter) decodeWorkOSSession(credential account.Credential) (workosSession, bool) {
	if strings.TrimSpace(credential.EncryptedConsoleWorkOSSession) == "" || a.cipher == nil {
		return workosSession{}, false
	}
	plain, err := a.cipher.Decrypt(credential.EncryptedConsoleWorkOSSession)
	if err != nil {
		return workosSession{}, false
	}
	var session workosSession
	if json.Unmarshal([]byte(plain), &session) != nil || !session.complete() {
		return workosSession{}, false
	}
	return session, true
}

func (a *Adapter) saveWorkOSSession(ctx context.Context, accountID uint64, session workosSession) {
	store := a.cacheStore()
	if store == nil || accountID == 0 || a.cipher == nil {
		return
	}
	data, err := json.Marshal(session)
	if err != nil || len(data) > workosSessionMaxBytes {
		return
	}
	encrypted, err := a.cipher.Encrypt(string(data))
	if err != nil {
		a.logger().Warn("console_workos_session_encrypt_failed", "account_id", accountID, "error", err)
		return
	}
	if err := store.UpdateConsoleWorkOSSession(context.WithoutCancel(ctx), accountID, encrypted); err != nil {
		a.logger().Warn("console_workos_session_cache_write_failed", "account_id", accountID, "error", err)
	}
}

// consoleUsageStatus is the weekly included-usage pool shared by every Console
// model. Amounts are integer cents; usage can exceed the budget because the
// upstream records cost after execution instead of pre-checking it.
type consoleUsageStatus struct {
	BudgetCents int
	UsedCents   int
	ResetsAt    *time.Time
}

// fetchConsoleUsage reads usage-status with the cached WorkOS session and
// exchanges a fresh one when the cache is missing or rejected (401). No expiry
// is tracked: the 401 is the refresh signal.
func (a *Adapter) fetchConsoleUsage(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease) (consoleUsageStatus, error) {
	session, cached := a.decodeWorkOSSession(credential)
	for attempt := 0; attempt < 2; attempt++ {
		if !cached {
			fresh, err := a.loginWorkOS(ctx, ssoToken, lease)
			if err != nil {
				return consoleUsageStatus{}, err
			}
			session = fresh
			a.saveWorkOSSession(ctx, credential.ID, session)
		}
		status, data, err := a.requestConsoleUsage(ctx, ssoToken, session, lease)
		if err != nil {
			return consoleUsageStatus{}, err
		}
		switch {
		case status >= 200 && status < 300:
			return parseConsoleUsageStatus(data)
		case status == http.StatusUnauthorized && cached:
			cached = false
			continue
		case status == http.StatusUnauthorized:
			return consoleUsageStatus{}, fmt.Errorf("%w: Console usage-status 拒绝新换取的 WorkOS 会话", provider.ErrUnauthorized)
		case status == http.StatusForbidden && provider.IsDefinitiveAccountBlockBody(data):
			return consoleUsageStatus{}, fmt.Errorf("%w: Console usage-status 账号被封禁", provider.ErrUnauthorized)
		default:
			if status == http.StatusForbidden {
				lease.InvalidateClearance()
				if reason := cloudflareRejection(data); reason != "" {
					return consoleUsageStatus{}, fmt.Errorf("%w: Console usage-status 被 %s", errCloudflareRejected, reason)
				}
			}
			return consoleUsageStatus{}, fmt.Errorf("Console usage-status 接口返回 %d", status)
		}
	}
	return consoleUsageStatus{}, errors.New("Console usage-status 重试状态无效")
}

func (a *Adapter) requestConsoleUsage(ctx context.Context, ssoToken string, session workosSession, lease *infraegress.Lease) (int, []byte, error) {
	origin := a.sessionOrigin()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+workosUsagePath, nil)
	if err != nil {
		return 0, nil, err
	}
	applyDashboardHeaders(request, lease)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Origin", origin)
	request.Header.Set("Referer", origin+workosUsageRedirect)
	request.Header.Set("Sec-Fetch-Dest", "empty")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("x-cursor-dashboard-surface", "v2")
	request.Header.Set("Cookie", infraegress.BuildSSOCookie(ssoToken, lease.CFCookies)+"; "+session.cookieHeader())
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, workosBodyLimit))
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, data, nil
}

// loginWorkOS follows the dashboard login redirect chain by hand (the egress
// client never follows redirects) and collects the WorkOS cookie triple from
// the grok.com hops. Linked accounts finish without any interactive page.
func (a *Adapter) loginWorkOS(ctx context.Context, ssoToken string, lease *infraegress.Lease) (workosSession, error) {
	origin := a.sessionOrigin()
	base, err := url.Parse(origin)
	if err != nil || base.Host == "" {
		return workosSession{}, fmt.Errorf("Console SessionBaseURL 无效: %q", origin)
	}
	jar, _ := cookiejar.New(nil)
	current := origin + workosLoginPath + "?redirect_uri=" + url.QueryEscape(origin+workosUsageRedirect)
	for hop := 0; hop <= workosMaxRedirects; hop++ {
		target, err := url.Parse(current)
		if err != nil {
			return workosSession{}, fmt.Errorf("Console WorkOS 跳转地址无效: %w", err)
		}
		if !isAllowedWorkOSHost(target, base.Host) {
			return workosSession{}, fmt.Errorf("Console WorkOS 跳转到未知域名 %s", target.Host)
		}
		if isCursorHostedSignIn(target) {
			return workosSession{}, a.classifyWorkOSSignIn(ctx, ssoToken, lease)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return workosSession{}, err
		}
		applyDashboardHeaders(request, lease)
		request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		request.Header.Set("Sec-Fetch-Dest", "document")
		request.Header.Set("Sec-Fetch-Mode", "navigate")
		request.Header.Set("Sec-Fetch-Site", "none")
		cookies := cookieHeaderFor(jar, target)
		if strings.EqualFold(target.Host, base.Host) {
			cookies = joinCookieHeaders(infraegress.BuildSSOCookie(ssoToken, lease.CFCookies), cookies)
		}
		if cookies != "" {
			request.Header.Set("Cookie", cookies)
		}
		response, err := lease.DoDeferredForbidden(request)
		if err != nil {
			return workosSession{}, err
		}
		jar.SetCookies(target, response.Cookies())
		location := response.Header.Get("Location")
		status := response.StatusCode
		var body []byte
		if status < 300 || status >= 400 {
			body, _, _ = provider.ReadDiagnosticBody(response.Body)
		}
		_ = response.Body.Close()
		if session, ok := workosSessionFromJar(jar, base); ok {
			return session, nil
		}
		if status >= 300 && status < 400 && location != "" {
			next, err := target.Parse(location)
			if err != nil {
				return workosSession{}, fmt.Errorf("Console WorkOS 跳转地址无效: %w", err)
			}
			current = next.String()
			continue
		}
		if status == http.StatusForbidden {
			if strings.Contains(strings.ToLower(target.Host), "cursor") || strings.Contains(strings.ToLower(target.Host), "workos") {
				return workosSession{}, a.classifyWorkOSSignIn(ctx, ssoToken, lease)
			}
			if !provider.IsDefinitiveAccountBlockBody(body) {
				lease.InvalidateClearance()
			}
			if reason := cloudflareRejection(body); reason != "" {
				return workosSession{}, fmt.Errorf("%w: Console WorkOS 会话换取被 %s", errCloudflareRejected, reason)
			}
			return workosSession{}, fmt.Errorf("Console WorkOS 会话换取返回 403")
		}
		if status == http.StatusUnauthorized {
			return workosSession{}, fmt.Errorf("%w: Console WorkOS 会话换取返回 401", provider.ErrUnauthorized)
		}
		// An expired SSO also ends here (200 without the cookie triple). Ask the
		// session endpoint so a dead SSO is reported as unauthorized rather than
		// as a transient exchange failure.
		if _, _, sessionErr := a.fetchCursorMerged(ctx, ssoToken, lease); errors.Is(sessionErr, provider.ErrUnauthorized) {
			return workosSession{}, fmt.Errorf("%w: Grok SSO 会话已失效（unauthenticated）", provider.ErrUnauthorized)
		}
		return workosSession{}, fmt.Errorf("%w (status %d)", errConsoleWorkOSMissing, status)
	}
	return workosSession{}, fmt.Errorf("Console WorkOS 跳转次数超过 %d", workosMaxRedirects)
}

// classifyWorkOSSignIn decides whether landing on Cursor's hosted sign-in is
// definitive. cursorMerged=false (or absent) on grok.com's session means the
// account was never unified and has no Console credits; a merged account that
// still lands there is treated as a transient upstream fault.
func (a *Adapter) classifyWorkOSSignIn(ctx context.Context, ssoToken string, lease *infraegress.Lease) error {
	merged, known, err := a.fetchCursorMerged(ctx, ssoToken, lease)
	if err != nil {
		return fmt.Errorf("Console WorkOS 会话换取跳转到 Cursor 登录页，关联状态读取失败: %w", err)
	}
	if known && merged {
		return errors.New("Console WorkOS 会话换取跳转到 Cursor 登录页（cursorMerged=true，按瞬时故障处理）")
	}
	return errConsoleWorkOSUnlinked
}

// fetchCursorMerged reads session.cursorMerged from grok.com/api/auth/session,
// which needs only the sso cookie. known=false when the field is absent, as on
// accounts created before the Cursor unification existed.
func (a *Adapter) fetchCursorMerged(ctx context.Context, ssoToken string, lease *infraegress.Lease) (bool, bool, error) {
	origin := a.sessionOrigin()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/auth/session", nil)
	if err != nil {
		return false, false, err
	}
	applyDashboardHeaders(request, lease)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Referer", origin+"/")
	request.Header.Set("Sec-Fetch-Dest", "empty")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Cookie", infraegress.BuildSSOCookie(ssoToken, lease.CFCookies))
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		return false, false, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, workosBodyLimit))
	if err != nil {
		return false, false, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		return false, false, provider.ErrUnauthorized
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, false, fmt.Errorf("Grok Session 接口返回 %d", response.StatusCode)
	}
	return parseCursorMerged(data)
}

func parseCursorMerged(data []byte) (bool, bool, error) {
	var payload struct {
		Status  string `json:"status"`
		Session struct {
			CursorMerged *bool `json:"cursorMerged"`
		} `json:"session"`
		CursorMerged *bool `json:"cursorMerged"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false, false, fmt.Errorf("解析 Grok Session: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(payload.Status), "unauthenticated") {
		return false, false, provider.ErrUnauthorized
	}
	if payload.Session.CursorMerged != nil {
		return *payload.Session.CursorMerged, true, nil
	}
	if payload.CursorMerged != nil {
		return *payload.CursorMerged, true, nil
	}
	return false, false, nil
}

func workosSessionFromJar(jar http.CookieJar, base *url.URL) (workosSession, bool) {
	var session workosSession
	// The callback sets the cookies on the dashboard path; ask for that path so
	// path-scoped cookies are returned as well.
	probe := *base
	probe.Path = "/dashboard/api/dashboard/v2/usage-status"
	probe.RawQuery = ""
	for _, cookie := range jar.Cookies(&probe) {
		switch cookie.Name {
		case workosCookieSessionToken:
			session.SessionToken = cookie.Value
		case workosCookieXAIOrigin:
			session.XAIOrigin = cookie.Value
		case workosCookieID:
			session.WorkOSID = cookie.Value
		}
	}
	return session, session.complete()
}

func cookieHeaderFor(jar http.CookieJar, target *url.URL) string {
	parts := make([]string, 0, 4)
	for _, cookie := range jar.Cookies(target) {
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(parts, "; ")
}

func joinCookieHeaders(values ...string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "; ")
}

// isAllowedWorkOSHost bounds the redirect chain to the hosts the Grok/Cursor
// login is known to traverse, so an unexpected Location cannot steer the SSO
// session to an arbitrary origin.
func isAllowedWorkOSHost(target *url.URL, sessionHost string) bool {
	host := strings.ToLower(target.Hostname())
	if strings.EqualFold(target.Host, sessionHost) {
		return target.Scheme == "https" || target.Scheme == "http"
	}
	if target.Scheme != "https" {
		return false
	}
	for _, suffix := range []string{"grok.com", "x.ai", "cursor.com", "cursor.sh", "workos.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// isCursorHostedSignIn recognizes the interactive endpoints of the Cursor
// unification flow; following them further only renders a login form.
func isCursorHostedSignIn(target *url.URL) bool {
	host := strings.ToLower(target.Hostname())
	if host == "authenticator.cursor.sh" {
		return true
	}
	return (host == "cursor.com" || host == "www.cursor.com") && strings.HasPrefix(target.Path, "/unify")
}

func applyDashboardHeaders(request *http.Request, lease *infraegress.Lease) {
	userAgent := strings.TrimSpace(lease.UserAgent)
	if userAgent == "" {
		userAgent = infraegress.DefaultUserAgent
	}
	request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("User-Agent", userAgent)
	browserheaders.ApplyChromiumClientHints(request.Header, userAgent)
}

func (a *Adapter) sessionOrigin() string {
	origin := strings.TrimRight(strings.TrimSpace(a.config().SessionBaseURL), "/")
	if origin == "" {
		origin = "https://grok.com"
	}
	return origin
}

func parseConsoleUsageStatus(data []byte) (consoleUsageStatus, error) {
	var payload struct {
		Weekly map[string]json.RawMessage `json:"weeklyIncludedUsage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return consoleUsageStatus{}, fmt.Errorf("解析 Console usage-status: %w", err)
	}
	if payload.Weekly == nil {
		return consoleUsageStatus{}, errors.New("Console usage-status 缺少 weeklyIncludedUsage")
	}
	budget, ok := jsonInteger(payload.Weekly["budgetCents"])
	if !ok || budget < 0 {
		return consoleUsageStatus{}, errors.New("Console usage-status budgetCents 无效")
	}
	// proto3 JSON omits zero values, so a missing usedCents means 0.
	used, ok := jsonInteger(payload.Weekly["usedCents"])
	if !ok {
		used = 0
	}
	if used < 0 {
		return consoleUsageStatus{}, errors.New("Console usage-status usedCents 无效")
	}
	result := consoleUsageStatus{BudgetCents: int(budget), UsedCents: int(used)}
	if resetsAt, ok := jsonTimestamp(payload.Weekly["resetsAt"]); ok {
		result.ResetsAt = &resetsAt
	}
	return result, nil
}

// jsonInteger accepts both JSON numbers and the decimal strings proto3 JSON
// uses for 64-bit integers.
func jsonInteger(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		if value, err := number.Int64(); err == nil {
			return value, true
		}
		if value, err := number.Float64(); err == nil {
			return int64(value), true
		}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
			return value, true
		}
	}
	return 0, false
}

// jsonTimestamp accepts RFC3339 strings, proto Timestamp objects, and Unix
// seconds or milliseconds.
func jsonTimestamp(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		text = strings.TrimSpace(text)
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UTC(), true
		}
		raw = json.RawMessage(text)
	}
	var object struct {
		Seconds json.RawMessage `json:"seconds"`
	}
	if json.Unmarshal(raw, &object) == nil && len(object.Seconds) > 0 {
		if seconds, ok := jsonInteger(object.Seconds); ok && seconds > 0 {
			return time.Unix(seconds, 0).UTC(), true
		}
	}
	if value, ok := jsonInteger(raw); ok && value > 0 {
		if value > 1_000_000_000_000 {
			return time.UnixMilli(value).UTC(), true
		}
		return time.Unix(value, 0).UTC(), true
	}
	return time.Time{}, false
}

// cloudflareRejection names the Cloudflare page behind an HTML 403 so logs and
// the admin UI say "change the exit" instead of a bare status code. Both are
// egress problems, never account problems.
func cloudflareRejection(body []byte) string {
	text := strings.ToLower(string(body))
	switch {
	case strings.Contains(text, "attention required! | cloudflare") || strings.Contains(text, "cf-error-details"):
		return "Cloudflare 拦截（Attention Required，当前出口 IP 被封，请更换代理或出口）"
	case strings.Contains(text, "just a moment") || strings.Contains(text, "cf-challenge") || strings.Contains(text, "challenge-platform"):
		return "Cloudflare 人机挑战拦截（Just a moment，请配置 Clearance 或更换出口）"
	default:
		return ""
	}
}
