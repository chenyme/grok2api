package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	account "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

type degradedWebSearchAdapter struct{ attempts atomic.Int32 }

func (a *degradedWebSearchAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *degradedWebSearchAdapter) Definition() provider.Definition {
	definition := testConversationDefinition(account.ProviderBuild)
	definition.Credential.Refresh = false
	return definition
}
func (a *degradedWebSearchAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	a.attempts.Add(1)
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   io.NopCloser(strings.NewReader(`{"output":[{"type":"web_search_call","action":{"type":"search","query":"current information and recent sources"}}]}`)),
	}, nil
}

func TestCreateResponseRejectsKnownDegradedHostedWebSearch(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "test", SourceKey: "test", EncryptedAccessToken: "test",
		ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-test"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-test"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "test-key", Prefix: "test-prefix", SecretHash: strings.Repeat("0", 64),
		EncryptedSecret: "encrypted-key", Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &degradedWebSearchAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	concurrency := memory.NewConcurrencyLimiter()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	clientService := clientkeyapp.NewService(nil, nil, nil, 60, 4, nil)
	selector := NewSelector(accountRepo, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientService, registry, selector, responseRepo, 1)

	_, err = service.CreateResponse(ctx, Input{
		RequestID: "req-degraded-search", ClientKey: clientKey, PublicModel: "grok-test",
		Body: []byte(`{"model":"grok-test","input":"Rust 1.90 release notes","tools":[{"type":"web_search"}]}`),
	})
	var failure *UpstreamFailure
	if !errors.As(err, &failure) || failure.HTTPStatus != http.StatusServiceUnavailable || failure.Code != ErrorQualityDegraded {
		t.Fatalf("CreateResponse error = %#v, want quality degraded 503", err)
	}
	if got := adapter.attempts.Load(); got != 1 {
		t.Fatalf("upstream attempts = %d, want 1 because hosted search is not replayed", got)
	}
}

func TestQualityRequestUsesHostedWebSearch(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{name: "responses web search", body: `{"tools":[{"type":"web_search"}]}`, want: true},
		{name: "versioned web search", body: `{"tools":[{"type":"web_search_2025_08_26"}]}`, want: true},
		{name: "deferred web search", body: `{"input":[{"type":"additional_tools","tools":[{"type":"web_search"}]}]}`, want: true},
		{name: "nested namespace web search", body: `{"tools":[{"type":"namespace","tools":[{"type":"web_search"}]}]}`, want: true},
		{name: "local function", body: `{"tools":[{"type":"function","name":"web_search"}]}`, want: false},
		{name: "user content only", body: `{"input":"web_search"}`, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := qualityRequestUsesHostedWebSearch([]byte(test.body)); got != test.want {
				t.Fatalf("qualityRequestUsesHostedWebSearch() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResponsesContainDegradedWebSearch(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{name: "known placeholder", body: `{"output":[{"type":"web_search_call","action":{"type":"search","query":"Current information and recent sources"}}]}`, want: true},
		{name: "whitespace normalized", body: `{"output":[{"type":"web_search_call","action":{"type":"search","query":"latest   updates and authoritative references"}}]}`, want: true},
		{name: "wrapped response", body: `{"response":{"output":[{"type":"web_search_call","action":{"type":"search","query":"best expert recommendations and evidence"}}]}}`, want: true},
		{name: "relevant query", body: `{"output":[{"type":"web_search_call","action":{"type":"search","query":"Rust 1.90 release notes"}}]}`, want: false},
		{name: "text mentioning phrase", body: `{"output":[{"type":"message","content":[{"text":"current information and recent sources"}]}]}`, want: false},
		{name: "malformed response", body: `{`, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := responsesContainDegradedWebSearch([]byte(test.body)); got != test.want {
				t.Fatalf("responsesContainDegradedWebSearch() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestInspectDegradedResponsesWebSearchReplaysBody(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`{"output":[{"type":"message"}]}`))
	replay, degraded := inspectDegradedResponsesWebSearch([]byte(`{"tools":[{"type":"web_search"}]}`), body)
	if degraded {
		t.Fatal("a normal response was classified as degraded")
	}
	got, err := io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"output":[{"type":"message"}]}` {
		t.Fatalf("replayed response = %s", got)
	}
	if err := replay.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInspectDegradedResponsesWebSearchRequiresSearchRequest(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`{"output":[{"type":"web_search_call","action":{"type":"search","query":"current information and recent sources"}}]}`))
	replay, degraded := inspectDegradedResponsesWebSearch([]byte(`{"input":"hello"}`), body)
	if degraded {
		t.Fatal("response without a hosted search request was classified as degraded")
	}
	if _, err := io.ReadAll(replay); err != nil {
		t.Fatal(err)
	}
	_ = replay.Close()
}
