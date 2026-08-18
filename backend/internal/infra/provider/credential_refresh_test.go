package provider

import (
	"net/http"
	"testing"
)

func TestIsUnclassifiedCredentialAuthRejection(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		want   bool
	}{
		{name: "opaque bad request", status: http.StatusBadRequest, code: "oauth_http_400", want: true},
		{name: "unknown unauthorized", status: http.StatusUnauthorized, code: "challenge_required", want: true},
		{name: "invalid grant is terminal", status: http.StatusBadRequest, code: "invalid_grant"},
		{name: "invalid client is configuration", status: http.StatusUnauthorized, code: "invalid_client"},
		{name: "temporary oauth condition", status: http.StatusBadRequest, code: "temporarily_unavailable"},
		{name: "server failure", status: http.StatusServiceUnavailable, code: "unknown", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsUnclassifiedCredentialAuthRejection(test.status, test.code); got != test.want {
				t.Fatalf("IsUnclassifiedCredentialAuthRejection(%d, %q) = %t, want %t", test.status, test.code, got, test.want)
			}
		})
	}
}
