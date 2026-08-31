package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestImagePreSubmitRefusalRequiresExactPostEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{name: "generation", method: http.MethodPost, path: "/v1/images/generations", want: true},
		{name: "edit", method: http.MethodPost, path: "/v1/images/edits", want: true},
		{name: "wrong method", method: http.MethodGet, path: "/v1/images/generations"},
		{name: "lookalike path", method: http.MethodPost, path: "/v1/images/generations/retry"},
		{name: "non image", method: http.MethodPost, path: "/v1/responses"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(test.method, test.path, nil)
			context.Request.Header.Set(UpstreamRequestDispositionHeader, "not-submitted")
			if handled := AbortImagePreSubmitRefusal(context); handled != test.want {
				t.Fatalf("handled = %v, want %v", handled, test.want)
			}
			if test.want {
				if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get(UpstreamRequestDispositionHeader) != "not-submitted" || !strings.Contains(recorder.Body.String(), `"code":"upstream_not_submitted"`) {
					t.Fatalf("status=%d headers=%#v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
				}
			} else if recorder.Header().Get(UpstreamRequestDispositionHeader) != "" || recorder.Body.Len() != 0 {
				t.Fatalf("unhandled request minted evidence: headers=%#v body=%s", recorder.Header(), recorder.Body.String())
			}
		})
	}
}
