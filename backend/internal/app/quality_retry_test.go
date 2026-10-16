package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestDegradedAccountSwitchAppliesAndRestoresRequestRetryPolicy(t *testing.T) {
	for _, retryEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("yaml_retry_%t", retryEnabled), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(`secrets:
  jwtSecret: "12345678901234567890123456789012"
  credentialEncryptionKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
`), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.QualityGuard.RequestRetry.Enabled = retryEnabled
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "settings.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			cipher, err := security.NewCipher(cfg.Secrets.CredentialEncryptionKey)
			if err != nil {
				t.Fatal(err)
			}
			repo := relational.NewRuntimeSettingsRepository(db, cipher)
			var applied gateway.QualityRetryRuntime
			service := settingsapp.NewService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) {
				applied = qualityRetryRuntime(next.QualityGuard)
				if next.QualityGuard.RequestRetry.Enabled != retryEnabled {
					t.Fatal("management switch changed the YAML retry policy")
				}
			})
			for _, autoDisable := range []bool{true, false} {
				snapshot := service.Get()
				input := snapshot.Config
				input.Accounts.AutoDisableDegradedAccounts = autoDisable
				input.Accounts.AutoDisableDegradedAccountsProvided = true
				if _, err := service.Update(ctx, snapshot.Revision, input); err != nil {
					t.Fatal(err)
				}
				wantEnabled := retryEnabled || autoDisable
				if applied.Enabled != wantEnabled || applied.AutoDisable != autoDisable {
					t.Fatalf("hot reload: autoDisable=%t runtime=%#v", autoDisable, applied)
				}
				reloaded, _, _, err := settingsapp.LoadPersisted(ctx, cfg, repo)
				if err != nil {
					t.Fatal(err)
				}
				runtime := qualityRetryRuntime(reloaded.QualityGuard)
				if runtime.Enabled != wantEnabled || runtime.AutoDisable != autoDisable || reloaded.QualityGuard.RequestRetry.Enabled != retryEnabled {
					t.Fatalf("restart: autoDisable=%t runtime=%#v", autoDisable, runtime)
				}
			}
		})
	}
}
