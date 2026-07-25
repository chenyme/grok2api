package account

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
)

const (
	healthProbeTimeout    = 3 * time.Minute
	healthProbeMaxTokens  = 16
	healthProbeFailReason = "批量测活失败"
	healthProbeBodyLimit  = 8 << 10
)

// BatchHealthProbe 对选定账号发送最小 Responses 探测。
// 成功时尽量同步 Billing；凭据/权限类失败标记 reauthRequired；额度耗尽只记额度状态，不标失效。
func (s *Service) BatchHealthProbe(ctx context.Context, ids []uint64, upstreamModel string) (int, int, int, error) {
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return 0, 0, 0, err
	}
	upstreamModel = normalizeHealthProbeModel(upstreamModel)
	if upstreamModel == "" {
		return 0, 0, 0, invalidInput("请选择测活目标模型")
	}
	if s.providers == nil {
		return 0, 0, 0, fmt.Errorf("Provider 注册表未初始化")
	}

	probeIDs := make([]uint64, 0, len(values))
	for _, id := range values {
		value, getErr := s.accounts.Get(ctx, id)
		if getErr != nil {
			return 0, 0, 0, mapRepositoryError(getErr)
		}
		if value.Provider != accountdomain.ProviderBuild {
			return 0, 0, 0, invalidInput("批量测活目前仅支持 Grok Build 账号")
		}
		if !value.Enabled || value.AuthStatus != accountdomain.AuthStatusActive {
			continue
		}
		if _, ok := s.providers.Responses(value.Provider); !ok {
			continue
		}
		probeIDs = append(probeIDs, id)
	}
	skipped := len(values) - len(probeIDs)
	s.logger.Info("health_probe_batch_start", "total", len(values), "probeable", len(probeIDs), "skipped", skipped, "upstream_model", upstreamModel)
	if len(probeIDs) == 0 {
		return 0, 0, skipped, nil
	}

	pool := s.probePool
	if pool == nil {
		pool = s.syncPool
	}
	succeeded, failed, err := s.runAccountBatch(ctx, "health_probe", probeIDs, pool, nil, func(workCtx context.Context, id uint64) error {
		return s.probeAccountHealth(workCtx, id, upstreamModel)
	})
	s.logger.Info("health_probe_batch_done", "succeeded", succeeded, "failed", failed, "skipped", skipped, "upstream_model", upstreamModel, "error", err)
	return succeeded, failed, skipped, err
}

