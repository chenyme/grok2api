package relational

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestClientKeyBillingReservationsEnforceLimitAndExpire(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "client-key-reservations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "limited", Prefix: "limited", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8, BillingLimitUSDTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_reservation_limit_0001", 60, now.Add(time.Hour)); err != nil || !reserved {
		t.Fatal(err)
	}
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_reservation_limit_0001", 60, now.Add(time.Hour)); err != nil || !reserved {
		t.Fatalf("idempotent reserve: %v", err)
	}
	if _, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_reservation_limit_0002", 50, now.Add(time.Hour)); !errors.Is(err, repository.ErrLimitExceeded) {
		t.Fatalf("limit error = %v", err)
	}
	if err := keys.CancelBillingReservation(ctx, "evt_reservation_limit_0001"); err != nil {
		t.Fatal(err)
	}
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_reservation_expired_0001", 80, now.Add(-time.Minute)); err != nil || !reserved {
		t.Fatal(err)
	}
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_reservation_after_expiry_0001", 100, now.Add(time.Hour)); err != nil || !reserved {
		t.Fatalf("reserve after expiry cleanup: %v", err)
	}
	stored, err := keys.Get(ctx, key.ID)
	if err != nil || stored.ReservedUsageUSDTicks != 100 {
		t.Fatalf("stored = %#v, err = %v", stored, err)
	}
}

func TestClientKeyUpdateDoesNotOverwriteConcurrentBillingState(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "before", Prefix: "concurrent", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8, BillingLimitUSDTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	stale := key
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_update_preserves_reservation_0001", 40, time.Now().UTC().Add(time.Hour)); err != nil || !reserved {
		t.Fatal(err)
	}
	stale.Name = "after"
	updated, err := keys.Update(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "after" || updated.ReservedUsageUSDTicks != 40 {
		t.Fatalf("updated = %#v", updated)
	}
}
