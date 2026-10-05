package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

type billingPoCVoiceConn struct{}

func (*billingPoCVoiceConn) ReadMessage() (int, []byte, error) { return 0, nil, context.Canceled }
func (*billingPoCVoiceConn) WriteMessage(int, []byte) error    { return nil }
func (*billingPoCVoiceConn) SetReadLimit(int64)                {}
func (*billingPoCVoiceConn) Close() error                      { return nil }

type billingPoCVoiceAdapter struct{ conn *billingPoCVoiceConn }

func (*billingPoCVoiceAdapter) Provider() account.Provider { return account.ProviderConsole }
func (*billingPoCVoiceAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: account.ProviderConsole, Media: provider.MediaSurface{STT: true, Realtime: true}}
}
func (a *billingPoCVoiceAdapter) DialVoiceWebSocket(context.Context, provider.VoiceWebSocketRequest) (provider.VoiceWebSocketConn, func(), error) {
	return a.conn, func() {}, nil
}

func buildBillingTestService(t *testing.T) (*Service, *relational.ClientKeyRepository, clientkey.Key) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "voice-billing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO,
		Name: "billing-console", SourceKey: "billing-console", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	const model = "billing-stt"
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{PublicID: model, Provider: account.ProviderConsole, UpstreamModel: model, Capability: modeldomain.CapabilitySTT, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{Name: "test-key", Prefix: "test-key", SecretHash: "0000000000000000000000000000000000000000000000000000000000000000", EncryptedSecret: "encrypted", Enabled: true, BillingLimitUSDTicks: 10_000_000})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &billingPoCVoiceAdapter{conn: &billingPoCVoiceConn{}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	accounts := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	keys := clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil)
	return NewService(modelRepo, auditRepo, accounts, keys, registry, selector, nil, 1), keyRepo, key
}

// A client disconnecting after a confirmed transcript.done duration must
// still be billed for it, the same as a clean close would be: the cost was
// already incurred upstream the moment that duration was received, and a
// client-side transport outcome afterward does not undo it.
func TestVoiceWSInterruptedAfterConfirmedDurationStillBills(t *testing.T) {
	service, keyRepo, key := buildBillingTestService(t)
	session, err := service.OpenVoiceWebSocket(context.Background(), VoiceWebSocketInput{RequestID: "interrupted", ClientKey: key, PublicModel: "billing-stt", Path: "/stt"})
	if err != nil {
		t.Fatal(err)
	}
	session.Finalize(VoiceWebSocketOutcome{ErrorCode: "client_stream_interrupted", AudioDurationSeconds: 3.45})

	got, err := keyRepo.Get(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := audit.EstimateOfficialSTTCost(3.45, true)
	if !ok {
		t.Fatal("expected a priced result for a positive duration")
	}
	if got.BilledUsageUSDTicks != want.CostInUSDTicks {
		t.Fatalf("interrupted session billed %d ticks, want %d (same as a clean completion)", got.BilledUsageUSDTicks, want.CostInUSDTicks)
	}
}

func TestVoiceWSFailureBeforeAnyConfirmedDurationStaysUnbilled(t *testing.T) {
	service, keyRepo, key := buildBillingTestService(t)
	session, err := service.OpenVoiceWebSocket(context.Background(), VoiceWebSocketInput{RequestID: "no-duration", ClientKey: key, PublicModel: "billing-stt", Path: "/stt"})
	if err != nil {
		t.Fatal(err)
	}
	session.Finalize(VoiceWebSocketOutcome{ErrorCode: "client_stream_interrupted"})

	got, err := keyRepo.Get(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BilledUsageUSDTicks != 0 {
		t.Fatalf("session with no confirmed duration billed %d ticks, want 0", got.BilledUsageUSDTicks)
	}
}
