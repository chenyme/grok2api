package console

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	consoleQuotaTimeout = 45 * time.Second
	// consoleWeeklyWindow is INCLUDED_USAGE_PERIOD_WEEKLY; the exact reset
	// instant comes from resetsAt and is aligned to account creation.
	consoleWeeklyWindow = 7 * 24 * time.Hour
)

func (a *Adapter) SyncQuota(ctx context.Context, credential account.Credential) (provider.QuotaSnapshot, error) {
	windows, syncedAt, err := a.syncConsoleQuotas(ctx, credential)
	if err != nil {
		return provider.QuotaSnapshot{}, err
	}
	return provider.QuotaSnapshot{Windows: windows, SyncedAt: syncedAt}, nil
}

func (a *Adapter) SyncQuotaMode(ctx context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	if !isConsoleQuotaMode(mode) {
		return account.QuotaWindow{}, fmt.Errorf("不支持的 Console 额度模式 %q", mode)
	}
	windows, _, err := a.syncConsoleQuotas(ctx, credential)
	if err != nil {
		return account.QuotaWindow{}, err
	}
	for _, window := range windows {
		if window.Mode == mode {
			return window, nil
		}
	}
	return account.QuotaWindow{}, fmt.Errorf("Console usage 响应缺少 %s 额度", consoleQuotaKind(mode))
}

// syncConsoleQuotas reads the weekly included-usage pool from the grok.com
// dashboard. The legacy DPoP /v1/usage endpoint reports limit 0 for every
// free account and says nothing about whether the account can be called.
//
// Chat, image and video all draw on the same cents budget, so the three
// Console modes mirror one snapshot; routing, recovery and display keep
// their per-mode shape while reading identical values.
func (a *Adapter) syncConsoleQuotas(ctx context.Context, credential account.Credential) ([]account.QuotaWindow, time.Time, error) {
	ssoToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, time.Time{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, consoleQuotaTimeout)
	defer cancel()
	// The dashboard is served by grok.com, so it shares the Web scope's exit,
	// browser fingerprint and Clearance, exactly like the Session identity sync.
	lease, err := a.egress.AcquireCredential(requestCtx, egressdomain.ScopeWeb, credential)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer lease.Release()
	if remaining := a.breaker.blocked(lease.NodeID); remaining > 0 {
		return nil, time.Time{}, &egressBlockedError{retryAfter: remaining}
	}
	usage, err := a.fetchConsoleUsage(requestCtx, credential, ssoToken, lease)
	if err != nil {
		if errors.Is(err, errCloudflareRejected) {
			delay, opened := a.breaker.trip(lease.NodeID)
			if opened {
				a.logger().Warn("console_quota_egress_blocked", "egress_node_id", lease.NodeID, "pause", delay, "error", err)
			}
			return nil, time.Time{}, &egressBlockedError{retryAfter: delay}
		}
		if !errors.Is(err, provider.ErrUnauthorized) {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeWeb, lease.NodeID, 0, err)
		}
		return nil, time.Time{}, err
	}
	a.breaker.reset(lease.NodeID)
	a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeWeb, lease.NodeID, http.StatusOK, nil)
	now := time.Now().UTC()
	return consoleQuotaWindows(credential.ID, usage, now), now, nil
}

func consoleQuotaWindows(accountID uint64, usage consoleUsageStatus, now time.Time) []account.QuotaWindow {
	remaining := usage.BudgetCents - usage.UsedCents
	if remaining < 0 {
		// The upstream records cost after execution, so one large call can
		// overshoot the weekly budget several times.
		remaining = 0
	}
	usagePercent := 100.0
	if usage.BudgetCents > 0 {
		usagePercent = math.Min(100, float64(usage.UsedCents)/float64(usage.BudgetCents)*100)
	}
	var resetAt *time.Time
	if usage.ResetsAt != nil {
		value := usage.ResetsAt.UTC()
		resetAt = &value
	}
	windows := make([]account.QuotaWindow, 0, 3)
	for _, mode := range []string{QuotaMode, QuotaModeImage, QuotaModeVideo} {
		var modeResetAt *time.Time
		if resetAt != nil {
			value := *resetAt
			modeResetAt = &value
		}
		syncedAt := now
		windows = append(windows, account.QuotaWindow{
			AccountID: accountID, Mode: mode, Remaining: remaining, Total: usage.BudgetCents,
			UsagePercent: usagePercent, WindowSeconds: int(consoleWeeklyWindow / time.Second), ResetAt: modeResetAt,
			SyncedAt: &syncedAt, Source: account.QuotaSourceUpstream, UpdatedAt: now,
		})
	}
	return windows
}

func isConsoleQuotaMode(mode string) bool {
	return mode == QuotaMode || mode == QuotaModeImage || mode == QuotaModeVideo
}

func consoleQuotaMode(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "chat":
		return QuotaMode
	case "image":
		return QuotaModeImage
	case "video":
		return QuotaModeVideo
	default:
		return ""
	}
}

func consoleQuotaKind(mode string) string {
	switch mode {
	case QuotaMode:
		return "chat"
	case QuotaModeImage:
		return "image"
	case QuotaModeVideo:
		return "video"
	default:
		return mode
	}
}
