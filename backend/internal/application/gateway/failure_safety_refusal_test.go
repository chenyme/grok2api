package gateway

import (
	"net/http"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// Grok Build/CLI 的内容拒答返回 403 {"code":"permission-denied",
// "error":"I'm sorry, I can't help with that request."}。它必须被识别为请求级
// 内容拒绝：不打账号、不轮换整池，直接透传 403 给客户端。
func TestSafetyRefusalIsRequestScoped(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"ascii-apostrophe", `{"code":"permission-denied","error":"I'm sorry, I can't help with that request."}`},
		{"curly-apostrophe", "{\"code\":\"permission-denied\",\"error\":\"I\u2019m sorry, I can\u2019t help with that request.\"}"},
		{"cannot", `{"code":"permission-denied","error":"Sorry, I cannot help with that request."}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := newHTTPUpstreamFailure(http.StatusForbidden, []byte(tc.body), 42, "build")
			if !failure.SafetyRejection {
				t.Fatalf("SafetyRejection = false, want true: %#v", failure)
			}
			if failure.AccountScoped {
				t.Fatalf("refusal must not be account-scoped: %#v", failure)
			}
			if !isTerminalRequestForbidden(accountdomain.ProviderBuild, failure) {
				t.Fatalf("refusal must be terminal for Build so no account rotation happens")
			}
		})
	}
}

// 已知账户级 403 不能被误判成内容拒答。
func TestAccountScopedForbiddenIsNotSafetyRejection(t *testing.T) {
	body := []byte(`{"code":"permission-denied","error":"access to the chat endpoint is denied"}`)
	failure := newHTTPUpstreamFailure(http.StatusForbidden, body, 42, "build")
	if failure.SafetyRejection {
		t.Fatalf("account denial must not be classified as safety rejection: %#v", failure)
	}
	if !failure.PermanentAccountDenial {
		t.Fatalf("PermanentAccountDenial = false, want true: %#v", failure)
	}
}
