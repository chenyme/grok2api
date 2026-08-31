package clientkey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

func TestCreateDistinguishesOmittedLimitsFromExplicitZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "client-key-handler.db"))
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
	service := clientkeyapp.NewService(relational.NewClientKeyRepository(database), nil, nil, 60, 5, cipher)
	router := gin.New()
	NewHandler(service).Register(router.Group("/api"))

	assertCreate := func(body string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/client-keys", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("create response = %d %s", response.Code, response.Body.String())
		}
	}
	assertCreate(`{"name":"defaults"}`)
	assertCreate(`{"name":"unlimited","rpmLimit":0,"maxConcurrent":0}`)
	assertCreate(`{"name":"free-pool","accountPool":"free"}`)
	assertCreate(`{"name":"mixed-scope","providerScope":["grok_build","grok_web"],"tierScope":["free","super"]}`)
	assertCreate(`{"name":"stress-cohort","routingCohort":"stress"}`)

	defaults, total, err := service.List(ctx, 1, 20, "defaults", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(defaults) != 1 {
		t.Fatalf("default key list = %#v, total = %d, err = %v", defaults, total, err)
	}
	if defaults[0].RPMLimit != 60 || defaults[0].MaxConcurrent != 5 {
		t.Fatalf("omitted limits = rpm %d, concurrency %d", defaults[0].RPMLimit, defaults[0].MaxConcurrent)
	}
	unlimited, total, err := service.List(ctx, 1, 20, "unlimited", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(unlimited) != 1 {
		t.Fatalf("unlimited key list = %#v, total = %d, err = %v", unlimited, total, err)
	}
	if unlimited[0].RPMLimit != 0 || unlimited[0].MaxConcurrent != 0 {
		t.Fatalf("explicit zero limits = rpm %d, concurrency %d", unlimited[0].RPMLimit, unlimited[0].MaxConcurrent)
	}
	freePool, total, err := service.List(ctx, 1, 20, "free-pool", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(freePool) != 1 || freePool[0].ProviderScope != 7 || freePool[0].TierScope != 1 {
		t.Fatalf("free-pool key list = %#v, total = %d, err = %v", freePool, total, err)
	}
	mixedScope, total, err := service.List(ctx, 1, 20, "mixed-scope", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(mixedScope) != 1 || mixedScope[0].ProviderScope != 3 || mixedScope[0].TierScope != 3 {
		t.Fatalf("mixed-scope key list = %#v, total = %d, err = %v", mixedScope, total, err)
	}
	stressCohort, total, err := service.List(ctx, 1, 20, "stress-cohort", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(stressCohort) != 1 || stressCohort[0].RoutingCohort != "stress" {
		t.Fatalf("stress cohort key list = %#v, total = %d, err = %v", stressCohort, total, err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/client-keys", bytes.NewBufferString(`{"name":"invalid-pool","accountPool":"unknown"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid pool response = %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/client-keys", bytes.NewBufferString(`{"name":"ambiguous","accountPool":"free","tierScope":["super"]}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous scope response = %d %s", response.Code, response.Body.String())
	}
	for _, body := range []string{
		`{"name":"empty-providers","providerScope":[]}`,
		`{"name":"empty-tiers","tierScope":[]}`,
		`{"name":"empty-legacy-pool","accountPool":""}`,
		`{"name":"invalid-cohort","routingCohort":"INVALID"}`,
	} {
		request = httptest.NewRequest(http.MethodPost, "/api/client-keys", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		response = httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("empty scope response = %d %s", response.Code, response.Body.String())
		}
	}
}

func TestConcurrencySnapshotIsExactReadOnlyAndSecretFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "client-key-concurrency-handler.db"))
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
	limiter := &snapshotConcurrencyLimiter{values: make(map[string]int)}
	service := clientkeyapp.NewService(relational.NewClientKeyRepository(database), nil, limiter, 60, 5, cipher)
	created, err := service.Create(ctx, clientkeyapp.CreateInput{Name: "platform", Enabled: true, RPMLimit: 77, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}
	runtimeKey := repository.ClientConcurrencyKey(created.Key.ID)
	id := strconv.FormatUint(created.Key.ID, 10)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte("grok2api-client-key-v1\x00"+created.Secret)))
	limiter.values[runtimeKey] = 3
	router := gin.New()
	NewHandler(service).Register(router.Group("/api"))

	request := httptest.NewRequest(http.MethodGet, "/api/client-keys/"+id+"/concurrency", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("snapshot response = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot cache control = %q", recorder.Header().Get("Cache-Control"))
	}
	body := recorder.Body.String()
	for _, field := range []string{
		`"id":"` + id + `"`,
		`"name":"platform"`,
		`"prefix":"` + created.Key.Prefix + `"`,
		`"clientKeyFingerprint":"` + fingerprint + `"`,
		`"rpmLimit":77`,
		`"maxConcurrent":8`,
		`"currentInflight":3`,
	} {
		if !strings.Contains(body, field) {
			t.Fatalf("snapshot body %s missing %s", body, field)
		}
	}
	if strings.Contains(body, created.Secret) || strings.Contains(body, "secretHash") || strings.Contains(body, "encryptedSecret") {
		t.Fatalf("snapshot leaked secret material: %s", body)
	}
	if limiter.acquireCalls != 0 || limiter.currentCalls != 0 || limiter.batchCalls != 1 ||
		len(limiter.keys) != 1 || limiter.keys[0] != runtimeKey {
		t.Fatalf(
			"runtime reads: acquire=%d current=%d batch=%d keys=%v",
			limiter.acquireCalls, limiter.currentCalls, limiter.batchCalls, limiter.keys,
		)
	}

	limiter.err = errors.New("redis endpoint details")
	request = httptest.NewRequest(http.MethodGet, "/api/client-keys/"+id+"/concurrency", nil)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), `"code":"clientKeyRuntimeUnavailable"`) ||
		strings.Contains(recorder.Body.String(), "redis endpoint details") {
		t.Fatalf("unavailable response = %d %s", recorder.Code, recorder.Body.String())
	}
}

type snapshotConcurrencyLimiter struct {
	values       map[string]int
	err          error
	keys         []string
	acquireCalls int
	currentCalls int
	batchCalls   int
}

func (l *snapshotConcurrencyLimiter) Acquire(context.Context, string, int) (func(), bool, error) {
	l.acquireCalls++
	return func() {}, true, nil
}

func (l *snapshotConcurrencyLimiter) Current(context.Context, string) (int, error) {
	l.currentCalls++
	return 0, nil
}

func (l *snapshotConcurrencyLimiter) CurrentMany(_ context.Context, keys []string) (map[string]int, error) {
	l.batchCalls++
	l.keys = append([]string(nil), keys...)
	if l.err != nil {
		return nil, l.err
	}
	values := make(map[string]int, len(keys))
	for _, key := range keys {
		if value := l.values[key]; value != 0 {
			values[key] = value
		}
	}
	return values, nil
}

var _ repository.ConcurrencyLimiter = (*snapshotConcurrencyLimiter)(nil)
var _ repository.ConcurrencySnapshotReader = (*snapshotConcurrencyLimiter)(nil)
