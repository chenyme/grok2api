package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"google.golang.org/protobuf/encoding/protowire"
)

const weeklyQuotaMode = "weekly"

func (a *Adapter) SyncQuota(ctx context.Context, credential account.Credential) (provider.QuotaSnapshot, error) {
	windows := make([]account.QuotaWindow, 0, 2)
	autoWindow, autoErr := a.SyncQuotaMode(ctx, credential, "auto")
	if errors.Is(autoErr, provider.ErrUnauthorized) {
		return provider.QuotaSnapshot{}, autoErr
	}
	if autoErr == nil {
		windows = append(windows, autoWindow)
	}
	fastWindow, fastErr := a.SyncQuotaMode(ctx, credential, "fast")
	if errors.Is(fastErr, provider.ErrUnauthorized) {
		return provider.QuotaSnapshot{}, fastErr
	}
	if fastErr == nil {
		windows = append(windows, fastWindow)
	}
	if len(windows) > 0 {
		tier, _ := resolveWebTierFromQuota(credential.WebTier, windows, false)
		// Basic/未知账号没有付费周池，避免为每次完整同步额外访问付费端点。
		// 只有模式额度已经确认付费等级时才读取 weekly 作为权威额度。
		if tier == account.WebTierSuper || tier == account.WebTierHeavy {
			if weekly, weeklyErr := a.syncWeeklyCredits(ctx, credential); weeklyErr == nil {
				windows = []account.QuotaWindow{weekly}
			}
		}
		return provider.QuotaSnapshot{Tier: tier, Windows: windows, SyncedAt: time.Now().UTC()}, nil
	}
	// 模式端点暂不可用时，仅已确认的付费账号允许用 weekly 兜底；
	// Basic/Auto 不能凭周额度探测提权，也不应制造无意义的付费端点流量。
	if credential.WebTier == account.WebTierSuper || credential.WebTier == account.WebTierHeavy {
		if weekly, weeklyErr := a.syncWeeklyCredits(ctx, credential); weeklyErr == nil {
			return provider.QuotaSnapshot{Tier: credential.WebTier, Windows: []account.QuotaWindow{weekly}, SyncedAt: time.Now().UTC()}, nil
		} else {
			return provider.QuotaSnapshot{}, weeklyErr
		}
	}
	if fastErr != nil {
		return provider.QuotaSnapshot{}, fastErr
	}
	return provider.QuotaSnapshot{}, autoErr
}

func resolveWebTierFromQuota(current account.WebTier, windows []account.QuotaWindow, weeklyAvailable bool) (account.WebTier, bool) {
	if len(windows) > 0 {
		tier, known := inferWebTierFromQuota(windows)
		if !known {
			// 上游可能随时调整额度。无法识别的新形态保持 Auto，并由路由层
			// 按 Basic 的最小权限处理；不能伪造一个已确认的套餐等级。
			return account.WebTierAuto, false
		}
		return tier, weeklyAvailable && tier != account.WebTierBasic
	}
	// 周额度是付费账号信号，但无法区分 Super/Heavy。已确认的等级在模式
	// 额度暂时不可用时保留；未确认（Auto/Basic）的不能凭周额度提权。
	if current == account.WebTierHeavy || current == account.WebTierSuper {
		return current, weeklyAvailable
	}
	return current, false
}

// inferWebTierFromQuota 使用 Grok Web /rest/rate-limits 的真实额度形态判级。
// 同一快照出现矛盾信号时选择较低等级，避免把 Basic 账号路由到付费能力。
func inferWebTierFromQuota(windows []account.QuotaWindow) (account.WebTier, bool) {
	detected := account.WebTierAuto
	rank := map[account.WebTier]int{
		account.WebTierBasic: 1,
		account.WebTierSuper: 2,
		account.WebTierHeavy: 3,
	}
	for _, window := range windows {
		candidate := account.WebTierAuto
		switch window.Mode {
		case "auto":
			switch window.Total {
			case 7, 20:
				candidate = account.WebTierBasic
			case 50:
				candidate = account.WebTierSuper
			case 150:
				candidate = account.WebTierHeavy
			}
		case "fast":
			switch window.Total {
			case 30:
				candidate = account.WebTierBasic
			case 140:
				candidate = account.WebTierSuper
			case 400:
				candidate = account.WebTierHeavy
			}
		case "heavy":
			if window.Total > 0 {
				candidate = account.WebTierHeavy
			}
		}
		if candidate != account.WebTierAuto && (detected == account.WebTierAuto || rank[candidate] < rank[detected]) {
			detected = candidate
		}
	}
	return detected, detected != account.WebTierAuto
}

