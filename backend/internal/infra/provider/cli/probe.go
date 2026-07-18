package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

const (
	// DefaultHealthProbeTimeout 单账号上游测活超时。
	DefaultHealthProbeTimeout = 20 * time.Second
	healthProbeModel          = "grok-4.5"
)

// probeBody 使用固定最小 Responses 载荷；测活只关心 HTTP 状态码。
var healthProbeBody = []byte(`{"model":"grok-4.5","input":"ping","max_output_tokens":1,"store":false}`)

// ProbeResponses 对单个 Build 账号发起一次最小 Responses 请求，返回 HTTP 状态码。
// 网络失败返回 err 且 statusCode=0；本地构造请求失败同样返回 err。
// 成功拿到 HTTP 响应时 body 会被丢弃，调用方只应依赖状态码。
func (a *Adapter) ProbeResponses(ctx context.Context, credential account.Credential, accessToken string) (statusCode int, err error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return 0, errEmptyAccessToken
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultHealthProbeTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url("/responses"), bytes.NewReader(healthProbeBody))
	if err != nil {
		return 0, err
	}
	if err := a.applyHeaders(req, credential, accessToken, healthProbeModel, "", false); err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

type emptyAccessTokenError struct{}

func (emptyAccessTokenError) Error() string { return "access token 为空" }

var errEmptyAccessToken emptyAccessTokenError
