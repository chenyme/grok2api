package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const (
	credentialImportProvider    = "grok_build"
	maxCredentialImportAccounts = 10000
)

type credentialImportDocument struct {
	Accounts []importedCredentialEntry `json:"accounts"`
}

type importedCredentialEntry struct {
	Provider     string `json:"provider"`
	Name         string `json:"name"`
	ClientID     string `json:"client_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresAt    string `json:"expires_at"`
	ExpiresIn    int64  `json:"expires_in"`
	Email        string `json:"email"`
	UserID       string `json:"user_id"`
	PrincipalID  string `json:"principal_id"`
	TeamID       string `json:"team_id"`
}

func marshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	document := credentialImportDocument{Accounts: make([]importedCredentialEntry, 0, len(values))}
	for _, value := range values {
		entry := importedCredentialEntry{
			Provider: credentialImportProvider, Name: value.Name, ClientID: value.OIDCClientID,
			AccessToken: value.AccessToken, RefreshToken: value.RefreshToken, TokenType: "Bearer",
			Email: value.Email, UserID: value.UserID, TeamID: value.TeamID,
		}
		if !value.ExpiresAt.IsZero() {
			entry.ExpiresAt = value.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		document.Accounts = append(document.Accounts, entry)
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化账号凭据: %w", err)
	}
	return append(data, '\n'), nil
}

func parseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(data, &shape); err != nil {
		return nil, fmt.Errorf("解析账号凭据 JSON: %w", err)
	}

	var entries []importedCredentialEntry
	if _, batch := shape["accounts"]; batch {
		var document credentialImportDocument
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, fmt.Errorf("解析批量账号凭据: %w", err)
		}
		entries = document.Accounts
	} else {
		var entry importedCredentialEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			return nil, fmt.Errorf("解析 OAuth 凭据: %w", err)
		}
		entries = []importedCredentialEntry{entry}
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("账号凭据中没有账号")
	}
	if len(entries) > maxCredentialImportAccounts {
		return nil, fmt.Errorf("%w: 单次最多导入 %d 个账号", provider.ErrCredentialLimit, maxCredentialImportAccounts)
	}

	result := make([]provider.CredentialSeed, 0, len(entries))
	for index, entry := range entries {
		seed, err := normalizeImportedCredential(entry)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个账号: %w", index+1, err)
		}
		result = append(result, seed)
	}
	return result, nil
}

func normalizeImportedCredential(entry importedCredentialEntry) (provider.CredentialSeed, error) {
	providerName := strings.ToLower(strings.TrimSpace(entry.Provider))
	if providerName == "" {
		providerName = credentialImportProvider
	}
	if providerName != credentialImportProvider {
		return provider.CredentialSeed{}, fmt.Errorf("暂不支持 Provider %q", entry.Provider)
	}
	accessToken := strings.TrimSpace(entry.AccessToken)
	refreshToken := strings.TrimSpace(entry.RefreshToken)
	if accessToken == "" && refreshToken == "" {
		return provider.CredentialSeed{}, fmt.Errorf("access_token 和 refresh_token 至少提供一个")
	}
	if entry.TokenType != "" && !strings.EqualFold(strings.TrimSpace(entry.TokenType), "Bearer") {
		return provider.CredentialSeed{}, fmt.Errorf("暂不支持 token_type %q", entry.TokenType)
	}

	claims := decodeJWTClaims(firstNonEmpty(entry.IDToken, accessToken))
	userID := firstNonEmpty(entry.UserID, entry.PrincipalID, stringClaim(claims, "sub"))
	email := firstNonEmpty(entry.Email, stringClaim(claims, "email"))
	teamID := firstNonEmpty(entry.TeamID, stringClaim(claims, "team_id"))
	expiresAt, err := importedCredentialExpiry(entry, claims)
	if err != nil {
		return provider.CredentialSeed{}, err
	}
	clientID := firstNonEmpty(entry.ClientID, defaultOAuthClientID)
	identity := firstNonEmpty(userID, strings.ToLower(email), teamID, refreshToken, accessToken)
	sourceKey := "import:" + security.HashToken(strings.Join([]string{providerName, clientID, identity}, "|"))

	return provider.CredentialSeed{
		Name: firstNonEmpty(entry.Name, email, userID, "Grok Build account"), Email: email, UserID: userID, TeamID: teamID,
		SourceKey: sourceKey, OIDCClientID: clientID, AccessToken: accessToken, RefreshToken: refreshToken, ExpiresAt: expiresAt,
	}, nil
}

func importedCredentialExpiry(entry importedCredentialEntry, claims map[string]any) (time.Time, error) {
	if strings.TrimSpace(entry.ExpiresAt) != "" {
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(entry.ExpiresAt))
		if err != nil {
			return time.Time{}, fmt.Errorf("expires_at 必须是 RFC3339 时间: %w", err)
		}
		return parsed.UTC(), nil
	}
	if expiresAt, ok := numericDateClaim(claims, "exp"); ok {
		return expiresAt, nil
	}
	if entry.ExpiresIn < 0 {
		return time.Time{}, fmt.Errorf("expires_in 不能小于零")
	}
	if entry.ExpiresIn > int64((365*24*time.Hour)/time.Second) {
		return time.Time{}, fmt.Errorf("expires_in 超出合理范围")
	}
	if entry.ExpiresIn > 0 {
		return time.Now().UTC().Add(time.Duration(entry.ExpiresIn) * time.Second), nil
	}
	return time.Time{}, nil
}

func numericDateClaim(claims map[string]any, key string) (time.Time, bool) {
	value, ok := claims[key].(float64)
	if !ok || value <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(value), 0).UTC(), true
}

func decodeJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

func stringClaim(claims map[string]any, key string) string {
	value, _ := claims[key].(string)
	return value
}
