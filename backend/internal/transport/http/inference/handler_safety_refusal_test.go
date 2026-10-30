package inference

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/gin-gonic/gin"
)

// 内容拒答（403 + permission-denied + "I can't help with that request"）必须原样
// 透传给客户端：状态码保持 403、body 不被改写成 503 凭证错误，供上游/客户端自行处理。
func TestSafetyRefusalPassedThroughAsIs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const refusal = `{"code":"permission-denied","error":"I'm sorry, I can't help with that request."}`
	for _, tc := range []struct {
		name      string
		anthropic bool
	}{
		{name: "openai-chat"},
		{name: "anthropic", anthropic: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finalCode := ""
			result := &gateway.Result{
				StatusCode:      http.StatusForbidden,
				Status:          "403 Forbidden",
				Header:          http.Header{"Content-Type": {"application/json"}},
				Body:            io.NopCloser(strings.NewReader(refusal)),
				SafetyRejection: true,
				Finalize:        func(_ gateway.Usage, _, code string) { finalCode = code },
			}
			handler := &Handler{}
			router := gin.New()
			router.GET("/", func(c *gin.Context) {
				if tc.anthropic {
					handler.writeAnthropicResult(c, result, false)
					return
				}
				handler.writeResult(c, result, false, streamProtocolResponses)
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "I can't help with that request") {
				t.Fatalf("refusal body not passed through: %s", recorder.Body.String())
			}
			if finalCode == "upstream_unavailable" {
				t.Fatalf("finalize code must not be upstream_unavailable for a passed-through refusal")
			}
		})
	}
}

// 未标记 SafetyRejection 的 403 仍走原来的 503 脱敏路径，避免泄露上游凭证细节。
func TestCredentialForbiddenStillSanitized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	finalCode := ""
	result := &gateway.Result{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"code":"permission-denied","error":"secret upstream credential detail"}`)),
		Finalize:   func(_ gateway.Usage, _, code string) { finalCode = code },
	}
	handler := &Handler{}
	router := gin.New()
	router.GET("/", func(c *gin.Context) { handler.writeResult(c, result, false, streamProtocolResponses) })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if finalCode != "upstream_unavailable" {
		t.Fatalf("finalize code = %q, want upstream_unavailable", finalCode)
	}
}