func (s *Service) probeAccountHealth(ctx context.Context, id uint64, upstreamModel string) error {
	started := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()

	value, err := s.accounts.Get(probeCtx, id)
	if err != nil {
		return mapRepositoryError(err)
	}
	if value.Provider != accountdomain.ProviderBuild {
		return invalidInput("批量测活目前仅支持 Grok Build 账号")
	}
	if !value.Enabled || value.AuthStatus != accountdomain.AuthStatusActive {
		s.logger.Info("health_probe_skipped", "account_id", id, "account_name", value.Name, "enabled", value.Enabled, "auth_status", value.AuthStatus)
		return nil
	}
	adapter, ok := s.providers.Responses(value.Provider)
	if !ok {
		return fmt.Errorf("Provider %s 不支持对话探测", value.Provider)
	}

	s.logger.Info("health_probe_start", "account_id", id, "account_name", value.Name, "upstream_model", upstreamModel, "timeout", healthProbeTimeout.String())
	credential, err := s.EnsureCredential(probeCtx, value, false)
	if err != nil {
		s.logger.Warn("health_probe_credential_failed", "account_id", id, "account_name", value.Name, "error", err, "duration_ms", time.Since(started).Milliseconds())
		if isCredentialUnavailableError(err) {
			_ = s.markHealthProbeReauth(probeCtx, id, fmt.Sprintf("%s: 凭据不可用", healthProbeFailReason))
		}
		return err
	}

	// 使用原生 Responses 请求，避免 Chat 兼容层/缓存路由注入额外工具导致误伤。
	body, err := json.Marshal(map[string]any{
		"model":             upstreamModel,
		"stream":            false,
		"max_output_tokens": healthProbeMaxTokens,
		"input": []map[string]any{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "hi"},
				},
			},
		},
	})
	if err != nil {
		return err
	}

	var billing *accountdomain.Billing
	if snapshot, billingErr := s.accounts.GetBilling(probeCtx, id); billingErr == nil {
		billing = &snapshot
	}

	upstreamStarted := time.Now()
	response, err := adapter.ForwardResponse(probeCtx, provider.ResponseResourceRequest{
		Credential:    credential,
		Billing:       billing,
		Method:        http.MethodPost,
		Path:          "/responses",
		Body:          body,
		Model:         upstreamModel,
		Streaming:     false,
		NormalizeBody: true,
		// 留空 Operation：走原生 Responses 规范化，不注入 Chat 缓存路由工具。
	})
	upstreamMS := time.Since(upstreamStarted).Milliseconds()
	if err != nil {
		s.logger.Warn("health_probe_transport_failed", "account_id", id, "account_name", value.Name, "upstream_model", upstreamModel, "error", err, "upstream_ms", upstreamMS, "duration_ms", time.Since(started).Milliseconds())
		if isCredentialUnavailableError(err) {
			_ = s.markHealthProbeReauth(probeCtx, id, fmt.Sprintf("%s: %v", healthProbeFailReason, err))
		}
		return err
	}
	if response == nil || response.Body == nil {
		s.logger.Warn("health_probe_empty_response", "account_id", id, "account_name", value.Name, "upstream_ms", upstreamMS)
		return fmt.Errorf("测活返回空响应")
	}
	defer response.Body.Close()

	bodyBytes, bodyErr := io.ReadAll(io.LimitReader(response.Body, healthProbeBodyLimit))
	if bodyErr != nil {
		s.logger.Warn("health_probe_body_read_failed", "account_id", id, "account_name", value.Name, "status", response.StatusCode, "error", bodyErr, "upstream_ms", upstreamMS)
		return bodyErr
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		s.logger.Info("health_probe_success", "account_id", id, "account_name", value.Name, "upstream_model", upstreamModel, "status", response.StatusCode, "upstream_ms", upstreamMS, "duration_ms", time.Since(started).Milliseconds(), "body", compactProbeBody(bodyBytes))
		if _, billingErr := s.RefreshBilling(context.WithoutCancel(probeCtx), id); billingErr != nil {
			s.logger.Warn("health_probe_billing_sync_failed", "account_id", id, "account_name", value.Name, "error", billingErr)
		} else {
			s.logger.Info("health_probe_billing_synced", "account_id", id, "account_name", value.Name)
		}
		// 一次真实成功请求代表额度已恢复：清掉 free/paid 待重置状态，使 UI 立刻离开「待重置」。
		if err := s.accounts.ClearQuotaRecovery(context.WithoutCancel(probeCtx), id); err != nil {
			s.logger.Warn("health_probe_recovery_clear_failed", "account_id", id, "account_name", value.Name, "error", err)
		} else {
			s.logger.Info("health_probe_recovery_cleared", "account_id", id, "account_name", value.Name)
		}
		return nil
	}

	s.logger.Warn("health_probe_failed", "account_id", id, "account_name", value.Name, "upstream_model", upstreamModel, "status", response.StatusCode, "upstream_ms", upstreamMS, "duration_ms", time.Since(started).Milliseconds(), "body", compactProbeBody(bodyBytes))
	return s.handleHealthProbeFailure(probeCtx, credential, response.StatusCode, bodyBytes)
}

