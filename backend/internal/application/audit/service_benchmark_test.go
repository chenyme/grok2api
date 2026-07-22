package audit

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func BenchmarkAuditServiceSQLite(b *testing.B) {
	for _, attemptCount := range []int{0, 2} {
		b.Run(fmt.Sprintf("attempts-%d", attemptCount), func(b *testing.B) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "audit-benchmark.db"))
			if err != nil {
				b.Fatal(err)
			}
			if err := database.InitializeSchema(ctx); err != nil {
				database.Close()
				b.Fatal(err)
			}
			service := NewService(relational.NewAuditRepository(database), slog.Default(), 16_384, 256, 250*time.Millisecond)
			service.Start()

			var sequence atomic.Uint64
			errCh := make(chan error, 1)
			baseTime := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
			b.SetParallelism(4)
			b.ResetTimer()
			b.RunParallel(func(worker *testing.PB) {
				for worker.Next() {
					index := sequence.Add(1)
					record := auditdomain.Record{
						EventID: fmt.Sprintf("evt_benchmark_%020d", index), RequestID: fmt.Sprintf("benchmark-%d", index),
						ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: baseTime.Add(time.Duration(index)),
					}
					for attempt := 1; attempt <= attemptCount; attempt++ {
						record.Attempts = append(record.Attempts, auditdomain.Attempt{
							Number: attempt, Source: auditdomain.AttemptSourceCredential, Stage: "credential", StartedAt: record.CreatedAt,
						})
					}
					if err := service.Create(context.Background(), record); err != nil {
						select {
						case errCh <- err:
						default:
						}
					}
				}
			})
			b.StopTimer()
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := service.Close(closeCtx); err != nil {
				cancel()
				database.Close()
				b.Fatal(err)
			}
			cancel()
			if err := database.Close(); err != nil {
				b.Fatal(err)
			}
			select {
			case err := <-errCh:
				b.Fatal(err)
			default:
			}
		})
	}
}
