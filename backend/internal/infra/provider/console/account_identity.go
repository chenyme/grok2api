package console

import (
	"context"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/sessionidentity"
)

// SyncAccountIdentity 通过 Grok Web Session 接口补充 Console SSO 身份。
// 请求仍使用该 Console 凭据对应的代理、Cookie 与 Resin 身份。
func (a *Adapter) SyncAccountIdentity(ctx context.Context, credential account.Credential) (provider.AccountIdentity, error) {
	if credential.Provider != account.ProviderConsole || credential.AuthType != account.AuthTypeSSO {
		return provider.AccountIdentity{}, fmt.Errorf("仅 Grok Console SSO 账号支持身份同步")
	}
	return sessionidentity.Fetch(ctx, a.config().SessionBaseURL, credential, a.egress, a.cipher)
}

var _ provider.AccountIdentityAdapter = (*Adapter)(nil)
