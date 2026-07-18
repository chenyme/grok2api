package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type providerLinkRepository interface {
	ReconcileProviderLinks(ctx context.Context, accountID uint64) error
	UpdateIdentityMetadata(ctx context.Context, accountID uint64, email, userID, teamID string) error
}

// SyncAccountIdentity 尽力补充 Web/Console 的稳定上游身份，并据此建立高可信弱关联。
// 只有明确的 401 会将当前 Provider 账号移出号池；其他同步失败不影响健康状态。
func (s *Service) SyncAccountIdentity(ctx context.Context, id uint64) error {
	_, err, _ := s.identitySyncs.Do(fmt.Sprintf("%d", id), func() (any, error) {
		return nil, s.syncAccountIdentity(ctx, id)
	})
	return err
}

func (s *Service) syncAccountIdentity(ctx context.Context, id uint64) error {
	links, ok := s.accounts.(providerLinkRepository)
	if !ok {
		return nil
	}
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return mapRepositoryError(err)
	}
	if value.Provider != accountdomain.ProviderWeb && value.Provider != accountdomain.ProviderConsole {
		return nil
	}
	// Session 身份只需成功补充一次；已有 user_id 或 email 时仅做本地关联协调，
	// 不再重复访问上游。没有任何身份数据的失败账号会在后续同步时重试。
	if strings.TrimSpace(value.UserID) != "" || strings.TrimSpace(value.Email) != "" {
		return mapRepositoryError(links.ReconcileProviderLinks(ctx, id))
	}
	if s.providers == nil {
		return fmt.Errorf("Provider 注册表未初始化")
	}
	adapter, ok := s.providers.AccountIdentity(value.Provider)
	if !ok {
		return nil
	}
	identity, err := adapter.SyncAccountIdentity(ctx, value)
	if err != nil {
		if errors.Is(err, provider.ErrUnauthorized) {
			markErr := s.markSSOCredentialRejected(ctx, value, fmt.Sprintf("%s SSO credential rejected", value.Provider))
			return errors.Join(err, markErr)
		}
		return err
	}
	if len(identity.Email) > 255 || len(identity.UserID) > 255 || len(identity.TeamID) > 255 {
		return fmt.Errorf("Grok Web Session 身份字段超过安全上限")
	}
	if err := links.UpdateIdentityMetadata(ctx, id, identity.Email, identity.UserID, identity.TeamID); err != nil {
		return mapRepositoryError(err)
	}
	return mapRepositoryError(links.ReconcileProviderLinks(ctx, id))
}

func (s *Service) reconcileProviderLinksBestEffort(ctx context.Context, id uint64) {
	links, ok := s.accounts.(providerLinkRepository)
	if !ok {
		return
	}
	if err := links.ReconcileProviderLinks(ctx, id); err != nil {
		s.logger.Warn("account_provider_link_reconcile_failed", "account_id", id, "error", err)
	}
}

func (s *Service) syncAccountIdentityBestEffort(ctx context.Context, id uint64) error {
	if err := s.SyncAccountIdentity(ctx, id); err != nil {
		s.logger.Warn("account_identity_sync_failed", "account_id", id, "error", err)
		return err
	}
	return nil
}
