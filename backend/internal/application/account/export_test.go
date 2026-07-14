package account

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	cliprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestExportCredentialsRoundTripsImportFormat(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accessToken, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	refreshToken, err := cipher.Encrypt("refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	repository := relational.NewAccountRepository(database)
	if _, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "primary", Email: "user@example.com", UserID: "user-1",
		SourceKey: "export-test", OIDCClientID: "client-1", EncryptedAccessToken: accessToken,
		EncryptedRefreshToken: refreshToken, ExpiresAt: expiresAt, Enabled: false,
		AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 8,
	}); err != nil {
		t.Fatal(err)
	}
	adapter := cliprovider.NewAdapter(cliprovider.Config{}, cipher)
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)

	result, err := service.ExportCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	values, err := adapter.ParseImportedCredentials(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || len(values) != 1 {
		t.Fatalf("export count = %d, imported values = %d", result.Count, len(values))
	}
	value := values[0]
	if value.Name != "primary" || value.Email != "user@example.com" || value.UserID != "user-1" || value.OIDCClientID != "client-1" || value.AccessToken != "access-token" || value.RefreshToken != "refresh-token" || !value.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("round-trip credential = %#v", value)
	}
	progress := make([][2]int, 0, 2)
	if _, err := service.ImportCredentialsWithProgress(ctx, result.Data, nil, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(progress) != 2 || progress[0] != [2]int{0, 1} || progress[1] != [2]int{1, 1} {
		t.Fatalf("import progress = %#v", progress)
	}

	multiProgress := make([][2]int, 0, 3)
	multiResult, err := service.ImportCredentialDocumentsWithProgress(ctx, [][]byte{
		result.Data,
		result.Data,
		[]byte(`{"provider":"grok_build","name":"secondary","access_token":"second-access","refresh_token":"second-refresh","user_id":"user-2"}`),
	}, nil, func(completed, total int) error {
		multiProgress = append(multiProgress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if multiResult.Created != 1 || multiResult.Updated != 1 {
		t.Fatalf("multi-file import result = %#v", multiResult)
	}
	if len(multiProgress) != 3 || multiProgress[0] != [2]int{0, 2} || multiProgress[2] != [2]int{2, 2} {
		t.Fatalf("multi-file import progress = %#v", multiProgress)
	}
}
