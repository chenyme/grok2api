package account

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestNewAccountResponseExposesBuildBotFlagOnlyForBuild(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	build := newAccountResponse(accountapp.View{
		Credential:      accountdomain.Credential{Provider: accountdomain.ProviderBuild, BuildRouteMode: accountdomain.BuildRouteXAI, WebNSFWEnabledAt: &now, WebTermsAcceptedAt: &now},
		BuildBotFlagged: true,
	})
	if !build.BuildBotFlagged || build.BuildRouteMode != string(accountdomain.BuildRouteXAI) || build.WebNSFWEnabledAt == nil || !build.WebNSFWEnabledAt.Equal(now) || build.WebTermsAcceptedAt == nil || !build.WebTermsAcceptedAt.Equal(now) {
		t.Fatalf("Build metadata = %#v", build)
	}
	web := newAccountResponse(accountapp.View{
		Credential:      accountdomain.Credential{Provider: accountdomain.ProviderWeb, WebNSFWEnabledAt: &now, WebTermsAcceptedAt: &now},
		BuildBotFlagged: true,
	})
	if web.BuildBotFlagged || web.BuildRouteMode != string(accountdomain.BuildRouteAuto) || web.WebNSFWEnabledAt == nil || !web.WebNSFWEnabledAt.Equal(now) || web.WebTermsAcceptedAt == nil || !web.WebTermsAcceptedAt.Equal(now) {
		t.Fatalf("non-Build metadata = %#v", web)
	}
}

func TestNewAccountResponseExposesAllLinkedAccounts(t *testing.T) {
	response := newAccountResponse(accountapp.View{Credential: accountdomain.Credential{
		Provider: accountdomain.ProviderWeb,
		LinkedAccounts: []accountdomain.LinkedAccount{
			{ID: 2, Provider: accountdomain.ProviderBuild, Name: "build", Email: "build@example.com", UserID: "build-user"},
			{ID: 3, Provider: accountdomain.ProviderConsole, Name: "console", Email: "console@example.com", UserID: "console-user"},
		},
	}})
	if len(response.LinkedAccounts) != 2 || response.LinkedAccounts[0].Provider != string(accountdomain.ProviderBuild) || response.LinkedAccounts[0].Email != "build@example.com" || response.LinkedAccounts[0].UserID != "build-user" || response.LinkedAccounts[1].Provider != string(accountdomain.ProviderConsole) || response.LinkedAccounts[1].Email != "console@example.com" || response.LinkedAccounts[1].UserID != "console-user" {
		t.Fatalf("linked accounts = %#v", response.LinkedAccounts)
	}
}

func TestNewAccountResponseExposesCredentialRefreshError(t *testing.T) {
	response := newAccountResponse(accountapp.View{Credential: accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, LastRefreshErrorStatus: 400, LastRefreshErrorCode: "invalid_grant", LastRefreshErrorMessage: "Refresh token has expired", LastRefreshErrorResponse: `{"error":"invalid_grant"}`,
	}})
	if response.LastRefreshErrorStatus != 400 || response.LastRefreshError != "invalid_grant" || response.LastRefreshErrorMessage != "Refresh token has expired" || response.LastRefreshErrorResponse == "" {
		t.Fatalf("refresh error = %#v", response)
	}
}

type accountSynchronizerStub struct {
	accountIDs []uint64
}

type accountProgressSynchronizerStub struct {
	accountSynchronizerStub
}

func TestWriteServiceErrorUsesCredentialLimitCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "import", err: fmt.Errorf("%w: too many", accountapp.ErrImportLimit), code: "accountImportLimitExceeded"},
		{name: "export", err: fmt.Errorf("%w: too many", accountapp.ErrExportLimit), code: "accountExportLimitExceeded"},
		{name: "invalid input", err: fmt.Errorf("%w: bad", accountapp.ErrInvalidInput), code: "fallback"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			new(Handler).writeServiceError(ctx, "fallback", test.err, 500, "failed")
			if test.name == "invalid input" {
				if recorder.Code != 400 {
					t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
				}
				return
			}
			if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestCredentialExportRejectsOffsetPagination(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/api/admin/v1/accounts/export?provider=grok_build&limit=1000&offset=1000", nil)
	new(Handler).exportCredentials(ctx)
	if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "afterId") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestCredentialExportExposesBatchMetadataHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	new(Handler).writeCredentialExport(ctx, accountdomain.ProviderBuild, accountapp.ExportResult{Data: []byte(`[]`)})

	exposed := recorder.Header().Get("Access-Control-Expose-Headers")
	for _, header := range []string{"X-Exported-Accounts", "X-Export-Next-ID", "X-Export-Snapshot-Max-ID", "X-Export-Has-More"} {
		if !strings.Contains(exposed, header) {
			t.Fatalf("exposed headers %q do not include %s", exposed, header)
		}
	}
}

func TestParseLinkedDeleteTargets(t *testing.T) {
	targets, err := parseLinkedDeleteTargets([]string{"grok_build", "grok_console", "grok_build"})
	if err != nil || len(targets) != 2 || targets[0] != accountdomain.ProviderBuild || targets[1] != accountdomain.ProviderConsole {
		t.Fatalf("targets=%v err=%v", targets, err)
	}
	if _, err := parseLinkedDeleteTargets([]string{"nope"}); err == nil {
		t.Fatal("expected invalid target")
	}
	targets, err = parseLinkedDeleteTargets(nil)
	if err != nil || targets != nil {
		t.Fatalf("empty targets=%v err=%v", targets, err)
	}
}

