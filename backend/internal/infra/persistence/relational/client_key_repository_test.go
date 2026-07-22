package relational

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
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

func TestClientKeyBillingReservationSkipsExpiryCleanupWithoutPressure(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "limited", Prefix: "cleanup-pressure", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_cleanup_pressure_expired", 20, now.Add(-time.Minute)); err != nil || !reserved {
		t.Fatalf("expired reserve: reserved=%v, err=%v", reserved, err)
	}
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_cleanup_pressure_active_1", 30, now.Add(time.Hour)); err != nil || !reserved {
		t.Fatalf("reserve with remaining capacity: reserved=%v, err=%v", reserved, err)
	}
	var reservations int64
	if err := database.db.WithContext(ctx).Model(&billingReservationModel{}).Where("client_key_id = ?", key.ID).Count(&reservations).Error; err != nil {
		t.Fatal(err)
	}
	if reservations != 2 {
		t.Fatalf("reservation count = %d, want 2", reservations)
	}
	stored, err := keys.Get(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ReservedUsageUSDTicks != 50 {
		t.Fatalf("reserved usage = %d, want 50", stored.ReservedUsageUSDTicks)
	}
	cleaned, err := keys.CleanupExpiredBillingReservations(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned != 1 {
		t.Fatalf("cleaned = %d, want 1", cleaned)
	}
	stored, err = keys.Get(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ReservedUsageUSDTicks != 30 {
		t.Fatalf("reserved usage after cleanup = %d, want 30", stored.ReservedUsageUSDTicks)
	}
}

func TestClientKeyBillingReservationRecreatesExpiredEvent(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "limited", Prefix: "renew-expired", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const eventID = "evt_renew_expired_reservation"
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, eventID, 40, now.Add(-time.Minute)); err != nil || !reserved {
		t.Fatalf("expired reserve: reserved=%v, err=%v", reserved, err)
	}
	renewedUntil := now.Add(time.Hour)
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, eventID, 60, renewedUntil); err != nil || !reserved {
		t.Fatalf("recreate reserve: reserved=%v, err=%v", reserved, err)
	}
	var reservation billingReservationModel
	if err := database.db.WithContext(ctx).Where("event_id = ?", eventID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if !reservation.ExpiresAt.Equal(renewedUntil) {
		t.Fatalf("expires at = %s, want %s", reservation.ExpiresAt, renewedUntil)
	}
	stored, err := keys.Get(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ReservedUsageUSDTicks != 60 {
		t.Fatalf("reserved usage = %d, want 60", stored.ReservedUsageUSDTicks)
	}
}

func TestClientKeyBillingReservationCannotMoveExpiredEventBetweenKeys(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	keys := NewClientKeyRepository(database)
	first, err := keys.Create(ctx, clientkeydomain.Key{Name: "first", Prefix: "expired-owner-a", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	second, err := keys.Create(ctx, clientkeydomain.Key{Name: "second", Prefix: "expired-owner-b", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	const eventID = "evt_expired_reservation_owner"
	if reserved, err := keys.ReserveBillingUsage(ctx, first.ID, eventID, 40, time.Now().UTC().Add(-time.Minute)); err != nil || !reserved {
		t.Fatalf("expired reserve: reserved=%v, err=%v", reserved, err)
	}
	if _, err := keys.ReserveBillingUsage(ctx, second.ID, eventID, 40, time.Now().UTC().Add(time.Hour)); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("cross-key reserve error = %v, want conflict", err)
	}
	stored, err := keys.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ReservedUsageUSDTicks != 40 {
		t.Fatalf("first key reserved usage = %d, want 40", stored.ReservedUsageUSDTicks)
	}
}

func TestClientKeyBillingReservationsDoNotExceedLimitConcurrently(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "limited", Prefix: "concurrent-reserve", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 20
	start := make(chan struct{})
	errorsCh := make(chan error, workers)
	var successes atomic.Int64
	var wait sync.WaitGroup
	wait.Add(workers)
	for index := range workers {
		go func() {
			defer wait.Done()
			<-start
			reserved, reserveErr := keys.ReserveBillingUsage(ctx, key.ID, fmt.Sprintf("evt_concurrent_reservation_%04d", index), 10, time.Now().UTC().Add(time.Hour))
			switch {
			case reserveErr == nil && reserved:
				successes.Add(1)
			case errors.Is(reserveErr, repository.ErrLimitExceeded):
			default:
				errorsCh <- fmt.Errorf("worker %d: reserved=%v, err=%w", index, reserved, reserveErr)
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errorsCh)
	for reserveErr := range errorsCh {
		t.Error(reserveErr)
	}
	if t.Failed() {
		return
	}
	if successes.Load() != 10 {
		t.Fatalf("successful reservations = %d, want 10", successes.Load())
	}
	stored, err := keys.Get(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ReservedUsageUSDTicks != 100 {
		t.Fatalf("reserved usage = %d, want 100", stored.ReservedUsageUSDTicks)
	}
}

func TestCleanupExpiredBillingReservationsProtectsPendingMediaUsage(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "media", Prefix: "media-cleanup", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	type mediaCase struct {
		id       string
		status   string
		recorded bool
	}
	cases := []mediaCase{
		{id: "video_cleanup_queued", status: "queued"},
		{id: "video_cleanup_running", status: "in_progress"},
		{id: "video_cleanup_completed", status: "completed"},
		{id: "video_cleanup_recorded", status: "completed", recorded: true},
	}
	for index, test := range cases {
		eventID := "video_usage_" + test.id
		if reserved, reserveErr := keys.ReserveBillingUsage(ctx, key.ID, eventID, 100, now.Add(-time.Minute)); reserveErr != nil || !reserved {
			t.Fatalf("reserve %s: reserved=%v err=%v", eventID, reserved, reserveErr)
		}
		var usageRecordedAt *time.Time
		if test.recorded {
			value := now.Add(-time.Second)
			usageRecordedAt = &value
		}
		job := mediaJobModel{
			ID: test.id, RequestID: fmt.Sprintf("media-cleanup-%d", index), ClientKeyID: key.ID,
			Provider: "grok_web", Model: "video", ModelRouteID: 1, UpstreamModel: "video", Prompt: "test",
			Seconds: 6, Size: "16:9", Quality: "720p", Status: test.status, InputJSON: "{}",
			CreatedAt: now, UpdatedAt: now, UsageRecordedAt: usageRecordedAt,
		}
		if err := database.db.WithContext(ctx).Create(&job).Error; err != nil {
			t.Fatal(err)
		}
	}
	cleaned, err := keys.CleanupExpiredBillingReservations(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned != 1 {
		t.Fatalf("cleaned = %d, want only recorded terminal media reservation", cleaned)
	}
	var remaining []billingReservationModel
	if err := database.db.WithContext(ctx).Order("event_id ASC").Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 3 {
		t.Fatalf("remaining reservations = %#v", remaining)
	}
}

func TestCleanupExpiredBillingReservationsLimitsActualRows(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "batch", Prefix: "cleanup-row-limit", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for index := range 5 {
		if reserved, reserveErr := keys.ReserveBillingUsage(ctx, key.ID, fmt.Sprintf("evt_cleanup_row_%02d", index), 10, now.Add(-time.Minute)); reserveErr != nil || !reserved {
			t.Fatalf("reserve %d: reserved=%v err=%v", index, reserved, reserveErr)
		}
	}
	cleaned, err := keys.CleanupExpiredBillingReservations(ctx, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned != 2 {
		t.Fatalf("cleaned = %d, want 2", cleaned)
	}
	var remaining int64
	if err := database.db.WithContext(ctx).Model(&billingReservationModel{}).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 3 {
		t.Fatalf("remaining = %d, want 3", remaining)
	}
}

func TestBillingSettlementRacesExpiredCleanupWithoutLosingUsage(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	keys := NewClientKeyRepository(database)
	audits := NewAuditRepository(database)
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "race", Prefix: "settlement-cleanup", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	const iterations = 20
	for index := range iterations {
		now := time.Now().UTC()
		eventID := fmt.Sprintf("evt_settlement_cleanup_%02d", index)
		if reserved, reserveErr := keys.ReserveBillingUsage(ctx, key.ID, eventID, 10, now.Add(-time.Minute)); reserveErr != nil || !reserved {
			t.Fatalf("reserve %d: reserved=%v err=%v", index, reserved, reserveErr)
		}
		start := make(chan struct{})
		errorsCh := make(chan error, 2)
		go func() {
			<-start
			errorsCh <- audits.Create(ctx, auditdomain.Record{
				EventID: eventID, RequestID: eventID, ClientKeyID: key.ID, ModelRouteID: 1,
				Provider: "grok_build", Operation: auditdomain.OperationResponses, UsageSource: auditdomain.UsageSourceUpstream,
				StatusCode: 200, CostInUSDTicks: 10, CreatedAt: now,
			})
		}()
		go func() {
			<-start
			_, cleanupErr := keys.CleanupExpiredBillingReservations(ctx, now, 1)
			errorsCh <- cleanupErr
		}()
		close(start)
		for range 2 {
			if concurrentErr := <-errorsCh; concurrentErr != nil {
				t.Fatalf("iteration %d: %v", index, concurrentErr)
			}
		}
	}
	stored, err := keys.Get(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BilledUsageUSDTicks != iterations*10 || stored.ReservedUsageUSDTicks != 0 {
		t.Fatalf("billing state = %#v", stored)
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
