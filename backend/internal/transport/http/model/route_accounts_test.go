package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestAddRouteAccountsHTTPContractAndConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "route-accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	modelRepo := relational.NewModelRepository(database)
	accountRepo := relational.NewAccountRepository(database)
	first := createHTTPRouteAccount(t, ctx, accountRepo, "route-http-first")
	second := createHTTPRouteAccount(t, ctx, accountRepo, "route-http-second")
	route, err := modelRepo.Create(ctx, modeldomain.Route{
		PublicID: "grok-imagine-image-2.0", Provider: account.ProviderWeb,
		UpstreamModel: "grok-imagine-image-2.0", Capability: modeldomain.CapabilityImage, Enabled: true,
	}, []uint64{first.ID})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewHandler(modelapp.NewService(modelRepo, accountRepo, nil, nil)).Register(router.Group(""))

	body := fmt.Sprintf(`{"accountIds":[%q,%q],"expected":{"publicId":"grok-imagine-image-2.0","provider":"grok_web","upstreamModel":"grok-imagine-image-2.0","capability":"image","enabled":true}}`, fmt.Sprint(second.ID), fmt.Sprint(second.ID))
	recorder := requestRouteAccountAdd(t, router, route.ID, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Data modelResponse `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.ID != route.ID || envelope.Data.PublicID != "grok-imagine-image-2.0" || envelope.Data.Provider != "grok_web" || envelope.Data.UpstreamModel != "Web/grok-imagine-image-2.0" || envelope.Data.Capability != "image" || !envelope.Data.Enabled {
		t.Fatalf("route response = %#v", envelope.Data)
	}
	wantIDs := []string{fmt.Sprint(first.ID), fmt.Sprint(second.ID)}
	if fmt.Sprint(envelope.Data.AccountIDs) != fmt.Sprint(wantIDs) || !envelope.Data.BindingMode {
		t.Fatalf("route bindings = %#v", envelope.Data)
	}
	if strings.Contains(recorder.Body.String(), "encrypted") || strings.Contains(recorder.Body.String(), "sourceKey") {
		t.Fatalf("response exposed credential material: %s", recorder.Body.String())
	}

	conflictBody := strings.Replace(body, `"publicId":"grok-imagine-image-2.0"`, `"publicId":"other-route"`, 1)
	conflict := requestRouteAccountAdd(t, router, route.ID, conflictBody)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"code":"modelConflict"`) {
		t.Fatalf("conflict status = %d, body = %s", conflict.Code, conflict.Body.String())
	}
	stored, err := modelRepo.Get(ctx, route.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(stored.BoundAccountIDs) != fmt.Sprint([]uint64{first.ID, second.ID}) {
		t.Fatalf("conflict changed bindings: %v", stored.BoundAccountIDs)
	}

	// The legacy whole-set PATCH must not race the managed Web image route.
	// Atomic add is the only membership writer for this route class.
	patchBody := fmt.Sprintf(`{"accountIds":[%q]}`, fmt.Sprint(first.ID))
	patchRequest := httptest.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("/models/%d", route.ID),
		strings.NewReader(patchBody),
	)
	patchRequest.Header.Set("Content-Type", "application/json")
	patchRecorder := httptest.NewRecorder()
	router.ServeHTTP(patchRecorder, patchRequest)
	if patchRecorder.Code != http.StatusConflict || !strings.Contains(patchRecorder.Body.String(), `"code":"modelConflict"`) {
		t.Fatalf("legacy PATCH status = %d, body = %s", patchRecorder.Code, patchRecorder.Body.String())
	}
	stored, err = modelRepo.Get(ctx, route.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(stored.BoundAccountIDs) != fmt.Sprint([]uint64{first.ID, second.ID}) {
		t.Fatalf("legacy PATCH erased atomic membership: %v", stored.BoundAccountIDs)
	}
}

func TestAddRouteAccountsRejectsIncompleteOrUnknownJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(modelapp.NewService(nil, nil, nil, nil)).Register(router.Group(""))
	for _, body := range []string{
		`{"accountIds":["1"],"expected":{"publicId":"model","provider":"grok_web","upstreamModel":"model","capability":"image"}}`,
		`{"accountIds":["1"],"expected":{"publicId":"model","provider":"grok_web","upstreamModel":"model","capability":"image","enabled":true},"unknown":true}`,
		`{"accountIds":["1"],"expected":{"publicId":"model","provider":"grok_web","upstreamModel":"model","capability":"image","enabled":true}} {}`,
	} {
		recorder := requestRouteAccountAdd(t, router, 1, body)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"invalidRequest"`) {
			t.Fatalf("body %q: status = %d, response = %s", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAddRouteAccountsHTTPRefusesWildcardAndListReportsBindingMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "route-accounts-wildcard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	modelRepo := relational.NewModelRepository(database)
	accountRepo := relational.NewAccountRepository(database)
	webAccount := createHTTPRouteAccount(t, ctx, accountRepo, "route-http-wildcard")
	err = modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: "grok-imagine-image-2.0", Provider: account.ProviderWeb,
		UpstreamModel: "grok-imagine-image-2.0", Capability: modeldomain.CapabilityImage, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	route, err := modelRepo.GetByPublicIDIncludingDisabled(ctx, "grok-imagine-image-2.0")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewHandler(modelapp.NewService(modelRepo, accountRepo, nil, nil)).Register(router.Group(""))
	body := fmt.Sprintf(`{"accountIds":[%q],"expected":{"publicId":"grok-imagine-image-2.0","provider":"grok_web","upstreamModel":"grok-imagine-image-2.0","capability":"image","enabled":true}}`, fmt.Sprint(webAccount.ID))
	conflict := requestRouteAccountAdd(t, router, route.ID, body)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"code":"modelConflict"`) {
		t.Fatalf("status = %d, body = %s", conflict.Code, conflict.Body.String())
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/models?page=1&pageSize=20", nil)
	listRecorder := httptest.NewRecorder()
	router.ServeHTTP(listRecorder, listRequest)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRecorder.Code, listRecorder.Body.String())
	}
	var envelope struct {
		Data struct {
			Items []modelResponse `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Items) != 1 || envelope.Data.Items[0].ID != route.ID {
		t.Fatalf("list response = %#v", envelope.Data.Items)
	}
	item := envelope.Data.Items[0]
	if item.BindingMode || len(item.AccountIDs) != 0 || !item.Available || item.SupportedAccounts != 1 {
		t.Fatalf("wildcard route projection = %#v", item)
	}
}

func createHTTPRouteAccount(t *testing.T, ctx context.Context, repo *relational.AccountRepository, source string) account.Credential {
	t.Helper()
	value, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: source, SourceKey: source,
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func requestRouteAccountAdd(t *testing.T, router http.Handler, routeID uint64, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/models/%d/accounts", routeID), strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}
