package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
)

const (
	officialAccountsBaseURL       = "https://accounts.x.ai"
	webAccountSettingTimeout      = 15 * time.Second
	webAccountSettingBodyLimit    = 64 << 10
	webAccountSettingErrorPreview = 240
	webBirthDateLockedMarker      = "[WKE=account:birth-date-change-limit-reached]"
)

var (
	acceptTermsBody = []byte{0x00, 0x00, 0x00, 0x00, 0x02, 0x10, 0x01}
	enableNSFWBody  = []byte{
		0x00, 0x00, 0x00, 0x00, 0x20,
		0x0a, 0x02, 0x10, 0x01,
		0x12, 0x1a, 0x0a, 0x18,
		'a', 'l', 'w', 'a', 'y', 's', '_', 's', 'h', 'o', 'w', '_', 'n', 's', 'f', 'w', '_', 'c', 'o', 'n', 't', 'e', 'n', 't',
	}
)

// AcceptTerms 接受当前 Grok Web SSO 账号的上游服务协议。
func (a *Adapter) AcceptTerms(ctx context.Context, credential account.Credential) error {
	accountBaseURL := strings.TrimRight(a.accountsBaseURL, "/")
	if accountBaseURL == "" {
		accountBaseURL = officialAccountsBaseURL
	}
	webBaseURL := strings.TrimRight(a.config().BaseURL, "/")
	if webBaseURL == "" {
		return fmt.Errorf("Grok Web BaseURL 不能为空")
	}
	productBody, err := json.Marshal(struct {
		TOSVersion int `json:"tosVersion"`
	}{TOSVersion: account.CurrentWebTermsVersion})
	if err != nil {
		return err
	}
	return a.runWebAccountSettings(ctx, credential,
		webAccountSettingRequest{
			endpoint:    accountBaseURL + "/auth_mgmt.AuthManagement/SetTosAcceptedVersion",
			body:        acceptTermsBody,
			contentType: "application/grpc-web+proto",
			origin:      accountBaseURL,
			referer:     accountBaseURL + "/accept-tos",
			grpcWeb:     true,
			connectES:   true,
			withoutCF:   true,
		},
		webAccountSettingRequest{
			endpoint:    webBaseURL + "/rest/auth/set-tos-accepted",
			body:        productBody,
			contentType: "application/json",
			origin:      webBaseURL,
			referer:     webBaseURL + "/",
			statsig:     true,
			clientHints: true,
		},
	)
}

