package gateway

import (
	"net/http"
	"testing"
)

func TestHTTPUpstreamFailureClassifiesBuildForbiddenBodies(t *testing.T) {
	tests := []struct {
		name                   string
		body                   string
		accountScoped          bool
		permanentAccountDenial bool
		quotaExhausted         bool
		freeQuotaExhausted     bool
		modelQuotaExhausted    bool
		upstreamCode           string
	}{
		{
			name: "top-level permanent chat denial", body: `{"status_code":403,"error":"Access to the chat endpoint is denied. Please update the permissions."}`,
			accountScoped: true, permanentAccountDenial: true,
		},
		{
			name: "spending limit", body: `{"code":"personal-team-blocked:spending-limit","error":"quota exhausted"}`,
			accountScoped: true, quotaExhausted: true, upstreamCode: "personal-team-blocked:spending-limit",
		},
		{
			name: "unknown policy rejection", body: `{"error":"upstream policy rejected request"}`,
		},
		{
			name: "free model quota", body: `{"error":"You've used all the included free usage for model grok-build"}`,
			accountScoped: true, quotaExhausted: true, freeQuotaExhausted: true, modelQuotaExhausted: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := newHTTPUpstreamFailure(http.StatusForbidden, []byte(test.body), 42, "build")
			if failure.HTTPStatus != http.StatusForbidden || failure.Code != "upstream_forbidden" || failure.AccountScoped != test.accountScoped || failure.PermanentAccountDenial != test.permanentAccountDenial || failure.QuotaExhausted != test.quotaExhausted || failure.FreeQuotaExhausted != test.freeQuotaExhausted || failure.ModelQuotaExhausted != test.modelQuotaExhausted || failure.UpstreamCode != test.upstreamCode {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
}