func TestNewAccountDeleteResponseShape(t *testing.T) {
	payload := newAccountDeleteResponse(accountapp.AccountDeleteResult{
		Deleted: 3, RootsDeleted: 1, LinkedDeleted: 2,
		DeletedByProvider: map[accountdomain.Provider]int64{
			accountdomain.ProviderWeb:     1,
			accountdomain.ProviderBuild:   1,
			accountdomain.ProviderConsole: 1,
		},
	})
	if payload["deleted"] != int64(3) || payload["rootsDeleted"] != int64(1) || payload["linkedDeleted"] != int64(2) {
		t.Fatalf("payload = %#v", payload)
	}
	byProvider, ok := payload["deletedByProvider"].(gin.H)
	if !ok || byProvider["grok_web"] != int64(1) {
		t.Fatalf("byProvider = %#v", payload["deletedByProvider"])
	}
}

func TestLinkedDeleteMissingAccountReturnsNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "linked-delete-not-found.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service := accountapp.NewService(relational.NewAccountRepository(database), nil, nil, nil, nil, nil, nil)
	handler := NewHandler(service, nil)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = []gin.Param{{Key: "id", Value: "999999"}}
	ginContext.Request = httptest.NewRequest("DELETE", "/api/admin/v1/accounts/999999", strings.NewReader(`{"provider":"grok_web","linkedDeleteTargets":["grok_build"]}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")

	handler.delete(ginContext)

	if recorder.Code != 404 || !strings.Contains(recorder.Body.String(), `"code":"accountNotFound"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func (s *accountSynchronizerStub) Sync(_ context.Context, accountIDs ...uint64) accountsyncapp.Result {
	s.accountIDs = append(s.accountIDs, accountIDs...)
	return accountsyncapp.Result{Succeeded: len(accountIDs)}
}

func (s *accountSynchronizerStub) SyncStream(_ context.Context, accountIDs <-chan uint64) accountsyncapp.Result {
	for accountID := range accountIDs {
		s.accountIDs = append(s.accountIDs, accountID)
	}
	return accountsyncapp.Result{Succeeded: len(s.accountIDs)}
}

func (s *accountProgressSynchronizerStub) SyncStreamObserved(_ context.Context, accountIDs <-chan uint64, observer func(completed, total int)) accountsyncapp.Result {
	for accountID := range accountIDs {
		s.accountIDs = append(s.accountIDs, accountID)
	}
	for completed := 1; completed <= len(s.accountIDs); completed++ {
		observer(completed, completed)
	}
	return accountsyncapp.Result{Succeeded: len(s.accountIDs)}
}

func TestSyncInitialUsesOnlyChangedAccounts(t *testing.T) {
	sync := &accountSynchronizerStub{}
	handler := NewHandler(nil, sync)

	result := handler.syncInitial(context.Background(), 3, 5)

	if result.Succeeded != 2 || len(sync.accountIDs) != 2 || sync.accountIDs[0] != 3 || sync.accountIDs[1] != 5 {
		t.Fatalf("account ids = %#v", sync.accountIDs)
	}
}

func TestWriteBuildConversionEventUsesSSEFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/api/admin/v1/accounts/web/convert-to-build", nil)

	if err := writeAccountEvent(ctx, "progress", accountTaskProgressResponse{Completed: 3, Total: 10}); err != nil {
		t.Fatal(err)
	}
	if body := recorder.Body.String(); body != "event: progress\ndata: {\"completed\":3,\"total\":10}\n\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestConvertWebToBuildRejectsInvalidStrategy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/api/admin/v1/accounts/web/convert-to-build", strings.NewReader(`{"ids":["1"],"strategy":"invalid"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	new(Handler).convertWebToBuild(ctx)

	if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), `"code":"invalidRequest"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestAccountProgressEventIncludesOptionalPhase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/api/admin/v1/accounts/import", nil)
	stream := &accountEventStream{context: ctx}
	var total atomic.Int64

	if err := stream.PhaseProgressObserver("importing", &total)(3, 10); err != nil {
		t.Fatal(err)
	}
	if body := recorder.Body.String(); body != "event: progress\ndata: {\"completed\":3,\"total\":10,\"phase\":\"importing\"}\n\n" {
		t.Fatalf("body = %q", body)
	}
	if total.Load() != 10 {
		t.Fatalf("total = %d", total.Load())
	}
}

func TestReadAccountImportDocumentsAcceptsMultipleFiles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range map[string]string{"first.json": `{"accounts":[]}`, "second.json": `{"provider":"grok_build"}`} {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/api/admin/v1/accounts/import", &body)
	ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())

	documents, ok := readAccountImportDocuments(ctx, "账号凭据 JSON")
	if !ok || len(documents) != 2 {
		t.Fatalf("documents = %q, status = %d", documents, recorder.Code)
	}
}

func TestAccountSyncPipelineUsesFinalQueuedTotal(t *testing.T) {
	syncer := &accountProgressSynchronizerStub{}
	handler := NewHandler(nil, syncer)
	progress := make([][2]int, 0, 5)
	pipeline := handler.startSyncPipeline(context.Background(), func(completed, total int) {
		progress = append(progress, [2]int{completed, total})
	})

	for _, accountID := range []uint64{11, 12, 13} {
		if err := pipeline.Observe(accountID); err != nil {
			t.Fatal(err)
		}
	}
	result := pipeline.Finish(false)

	if result.Succeeded != 3 {
		t.Fatalf("result = %#v", result)
	}
	if len(progress) == 0 || progress[len(progress)-1] != [2]int{3, 3} {
		t.Fatalf("progress = %#v", progress)
	}
	for _, value := range progress {
		if value[1] != 3 {
			t.Fatalf("progress contains changing total: %#v", progress)
		}
	}
}