// SetBirthDate 设置 Grok Web 账号生日。上游接收 RFC3339 字符串；日期由应用层生成。
func (a *Adapter) SetBirthDate(ctx context.Context, credential account.Credential, birthDate time.Time) error {
	cfg := a.config()
	data, err := json.Marshal(map[string]string{
		"birthDate": birthDate.UTC().Format("2006-01-02") + "T16:00:00.000Z",
	})
	if err != nil {
		return err
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	err = a.runWebAccountSetting(ctx, credential, webAccountSettingRequest{
		endpoint:    baseURL + "/rest/auth/set-birth-date",
		body:        data,
		contentType: "application/json",
		origin:      baseURL,
		referer:     baseURL + "/",
		statsig:     true,
	})
	var upstreamErr *webAccountSettingError
	if errors.As(err, &upstreamErr) && upstreamErr.birthDateAlreadySet {
		return provider.ErrBirthDateAlreadySet
	}
	return err
}

// EnableNSFW 将上游内容偏好设置为 always_show_nsfw_content。
// 生日只负责年龄资料，NSFW 仍需独立调用本接口。
func (a *Adapter) EnableNSFW(ctx context.Context, credential account.Credential) error {
	cfg := a.config()
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	return a.runWebAccountSetting(ctx, credential, webAccountSettingRequest{
		endpoint:    baseURL + "/auth_mgmt.AuthManagement/UpdateUserFeatureControls",
		body:        enableNSFWBody,
		contentType: "application/grpc-web+proto",
		origin:      baseURL,
		referer:     baseURL + "/",
		grpcWeb:     true,
		statsig:     true,
	})
}

type webAccountSettingRequest struct {
	endpoint    string
	body        []byte
	contentType string
	origin      string
	referer     string
	grpcWeb     bool
	connectES   bool
	statsig     bool
	clientHints bool
	withoutCF   bool
}

func (a *Adapter) runWebAccountSetting(ctx context.Context, credential account.Credential, input webAccountSettingRequest) error {
	return a.runWebAccountSettings(ctx, credential, input)
}

func (a *Adapter) runWebAccountSettings(ctx context.Context, credential account.Credential, inputs ...webAccountSettingRequest) error {
	if credential.Provider != account.ProviderWeb || credential.AuthType != account.AuthTypeSSO {
		return fmt.Errorf("仅 Grok Web SSO 账号支持资料设置")
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return provider.ErrUnauthorized
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)
	if err != nil {
		return err
	}
	defer lease.Release()
	for _, input := range inputs {
		if err := a.executeWebAccountSetting(ctx, token, lease, input); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) executeWebAccountSetting(ctx context.Context, token string, lease *infraegress.Lease, input webAccountSettingRequest) error {
	for attempt := 0; attempt < 2; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, webAccountSettingTimeout)
		request, requestErr := http.NewRequestWithContext(requestCtx, http.MethodPost, input.endpoint, bytes.NewReader(input.body))
		if requestErr != nil {
			cancel()
			return requestErr
		}
		request.Header = buildHeaders(token, lease, input.contentType)
		if input.withoutCF {
			request.Header.Set("Cookie", infraegress.BuildSSOCookie(token, ""))
		}
		applyAppHeaders(request.Header, input.origin, input.referer)
		if input.clientHints {
			browserheaders.ApplyChromiumClientHints(request.Header, lease.UserAgent)
		}
		if input.grpcWeb {
			request.Header.Set("x-grpc-web", "1")
		}
		if input.connectES {
			request.Header.Set("x-user-agent", "connect-es/2.1.1")
		}
		if input.statsig {
			a.applySignedStatsig(requestCtx, request, token, lease)
		}

		response, requestErr := lease.Do(request)
		if requestErr != nil {
			cancel()
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, requestErr)
			return requestErr
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, webAccountSettingBodyLimit+1))
		_ = response.Body.Close()
		cancel()
		if readErr != nil {
			return readErr
		}
		if len(body) > webAccountSettingBodyLimit {
			return fmt.Errorf("Grok Web 账号设置响应超过安全上限")
		}
		if response.StatusCode == http.StatusForbidden && input.statsig && attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, input.endpoint) {
			continue
		}
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
		if response.StatusCode == http.StatusUnauthorized {
			return provider.ErrUnauthorized
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return newWebAccountSettingError(response.StatusCode, body)
		}
		if input.grpcWeb {
			if err := validateAccountSettingGRPCStatus(response, body); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("Grok Web Statsig 刷新后仍被拒绝")
}

func validateAccountSettingGRPCStatus(response *http.Response, body []byte) error {
	_, bodyStatus, err := parseGRPCWebFrames(body)
	if err != nil {
		return fmt.Errorf("解析 Grok Web 账号设置响应: %w", err)
	}
	for _, status := range []string{
		response.Header.Get("grpc-status"),
		response.Trailer.Get("grpc-status"),
		bodyStatus,
	} {
		status = strings.TrimSpace(status)
		if status != "" && status != "0" {
			return fmt.Errorf("Grok Web 账号设置 gRPC 状态 %s", status)
		}
	}
	return nil
}

type webAccountSettingError struct {
	status              int
	body                string
	birthDateAlreadySet bool
}

func newWebAccountSettingError(status int, body []byte) *webAccountSettingError {
	return &webAccountSettingError{
		status:              status,
		body:                accountSettingPreview(body),
		birthDateAlreadySet: status == http.StatusTooManyRequests && bytes.Contains(body, []byte(webBirthDateLockedMarker)),
	}
}

func (e *webAccountSettingError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("Grok Web 账号设置上游返回 %d", e.status)
	}
	return fmt.Sprintf("Grok Web 账号设置上游返回 %d: %s", e.status, e.body)
}

func (e *webAccountSettingError) HTTPStatusCode() int { return e.status }

func accountSettingPreview(body []byte) string {
	value := strings.Join(strings.Fields(string(body)), " ")
	if len(value) <= webAccountSettingErrorPreview {
		return value
	}
	return value[:webAccountSettingErrorPreview]
}

var _ provider.WebAccountSettingsAdapter = (*Adapter)(nil)
