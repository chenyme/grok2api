package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
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
	Name              string `json:"name"`
	Email             string `json:"email,omitempty"`
	UserID            string `json:"user_id,omitempty"`
	SSOToken          string `json:"sso_token"`
	Token             string `json:"token"`
	CloudflareCookies string `json:"cloudflare_cookies"`
	// Optional caches; both are rebuilt from the SSO when absent or stale.
	APIKeyID           string `json:"api_key_id,omitempty"`
	XAPIKey            string `json:"x_apikey,omitempty"`
	WorkOSSessionToken string `json:"workos_session_token,omitempty"`
	WorkOSXAIOrigin    string `json:"workos_xai_origin,omitempty"`
	WorkOSID           string `json:"workos_id,omitempty"`
}

func parseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("账号文件中没有 Grok Console 账号")
	}
	// 「[」为 JSON 保留前缀：顶层裸数组走 JSON 解析，避免被当成纯文本 token 静默导入。
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return parsePlainTextCredentials(trimmed)
	}
	entries, err := provider.DecodeCredentialJSONEntries[importEntry](data, string(account.ProviderConsole), maxImportAccounts)
	if err != nil {
		return nil, fmt.Errorf("解析 Grok Console 账号 JSON: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("账号文件中没有 Grok Console 账号")
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
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = "Grok Console " + security.HashToken(token)[:8]
		}
		seed := credentialSeed(name, token)
		seed.Email = strings.TrimSpace(entry.Email)
		seed.UserID = strings.TrimSpace(entry.UserID)
		seed.CloudflareCookies = entry.CloudflareCookies
		applyConsoleCaches(&seed, firstNonEmpty(entry.APIKeyID, entry.XAPIKey), entry.WorkOSSessionToken, entry.WorkOSXAIOrigin, entry.WorkOSID)
		result = append(result, seed)
	}
	return result, nil
}

func parsePlainTextCredentials(value string) ([]provider.CredentialSeed, error) {
	lines := strings.Split(value, "\n")
	seen := make(map[string]struct{}, len(lines))
	result := make([]provider.CredentialSeed, 0, len(lines))
	for index, line := range lines {
		if strings.Contains(line, consoleFieldSeparator) {
			seed, err := parseSeparatedConsoleLine(line, index+1)
			if err != nil {
				return nil, err
			}
			if seed == nil {
				continue
			}
			if _, exists := seen[seed.AccessToken]; exists {
				continue
			}
			seen[seed.AccessToken] = struct{}{}
			result = append(result, *seed)
			if len(result) > maxImportAccounts {
				return nil, provider.ErrCredentialLimit
			}
			continue
		}
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
		result = append(result, credentialSeed("Grok Console "+security.HashToken(token)[:8], token))
		if len(result) > maxImportAccounts {
			return nil, provider.ErrCredentialLimit
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("文本中没有有效的 sso token")
	}
	return result, nil
}

// consoleFieldSeparator delimits the registration-tool line format:
//
//	email----password----sso[----workosSession----workosXaiOrigin----workosId[----x-apikey]]
//
// The password is never read or stored: nothing on the Console path uses it.
const consoleFieldSeparator = "----"

func parseSeparatedConsoleLine(line string, lineNumber int) (*provider.CredentialSeed, error) {
	fields := strings.Split(strings.TrimSpace(line), consoleFieldSeparator)
	for index := range fields {
		fields[index] = strings.TrimSpace(fields[index])
	}
	if len(fields) < 3 {
		return nil, fmt.Errorf("第 %d 行格式无效：应为 邮箱----密码----sso[----workosSession----workosXaiOrigin----workosId----x-apikey]", lineNumber)
	}
	token := sanitizeSSOToken(fields[2])
	if token == "" {
		return nil, fmt.Errorf("第 %d 行缺少 sso", lineNumber)
	}
	if len(token) > maxSSOTokenBytes {
		return nil, fmt.Errorf("第 %d 行的 sso token 超过 16 KiB", lineNumber)
	}
	field := func(index int) string {
		if index < len(fields) {
			return fields[index]
		}
		return ""
	}
	email := field(0)
	name := email
	if name == "" {
		name = "Grok Console " + security.HashToken(token)[:8]
	}
	seed := credentialSeed(name, token)
	seed.Email = email
	applyConsoleCaches(&seed, field(6), field(3), field(4), field(5))
	return &seed, nil
}

var consoleImportAPIKeyPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// applyConsoleCaches accepts only well-formed caches: an apiKeyId UUID and a
// complete WorkOS triple. Anything else is dropped and re-derived from the SSO.
func applyConsoleCaches(seed *provider.CredentialSeed, apiKeyID, sessionToken, xaiOrigin, workosID string) {
	if apiKeyID = strings.TrimSpace(apiKeyID); consoleImportAPIKeyPattern.MatchString(apiKeyID) {
		seed.ConsoleAPIKeyID = strings.ToLower(apiKeyID)
	}
	session := workosSession{SessionToken: strings.TrimSpace(sessionToken), XAIOrigin: strings.TrimSpace(xaiOrigin), WorkOSID: strings.TrimSpace(workosID)}
	if !session.complete() {
		return
	}
	if data, err := json.Marshal(session); err == nil && len(data) <= workosSessionMaxBytes {
		seed.ConsoleWorkOSSession = string(data)
	}
}

func credentialSeed(name, token string) provider.CredentialSeed {
	return provider.CredentialSeed{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: name,
		SourceKey: "console-sso:" + security.HashToken(token), AccessToken: token,
	}
}

func marshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	document := importDocument{Provider: string(account.ProviderConsole), Accounts: make([]importEntry, 0, len(values))}
	for _, value := range values {
		document.Accounts = append(document.Accounts, importEntry{Name: value.Name, Email: value.Email, UserID: value.UserID, SSOToken: value.AccessToken, CloudflareCookies: value.CloudflareCookies})
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
