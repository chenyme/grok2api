package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func BenchmarkSelectorMultiModelCandidateLoad(b *testing.B) {
	const accountCount = 300
	models := []string{
		"benchmark-model-1", "benchmark-model-2", "benchmark-model-3", "benchmark-model-4",
		"benchmark-model-5", "benchmark-model-6", "benchmark-model-7", "benchmark-model-8",
	}
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "selector-layered-benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	routes := relational.NewModelRepository(database)
	credentials := make([]account.Credential, accountCount)
	for index := range credentials {
		credentials[index] = account.Credential{
			Provider: account.ProviderBuild, Name: fmt.Sprintf("benchmark-%04d", index),
			SourceKey: fmt.Sprintf("benchmark-source-%04d", index), EncryptedAccessToken: "encrypted",
			AuthStatus: account.AuthStatusActive, Priority: account.DefaultPriority, MaxConcurrent: account.DefaultMaxConcurrent,
		}
	}
	created, err := accounts.UpsertManyByIdentity(ctx, credentials)
	if err != nil {
		b.Fatal(err)
	}
	syncedAt := time.Now().UTC()
	for _, value := range created {
		if err := routes.ReplaceAccountCapabilities(ctx, value.ID, models, syncedAt); err != nil {
			b.Fatal(err)
		}
	}

	for _, modelCount := range []int{2, 8} {
		b.Run(fmt.Sprintf("models_%d", modelCount), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				selector := NewSelector(accounts, nil, nil, nil, time.Hour, time.Second, time.Minute)
				for _, upstreamModel := range models[:modelCount] {
					candidates, loadErr := selector.loadCandidates(ctx, account.ProviderBuild, upstreamModel, "", time.Now().UTC())
					if loadErr != nil {
						b.Fatal(loadErr)
					}
					if len(candidates) != accountCount {
						b.Fatalf("candidates = %d, want %d", len(candidates), accountCount)
					}
				}
			}
		})
	}
}
