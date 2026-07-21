package gateway

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

const (
	finalizationHealthBudget    = 750 * time.Millisecond
	finalizationOwnershipBudget = 1500 * time.Millisecond
	finalizationQuotaBudget     = time.Second
	finalizationAuditBudget     = 3 * time.Second
	finalizationMetadataBudget  = 500 * time.Millisecond
)

type finalizationBudget struct {
	deadline  time.Time
	operation string
	provider  string
}

func newFinalizationBudget(operation, provider string) finalizationBudget {
	return finalizationBudget{
		deadline:  time.Now().Add(finalizationTimeout),
		operation: operation,
		provider:  provider,
	}
}

func (b finalizationBudget) run(stage string, limit time.Duration, action func(context.Context) error) error {
	started := time.Now()
	deadline := b.deadline
	if candidate := started.Add(limit); candidate.Before(deadline) {
		deadline = candidate
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	err := action(ctx)
	cancel()
	outcome := "success"
	if err != nil {
		outcome = "failed"
	}
	perfmetrics.Default.ObserveDuration("finalization_stage_duration_us", perfmetrics.Labels{
		Subsystem: "gateway", Operation: b.operation, Provider: b.provider, Stage: stage, Outcome: outcome,
	}, time.Since(started))
	return err
}
