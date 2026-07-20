package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/google/uuid"
)

const defaultBuildOAuthClientID = "b1a00492-073a-47ea-816f-4c329264a828"

// ReportReason 是 CLI 号池报障原因。
type ReportReason string

const (
	ReportReasonOK                 ReportReason = "ok"
	ReportReasonQuotaExhausted     ReportReason = "quota_exhausted"
	ReportReasonFreeUsageExhausted ReportReason = "free_usage_exhausted"
	ReportReasonAuthRejected       ReportReason = "auth_rejected"
)

// CliPool 为 grok-build CLI 提供与网关共用 Selector 的租号/报障/释放能力。
type CliPool struct {
	selector *Selector
	repo     repository.AccountRepository
	accounts *accountapp.Service
	cipher   *security.Cipher
	mu       sync.Mutex
	leases   map[string]*cliPoolLease
}

type cliPoolLease struct {
	lease   *accountLease
	billing *account.Billing
}

// LeaseResult 是租号成功后返回给 CLI 的明文 OAuth 凭据。
type LeaseResult struct {
	LeaseID      string     `json:"leaseId"`
	AccountID    uint64     `json:"accountId"`
	Name         string     `json:"name"`
	Email        string     `json:"email,omitempty"`
	UserID       string     `json:"userId,omitempty"`
	TeamID       string     `json:"teamId,omitempty"`
	ClientID     string     `json:"clientId"`
	AccessToken  string     `json:"accessToken"`
	RefreshToken string     `json:"refreshToken,omitempty"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
}

// NewCliPool 创建 CLI 号池服务。
func NewCliPool(selector *Selector, repo repository.AccountRepository, accounts *accountapp.Service, cipher *security.Cipher) *CliPool {
	return &CliPool{
		selector: selector,
		repo:     repo,
		accounts: accounts,
		cipher:   cipher,
		leases:   make(map[string]*cliPoolLease),
	}
}

// Lease 从 grok_build 号池租用一个账号（可排除已试账号）。
func (p *CliPool) Lease(ctx context.Context, excluded []uint64) (LeaseResult, error) {
	excludedSet := make(map[uint64]bool, len(excluded))
	for _, id := range excluded {
		if id != 0 {
			excludedSet[id] = true
		}
	}
	lease, err := p.selector.Acquire(ctx, account.ProviderBuild, "", "", "", excludedSet, false)
	if err != nil {
		return LeaseResult{}, err
	}
	accessToken, err := p.cipher.Decrypt(lease.Credential.EncryptedAccessToken)
	if err != nil {
		lease.Release()
		return LeaseResult{}, fmt.Errorf("解密 access token: %w", err)
	}
	refreshToken, err := p.cipher.Decrypt(lease.Credential.EncryptedRefreshToken)
	if err != nil {
		lease.Release()
		return LeaseResult{}, fmt.Errorf("解密 refresh token: %w", err)
	}
	if accessToken == "" && refreshToken == "" {
		lease.Release()
		return LeaseResult{}, fmt.Errorf("账号 %d 没有可用 OAuth 凭据", lease.Credential.ID)
	}
	clientID := lease.Credential.OIDCClientID
	if clientID == "" {
		clientID = defaultBuildOAuthClientID
	}
	leaseID := uuid.NewString()
	result := LeaseResult{
		LeaseID:      leaseID,
		AccountID:    lease.Credential.ID,
		Name:         lease.Credential.Name,
		Email:        lease.Credential.Email,
		UserID:       lease.Credential.UserID,
		TeamID:       lease.Credential.TeamID,
		ClientID:     clientID,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}
	if !lease.Credential.ExpiresAt.IsZero() {
		expires := lease.Credential.ExpiresAt.UTC()
		result.ExpiresAt = &expires
	}
	p.mu.Lock()
	p.leases[leaseID] = &cliPoolLease{lease: lease, billing: lease.Billing}
	p.mu.Unlock()
	return result, nil
}

// Report 将 CLI 侧账号结果写回号池（不释放并发槽）。
func (p *CliPool) Report(ctx context.Context, accountID uint64, reason ReportReason, httpStatus int, leaseID string) error {
	if accountID == 0 {
		return fmt.Errorf("accountId 不能为空")
	}
	credential, billing, err := p.resolveCredential(ctx, accountID, leaseID)
	if err != nil {
		return err
	}
	switch reason {
	case ReportReasonOK:
		p.selector.MarkSuccess(ctx, credential)
		return nil
	case ReportReasonFreeUsageExhausted:
		p.selector.MarkFreeQuotaExhausted(ctx, credential, 0, 0)
		return nil
	case ReportReasonQuotaExhausted:
		if !p.selector.MarkPaidQuotaExhausted(ctx, credential, billing) {
			status := httpStatus
			if status == 0 {
				status = http.StatusPaymentRequired
			}
			p.selector.MarkFailure(ctx, credential, status, 0)
			p.selector.MarkFreeQuotaExhausted(ctx, credential, 0, 0)
		}
		return nil
	case ReportReasonAuthRejected:
		status := httpStatus
		if status == 0 {
			status = http.StatusUnauthorized
		}
		if p.accounts != nil {
			_ = p.accounts.MarkReauthRequired(ctx, credential.ID, "cli pool credential rejected")
		}
		p.selector.MarkFailure(ctx, credential, status, 0)
		p.selector.MarkQuotaStateChanged(credential.Provider)
		return nil
	default:
		return fmt.Errorf("未知报障原因: %s", reason)
	}
}

// Release 释放租约对应的并发槽。
func (p *CliPool) Release(leaseID string) error {
	if leaseID == "" {
		return fmt.Errorf("leaseId 不能为空")
	}
	p.mu.Lock()
	entry, ok := p.leases[leaseID]
	if ok {
		delete(p.leases, leaseID)
	}
	p.mu.Unlock()
	if !ok {
		return nil
	}
	entry.lease.Release()
	return nil
}

func (p *CliPool) resolveCredential(ctx context.Context, accountID uint64, leaseID string) (account.Credential, *account.Billing, error) {
	if leaseID != "" {
		p.mu.Lock()
		entry, ok := p.leases[leaseID]
		p.mu.Unlock()
		if ok && entry.lease != nil && entry.lease.Credential.ID == accountID {
			return entry.lease.Credential, entry.billing, nil
		}
	}
	credential, err := p.repo.Get(ctx, accountID)
	if err != nil {
		return account.Credential{}, nil, err
	}
	if credential.Provider != account.ProviderBuild {
		return account.Credential{}, nil, fmt.Errorf("账号 %d 不是 grok_build", accountID)
	}
	billings, err := p.repo.GetBillings(ctx, []uint64{accountID})
	if err != nil {
		return account.Credential{}, nil, err
	}
	billing, ok := billings[accountID]
	if !ok {
		return credential, nil, nil
	}
	return credential, &billing, nil
}

// IsSelectionUnavailable 判断错误是否为选号不可用。
func IsSelectionUnavailable(err error) bool {
	var unavailable *SelectionUnavailableError
	return errors.As(err, &unavailable)
}
