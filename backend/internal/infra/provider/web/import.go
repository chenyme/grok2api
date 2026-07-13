package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const (
	maxImportAccounts = 10000
	maxSSOTokenBytes  = 16 << 10
)

type importDocument struct {
	Provider string        `json:"provider"`
	Accounts []importEntry `json:"accounts"`
}

type importEntry struct {
	Name     string `json:"name"`
	SSOToken string `json:"sso_token"`
	Token    string `json:"token"`
	Tier     string `json:"tier"`
}

func (a *Adapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("账号文件中没有 Grok Web 账号")
	}
	if !strings.HasPrefix(trimmed, "{") {
		return parsePlainTextCredentials(trimmed)
	}
	var document importDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("解析 Grok Web 账号 JSON: %w", err)
	}
	if document.Provider != "" && document.Provider != string(account.ProviderWeb) {
		return nil, fmt.Errorf("账号文件 Provider 必须是 %s", account.ProviderWeb)
	}
	entries := document.Accounts
	if len(entries) == 0 {
		return nil, fmt.Errorf("账号文件中没有 Grok Web 账号")
	}
	if len(entries) > maxImportAccounts {
		return nil, provider.ErrCredentialLimit
	}
	seen := make(map[string]struct{}, len(entries))
	result := make([]provider.CredentialSeed, 0, len(entries))
	for index, entry := range entries {
		token := sanitizeSSOToken(firstNonEmpty(entry.SSOToken, entry.Token))
		if token == "" {
			return nil, fmt.Errorf("第 %d 个账号缺少 sso_token", index+1)
		}
		if len(token) > maxSSOTokenBytes {
			return nil, fmt.Errorf("第 %d 个账号的 sso_token 超过 16 KiB", index+1)
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		tier := account.WebTier(strings.ToLower(strings.TrimSpace(entry.Tier)))
		if tier == "" {
			tier = account.WebTierAuto
		}
		if tier != account.WebTierAuto && tier != account.WebTierBasic && tier != account.WebTierSuper && tier != account.WebTierHeavy {
			return nil, fmt.Errorf("第 %d 个账号 tier 无效", index+1)
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = fmt.Sprintf("Grok Web %s", security.HashToken(token)[:8])
		}
		result = append(result, provider.CredentialSeed{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: tier,
			Name: name, SourceKey: "sso:" + security.HashToken(token), AccessToken: token,
		})
	}
	return result, nil
}

func parsePlainTextCredentials(value string) ([]provider.CredentialSeed, error) {
	lines := strings.Split(value, "\n")
	seen := make(map[string]struct{}, len(lines))
	result := make([]provider.CredentialSeed, 0, len(lines))
	for index, line := range lines {
		token := sanitizeSSOToken(line)
		if token == "" {
			continue
		}
		if len(token) > maxSSOTokenBytes {
			return nil, fmt.Errorf("第 %d 行的 sso token 超过 16 KiB", index+1)
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		result = append(result, provider.CredentialSeed{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierAuto,
			Name: "Grok Web " + security.HashToken(token)[:8], SourceKey: "sso:" + security.HashToken(token), AccessToken: token,
		})
		if len(result) > maxImportAccounts {
			return nil, provider.ErrCredentialLimit
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("文本中没有有效的 sso token")
	}
	return result, nil
}

func (a *Adapter) MarshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	document := importDocument{Provider: string(account.ProviderWeb), Accounts: make([]importEntry, 0, len(values))}
	for _, value := range values {
		document.Accounts = append(document.Accounts, importEntry{Name: value.Name, SSOToken: value.AccessToken, Tier: string(value.WebTier)})
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func sanitizeSSOToken(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "sso=") {
		value = strings.TrimSpace(value[len("sso="):])
	}
	if token, _, found := strings.Cut(value, ";"); found {
		value = token
	}
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