func (a *Adapter) SyncQuotaMode(ctx context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	if mode == weeklyQuotaMode {
		return a.syncWeeklyCredits(ctx, credential)
	}
	cfg := a.config()
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return account.QuotaWindow{}, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)
	if err != nil {
		return account.QuotaWindow{}, err
	}
	defer lease.Release()
	payload, _ := json.Marshal(map[string]string{"modelName": mode})
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.QuotaTimeoutSeconds)*time.Second)
	defer cancel()
	endpoint := cfg.BaseURL + "/rest/rate-limits"
	var response *http.Response
	var body []byte
	for attempt := 0; attempt < 2; attempt++ {
		request, requestErr := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if requestErr != nil {
			return account.QuotaWindow{}, requestErr
		}
		request.Header = buildHeaders(token, lease, "application/json")
		applyAppHeaders(request.Header, cfg.BaseURL, cfg.BaseURL+"/")
		a.applySignedStatsig(requestCtx, request, token, lease)
		response, err = lease.Do(request)
		if err != nil {
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
			return account.QuotaWindow{}, err
		}
		body, err = io.ReadAll(io.LimitReader(response.Body, 4<<20))
		_ = response.Body.Close()
		if err != nil {
			return account.QuotaWindow{}, err
		}
		if response.StatusCode == http.StatusForbidden {
			if attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, endpoint) {
				continue
			}
		}
		break
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
		if response.StatusCode == http.StatusUnauthorized {
			return account.QuotaWindow{}, provider.ErrUnauthorized
		}
		return account.QuotaWindow{}, fmt.Errorf("Grok Web 额度接口返回 %d", response.StatusCode)
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
	var value struct {
		WindowSizeSeconds int `json:"windowSizeSeconds"`
		RemainingQueries  int `json:"remainingQueries"`
		TotalQueries      int `json:"totalQueries"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return account.QuotaWindow{}, err
	}
	if value.TotalQueries <= 0 {
		return account.QuotaWindow{}, fmt.Errorf("Grok Web 额度响应缺少 totalQueries")
	}
	if value.WindowSizeSeconds <= 0 {
		value.WindowSizeSeconds = 7200
	}
	now := time.Now().UTC()
	resetAt := now.Add(time.Duration(value.WindowSizeSeconds) * time.Second)
	return account.QuotaWindow{
		AccountID: credential.ID, Mode: mode, Remaining: max(0, value.RemainingQueries), Total: value.TotalQueries,
		WindowSeconds: value.WindowSizeSeconds, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream, UpdatedAt: now,
	}, nil
}

func (a *Adapter) syncWeeklyCredits(ctx context.Context, credential account.Credential) (account.QuotaWindow, error) {
	cfg := a.config()
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return account.QuotaWindow{}, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)
	if err != nil {
		return account.QuotaWindow{}, err
	}
	defer lease.Release()

	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.QuotaTimeoutSeconds)*time.Second)
	defer cancel()
	endpoint := cfg.BaseURL + "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig"
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	if err != nil {
		return account.QuotaWindow{}, err
	}
	request.Header = buildHeaders(token, lease, "application/grpc-web+proto")
	applyAppHeaders(request.Header, cfg.BaseURL, cfg.BaseURL+"/")
	request.Header.Del("x-xai-request-id")
	request.Header.Set("x-grpc-web", "1")
	request.Header.Set("x-user-agent", "connect-es/2.1.1")

	response, err := lease.Do(request)
	if err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return account.QuotaWindow{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return account.QuotaWindow{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
		if response.StatusCode == http.StatusUnauthorized {
			return account.QuotaWindow{}, provider.ErrUnauthorized
		}
		return account.QuotaWindow{}, fmt.Errorf("Grok Web 周额度接口返回 %d", response.StatusCode)
	}
	window, err := parseWeeklyCreditsResponse(body, credential.ID, time.Now().UTC())
	if err != nil {
		return account.QuotaWindow{}, err
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
	return window, nil
}

func parseWeeklyCreditsResponse(body []byte, accountID uint64, syncedAt time.Time) (account.QuotaWindow, error) {
	payload, err := firstGRPCWebMessage(body)
	if err != nil {
		return account.QuotaWindow{}, err
	}
	config, err := protobufMessageField(payload, 1)
	if err != nil {
		return account.QuotaWindow{}, fmt.Errorf("解析 Grok Web 周额度响应: %w", err)
	}
	var usagePercent float64
	var usagePresent bool
	var periodStart, periodEnd *time.Time
	breakdown := make([]account.QuotaBreakdown, 0, 8)
	for len(config) > 0 {
		number, fieldType, n := protowire.ConsumeTag(config)
		if n < 0 {
			return account.QuotaWindow{}, fmt.Errorf("周额度 protobuf tag 无效")
		}
		config = config[n:]
		switch {
		case number == 1 && fieldType == protowire.Fixed32Type:
			value, consumed := protowire.ConsumeFixed32(config)
			if consumed < 0 {
				return account.QuotaWindow{}, fmt.Errorf("周额度使用率无效")
			}
			usagePercent = float64(math.Float32frombits(value))
			usagePresent = true
			config = config[consumed:]
		case (number == 4 || number == 5) && fieldType == protowire.BytesType:
			value, consumed := protowire.ConsumeBytes(config)
			if consumed < 0 {
				return account.QuotaWindow{}, fmt.Errorf("周额度周期无效")
			}
			parsed, parseErr := parseProtoTimestamp(value)
			if parseErr != nil {
				return account.QuotaWindow{}, parseErr
			}
			if number == 4 {
				periodStart = &parsed
			} else {
				periodEnd = &parsed
			}
			config = config[consumed:]
		case number == 7 && fieldType == protowire.BytesType:
			value, consumed := protowire.ConsumeBytes(config)
			if consumed < 0 {
				return account.QuotaWindow{}, fmt.Errorf("周额度产品分解无效")
			}
			if item, ok := parseQuotaBreakdown(value); ok {
				breakdown = append(breakdown, item)
			}
			config = config[consumed:]
		default:
			consumed := protowire.ConsumeFieldValue(number, fieldType, config)
			if consumed < 0 {
				return account.QuotaWindow{}, fmt.Errorf("周额度 protobuf 字段无效")
			}
			config = config[consumed:]
		}
	}
	if !usagePresent || math.IsNaN(usagePercent) || math.IsInf(usagePercent, 0) || usagePercent < 0 || usagePercent > 100 {
		return account.QuotaWindow{}, fmt.Errorf("Grok Web 周额度响应缺少有效使用率")
	}
	if periodStart == nil || periodEnd == nil || !periodEnd.After(*periodStart) {
		return account.QuotaWindow{}, fmt.Errorf("Grok Web 周额度响应缺少有效周期")
	}
	windowSeconds := int(periodEnd.Sub(*periodStart).Seconds())
	if windowSeconds < 24*60*60 || windowSeconds > 31*24*60*60 {
		return account.QuotaWindow{}, fmt.Errorf("Grok Web 周额度周期长度异常")
	}
	usedBasisPoints := int(math.Round(usagePercent * 100))
	return account.QuotaWindow{
		AccountID: accountID, Mode: weeklyQuotaMode, Remaining: max(0, 10000-usedBasisPoints), Total: 10000,
		UsagePercent: usagePercent, Breakdown: breakdown, WindowSeconds: windowSeconds,
		ResetAt: periodEnd, SyncedAt: &syncedAt, Source: account.QuotaSourceUpstream, UpdatedAt: syncedAt,
	}, nil
}

func firstGRPCWebMessage(body []byte) ([]byte, error) {
	var message []byte
	grpcStatus := ""
	for len(body) >= 5 {
		flag := body[0]
		length := int(binary.BigEndian.Uint32(body[1:5]))
		body = body[5:]
		if length < 0 || length > len(body) {
			return nil, fmt.Errorf("gRPC-Web 帧长度无效")
		}
		payload := body[:length]
		body = body[length:]
		if flag&0x80 == 0 {
			if flag != 0 {
				return nil, fmt.Errorf("不支持压缩的 gRPC-Web 响应")
			}
			if message == nil {
				message = append([]byte(nil), payload...)
			}
			continue
		}
		for _, line := range bytes.Split(payload, []byte{'\n'}) {
			name, value, ok := bytes.Cut(bytes.TrimSpace(line), []byte{':'})
			if ok && string(bytes.ToLower(bytes.TrimSpace(name))) == "grpc-status" {
				grpcStatus = string(bytes.TrimSpace(value))
			}
		}
	}
	if grpcStatus != "" && grpcStatus != "0" {
		return nil, fmt.Errorf("Grok Web 周额度 gRPC 状态为 %s", grpcStatus)
	}
	if message == nil {
		return nil, fmt.Errorf("Grok Web 周额度响应缺少消息帧")
	}
	return message, nil
}

func protobufMessageField(message []byte, target protowire.Number) ([]byte, error) {
	for len(message) > 0 {
		number, fieldType, n := protowire.ConsumeTag(message)
		if n < 0 {
			return nil, fmt.Errorf("protobuf tag 无效")
		}
		message = message[n:]
		if number == target && fieldType == protowire.BytesType {
			value, consumed := protowire.ConsumeBytes(message)
			if consumed < 0 {
				return nil, fmt.Errorf("protobuf message 无效")
			}
			return value, nil
		}
		consumed := protowire.ConsumeFieldValue(number, fieldType, message)
		if consumed < 0 {
			return nil, fmt.Errorf("protobuf 字段无效")
		}
		message = message[consumed:]
	}
	return nil, fmt.Errorf("protobuf 缺少字段 %d", target)
}

func parseProtoTimestamp(message []byte) (time.Time, error) {
	var seconds int64
	var nanos int32
	for len(message) > 0 {
		number, fieldType, n := protowire.ConsumeTag(message)
		if n < 0 {
			return time.Time{}, fmt.Errorf("protobuf timestamp tag 无效")
		}
		message = message[n:]
		if fieldType == protowire.VarintType && (number == 1 || number == 2) {
			value, consumed := protowire.ConsumeVarint(message)
			if consumed < 0 {
				return time.Time{}, fmt.Errorf("protobuf timestamp 值无效")
			}
			if number == 1 {
				seconds = int64(value)
			} else {
				nanos = int32(value)
			}
			message = message[consumed:]
			continue
		}
		consumed := protowire.ConsumeFieldValue(number, fieldType, message)
		if consumed < 0 {
			return time.Time{}, fmt.Errorf("protobuf timestamp 字段无效")
		}
		message = message[consumed:]
	}
	if seconds <= 0 || nanos < 0 || nanos >= int32(time.Second) {
		return time.Time{}, fmt.Errorf("protobuf timestamp 范围无效")
	}
	return time.Unix(seconds, int64(nanos)).UTC(), nil
}

func parseQuotaBreakdown(message []byte) (account.QuotaBreakdown, bool) {
	var result account.QuotaBreakdown
	var codePresent bool
	for len(message) > 0 {
		number, fieldType, n := protowire.ConsumeTag(message)
		if n < 0 {
			return account.QuotaBreakdown{}, false
		}
		message = message[n:]
		switch {
		case number == 1 && fieldType == protowire.VarintType:
			value, consumed := protowire.ConsumeVarint(message)
			if consumed < 0 {
				return account.QuotaBreakdown{}, false
			}
			result.ProductCode = int(value)
			codePresent = true
			message = message[consumed:]
		case number == 2 && fieldType == protowire.Fixed32Type:
			value, consumed := protowire.ConsumeFixed32(message)
			if consumed < 0 {
				return account.QuotaBreakdown{}, false
			}
			result.UsagePercent = float64(math.Float32frombits(value))
			message = message[consumed:]
		default:
			consumed := protowire.ConsumeFieldValue(number, fieldType, message)
			if consumed < 0 {
				return account.QuotaBreakdown{}, false
			}
			message = message[consumed:]
		}
	}
	if !codePresent || result.ProductCode < 0 || result.UsagePercent < 0 || result.UsagePercent > 100 || math.IsNaN(result.UsagePercent) || math.IsInf(result.UsagePercent, 0) {
		return account.QuotaBreakdown{}, false
	}
	return result, true
}
