package sessionidentity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/browserheaders"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const responseBodyLimit = 64 << 10

// Fetch 通过 Grok Web Session 接口读取 SSO 账号的稳定身份元数据。
// Web 与 Console 共用该链路，确保代理、Cookie、UA 和 Resin 身份一致。
func Fetch(ctx context.Context, baseURL string, credential account.Credential, egress *infraegress.Manager, cipher *security.Cipher) (provider.AccountIdentity, error) {
	if credential.AuthType != account.AuthTypeSSO || (credential.Provider != account.ProviderWeb && credential.Provider != account.ProviderConsole) {
		return provider.AccountIdentity{}, fmt.Errorf("仅 Grok Web 与 Console SSO 账号支持身份同步")
	}
	if egress == nil || cipher == nil {
		return provider.AccountIdentity{}, fmt.Errorf("Session 身份同步依赖未初始化")
	}
	token, err := cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	if strings.TrimSpace(token) == "" {
		return provider.AccountIdentity{}, provider.ErrUnauthorized
	}
	lease, err := egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	defer lease.Release()

	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	origin := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, origin+"/api/auth/session", nil)
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	request.Header = browserHeaders(token, origin, lease)
	response, err := lease.Do(request)
	if err != nil {
		egress.FeedbackForScope(context.WithoutCancel(ctx), domainegress.ScopeWeb, lease.NodeID, 0, err)
		return provider.AccountIdentity{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit+1))
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	if len(body) > responseBodyLimit {
		return provider.AccountIdentity{}, fmt.Errorf("Grok Session 响应超过安全上限")
	}
	egress.FeedbackForScope(context.WithoutCancel(ctx), domainegress.ScopeWeb, lease.NodeID, response.StatusCode, nil)
	if response.StatusCode == http.StatusUnauthorized {
		return provider.AccountIdentity{}, provider.ErrUnauthorized
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.AccountIdentity{}, fmt.Errorf("Grok Session 接口返回 %d", response.StatusCode)
	}
	return Parse(body)
}

func Parse(body []byte) (provider.AccountIdentity, error) {
	var value struct {
		Status  string `json:"status"`
		Session struct {
			UserID         string `json:"userId"`
			Email          string `json:"email"`
			OrganizationID string `json:"organizationId"`
		} `json:"session"`
		User struct {
			ID     string `json:"id"`
			UserID string `json:"userId"`
			Sub    string `json:"sub"`
			Email  string `json:"email"`
			TeamID string `json:"teamId"`
		} `json:"user"`
		ID     string `json:"id"`
		UserID string `json:"userId"`
		Sub    string `json:"sub"`
		Email  string `json:"email"`
		TeamID string `json:"teamId"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return provider.AccountIdentity{}, fmt.Errorf("解析 Grok Session: %w", err)
	}
	identity := provider.AccountIdentity{
		UserID: firstNonEmpty(value.Session.UserID, value.User.ID, value.User.UserID, value.User.Sub, value.ID, value.UserID, value.Sub),
		Email:  firstNonEmpty(value.Session.Email, value.User.Email, value.Email),
		TeamID: firstNonEmpty(value.Session.OrganizationID, value.User.TeamID, value.TeamID),
	}
	identity.UserID = strings.TrimSpace(identity.UserID)
	identity.Email = strings.TrimSpace(identity.Email)
	identity.TeamID = strings.TrimSpace(identity.TeamID)
	if identity.UserID == "" && identity.Email == "" {
		if strings.EqualFold(strings.TrimSpace(value.Status), "unauthenticated") {
			return provider.AccountIdentity{}, provider.ErrUnauthorized
		}
		return provider.AccountIdentity{}, fmt.Errorf("Grok Session 缺少账号身份")
	}
	return identity, nil
}

func browserHeaders(token, origin string, lease *infraegress.Lease) http.Header {
	userAgent := strings.TrimSpace(lease.UserAgent)
	if userAgent == "" {
		userAgent = infraegress.DefaultUserAgent
	}
	value := http.Header{}
	value.Set("Accept", "*/*")
	value.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	value.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	value.Set("Cache-Control", "no-cache")
	value.Set("Cookie", infraegress.BuildSSOCookie(token, lease.CFCookies))
	value.Set("Pragma", "no-cache")
	value.Set("Priority", "u=1, i")
	value.Set("Referer", origin+"/")
	value.Set("Sec-Fetch-Dest", "empty")
	value.Set("Sec-Fetch-Mode", "cors")
	value.Set("Sec-Fetch-Site", "same-origin")
	value.Set("User-Agent", userAgent)
	browserheaders.ApplyChromiumClientHints(value, userAgent)
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
