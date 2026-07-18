package account

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRunWebAccountScriptsRejectsInvalidRequestsBeforeSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name string
		body string
	}{
		{name: "all and ids", body: `{"all":true,"ids":["1"],"actions":{"acceptTerms":true}}`},
		{name: "empty selected", body: `{"ids":[],"actions":{"acceptTerms":true}}`},
		{name: "empty actions", body: `{"ids":["1"],"actions":{}}`},
		{name: "invalid id", body: `{"ids":["invalid"],"actions":{"acceptTerms":true}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("POST", "/api/admin/v1/accounts/web/run-scripts", strings.NewReader(test.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			new(Handler).runWebAccountScripts(ctx)

			if recorder.Code != 400 {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if contentType := recorder.Header().Get("Content-Type"); strings.HasPrefix(contentType, "text/event-stream") {
				t.Fatalf("invalid request switched to SSE: %q", contentType)
			}
		})
	}
}

func TestRunWebAccountScriptsRejectsOversizedSelectionBeforeSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ids := make([]string, maxWebAccountScriptRequestIDs+1)
	for index := range ids {
		ids[index] = strconv.Itoa(index + 1)
	}
	body, err := json.Marshal(webAccountScriptsRequest{
		IDs: ids,
		Actions: webAccountScriptActionsRequest{
			AcceptTerms: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/api/admin/v1/accounts/web/run-scripts", strings.NewReader(string(body)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	new(Handler).runWebAccountScripts(ctx)

	if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "单次最多处理 1000 个账号") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("oversized request switched to SSE: %q", contentType)
	}
}
