package relational

import (
	"context"
	"encoding/hex"
	"strings"
)

const (
	webSSOSourcePrefix    = "sso:"
	stableSSODigestLength = 64
	egressDigestLength    = 32
)

// backfillWebEgressIdentities 只使用已持久化的 SSO 摘要恢复现有 Resin 身份，
// 不解密凭据、不访问上游，也不会建立新的跨 Provider 关系。
func (d *Database) backfillWebEgressIdentities(ctx context.Context) error {
	var rows []struct {
		AccountID uint64
		SourceKey string
	}
	db := d.db.WithContext(ctx)
	if err := db.Table("provider_accounts AS account").
		Select("account.id AS account_id, account.source_key").
		Joins("JOIN web_account_profiles AS profile ON profile.account_id = account.id").
		Where("account.provider = ? AND profile.egress_identity = ''", "grok_web").
		Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		identity, ok := egressIdentityFromWebSourceKey(row.SourceKey)
		if !ok {
			continue
		}
		if err := db.Model(&webAccountProfileModel{}).
			Where("account_id = ? AND egress_identity = ''", row.AccountID).
			Update("egress_identity", identity).Error; err != nil {
			return err
		}
	}
	return nil
}

func egressIdentityFromWebSourceKey(sourceKey string) (string, bool) {
	digest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sourceKey), webSSOSourcePrefix))
	if !strings.HasPrefix(strings.TrimSpace(sourceKey), webSSOSourcePrefix) || len(digest) != stableSSODigestLength {
		return "", false
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", false
	}
	return "sso_" + strings.ToLower(digest[:egressDigestLength]), true
}
