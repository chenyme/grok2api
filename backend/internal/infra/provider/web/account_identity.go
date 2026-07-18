package web

import (
	"context"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/sessionidentity"
)

// SyncAccountIdentity 从 Grok Web 会话读取非敏感身份元数据。
// 请求复用账号的 Web 出口、浏览器指纹、Cookie 与 Resin 身份。
func (a *Adapter) SyncAccountIdentity(ctx context.Context, credential account.Credential) (provider.AccountIdentity, error) {
	if credential.Provider != account.ProviderWeb || credential.AuthType != account.AuthTypeSSO {
		return provider.AccountIdentity{}, fmt.Errorf("仅 Grok Web SSO 账号支持身份同步")
	}
	return sessionidentity.Fetch(ctx, a.config().BaseURL, credential, a.egress, a.cipher)
}

func parseAccountIdentity(body []byte) (provider.AccountIdentity, error) {
	return sessionidentity.Parse(body)
}

var _ provider.AccountIdentityAdapter = (*Adapter)(nil)