func (s *Service) handleHealthProbeFailure(ctx context.Context, credential accountdomain.Credential, status int, body []byte) error {
	snippet := compactProbeBody(body)
	text := strings.ToLower(snippet)
	reason := fmt.Sprintf("%s: 上游返回 %d", healthProbeFailReason, status)
	if snippet != "" {
		reason = fmt.Sprintf("%s: %s", reason, snippet)
	}

	action := "none"
	switch {
	case status == http.StatusBadRequest && containsAnyProbe(text, "model not found", "invalid-argument", "invalid_argument", "unknown model"):
		// 模型名/参数错误：不惩罚账号，方便改模型后立刻重试。
		action = "model_invalid_no_penalty"
	case status == http.StatusUnauthorized || (status == http.StatusForbidden && containsAnyProbe(text, "invalid_grant", "invalid token", "token expired", "unauthorized", "authentication", "permission-denied", "permission_denied", "access denied")):
		action = "mark_reauth"
		_ = s.markHealthProbeReauth(ctx, credential.ID, reason)
	case status == http.StatusPaymentRequired || containsAnyProbe(text, "spending-limit", "free-usage-exhausted", "usage-exhausted", "insufficient"):
		action = "quota_exhausted"
		if _, billingErr := s.RefreshBilling(context.WithoutCancel(ctx), credential.ID); billingErr != nil {
			s.logger.Warn("health_probe_quota_billing_sync_failed", "account_id", credential.ID, "error", billingErr)
		}
		if used, limit, exhausted := parseProbeFreeQuotaExhaustion(body); exhausted {
			now := time.Now().UTC()
			next := now.Add(24 * time.Hour)
			_ = s.accounts.SaveQuotaRecovery(context.WithoutCancel(ctx), accountdomain.QuotaRecovery{
				AccountID: credential.ID, Kind: accountdomain.QuotaRecoveryKindFree, Status: accountdomain.QuotaRecoveryStatusExhausted,
				ConfirmedUsed: used, ConfirmedLimit: limit, ExhaustedAt: &now, NextProbeAt: &next, LastConfirmedAt: &now, UpdatedAt: now,
			})
		}
	case status == http.StatusTooManyRequests:
		action = "rate_limit_cooldown"
		until := time.Now().UTC().Add(30 * time.Second)
		_ = s.accounts.UpdateHealth(context.WithoutCancel(ctx), credential.ID, credential.FailureCount+1, &until, reason, false)
	case status >= 500:
		action = "upstream_5xx_cooldown"
		until := time.Now().UTC().Add(30 * time.Second)
		_ = s.accounts.UpdateHealth(context.WithoutCancel(ctx), credential.ID, credential.FailureCount+1, &until, reason, false)
	default:
		// 其它 4xx：记录错误但不冷却，避免模型/参数问题把好号打进冷却。
		action = "client_error_no_penalty"
		_ = s.accounts.UpdateHealth(context.WithoutCancel(ctx), credential.ID, credential.FailureCount, nil, reason, false)
	}
	s.logger.Warn("health_probe_failure_action", "account_id", credential.ID, "account_name", credential.Name, "status", status, "action", action, "reason", reason)
	return fmt.Errorf("测活失败: 上游返回 %d (%s)", status, action)
}

// normalizeHealthProbeModel 去掉 Build/Web/Console 命名空间前缀，只保留上游真实模型名。
func normalizeHealthProbeModel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, providerValue := range accountdomain.Providers() {
		prefix := providerValue.ModelNamespace() + "/"
		if len(value) > len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
			return strings.TrimSpace(value[len(prefix):])
		}
	}
	if index := strings.Index(value, "/"); index >= 0 {
		// 兼容未知命名空间前缀。
		return strings.TrimSpace(value[index+1:])
	}
	return value
}

// markHealthProbeReauth 仅更新认证状态，不触碰凭据密文。
func (s *Service) markHealthProbeReauth(ctx context.Context, id uint64, reason string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	if err := s.MarkReauthRequired(writeCtx, id, reason); err != nil {
		s.logger.Error("health_probe_reauth_mark_failed", "account_id", id, "error", err)
		return err
	}
	return nil
}

func isCredentialUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "invalid_grant") ||
		strings.Contains(text, "refresh token") ||
		strings.Contains(text, "凭据") ||
		strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "token")
}

func compactProbeBody(body []byte) string {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	return snippet
}

func containsAnyProbe(text string, signals ...string) bool {
	for _, signal := range signals {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func parseProbeFreeQuotaExhaustion(body []byte) (int64, int64, bool) {
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "subscription:free-usage-exhausted") && !strings.Contains(text, "free-usage-exhausted") {
		return 0, 0, strings.Contains(text, "usage-exhausted") || strings.Contains(text, "used all the included free usage")
	}
	return 0, 0, true
}

// SetProbePool 绑定批量测活专用并发池；未设置时回退到 sync 池。
func (s *Service) SetProbePool(pool *batch.Pool) {
	if pool != nil {
		s.probePool = pool
	}
}
