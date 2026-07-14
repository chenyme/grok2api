package account

import (
	"context"
	"errors"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestPaidQuotaCreatesPeriodProbeAndClearsAfterRecovery(t *testing.T) {
	now := time.Now().UTC()
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	periodEnd := now.Add(time.Hour).Format(time.RFC3339)
	adapter.billing = accountdomain.Billing{MonthlyLimit: 100, Used: 100, BillingPeriodEnd: periodEnd}
	if _, err := service.RefreshBilling(context.Background(), credential.ID); err != nil {
		t.Fatal(err)
	}
	recovery, err := service.accounts.GetQuotaRecovery(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Kind != accountdomain.QuotaRecoveryKindPaid || recovery.Status != accountdomain.QuotaRecoveryStatusExhausted || recovery.NextProbeAt == nil || recovery.NextProbeAt.Format(time.RFC3339) != periodEnd {
		t.Fatalf("recovery = %#v", recovery)
	}

	adapter.billing = accountdomain.Billing{MonthlyLimit: 100, Used: 0, BillingPeriodEnd: now.Add(31 * 24 * time.Hour).Format(time.RFC3339)}
	recovered, err := service.ProbePaidQuota(context.Background(), credential)
	if err != nil || !recovered {
		t.Fatalf("recovered = %v, err = %v", recovered, err)
	}
	if _, err := service.accounts.GetQuotaRecovery(context.Background(), credential.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("paid recovery state should be cleared, err = %v", err)
	}
	if adapter.billingCount.Load() != 2 {
		t.Fatalf("billing probes = %d", adapter.billingCount.Load())
	}
}

func TestPaidQuotaStillExhaustedBacksOffAfterDueProbe(t *testing.T) {
	now := time.Now().UTC()
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	adapter.billing = accountdomain.Billing{MonthlyLimit: 100, Used: 100, BillingPeriodEnd: now.Add(-time.Minute).Format(time.RFC3339)}
	recovered, err := service.ProbePaidQuota(context.Background(), credential)
	if err != nil || recovered {
		t.Fatalf("recovered = %v, err = %v", recovered, err)
	}
	recovery, err := service.accounts.GetQuotaRecovery(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.NextProbeAt == nil || recovery.NextProbeAt.Before(time.Now().UTC().Add(14*time.Minute)) {
		t.Fatalf("next probe should be backed off, recovery = %#v", recovery)
	}
}
