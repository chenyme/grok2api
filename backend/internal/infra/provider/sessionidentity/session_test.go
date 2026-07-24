package sessionidentity

import (
	"errors"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestParseBlockedSessionIsUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"status":"blocked"}`))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestParseBlockedSessionWithIdentityFieldsIsUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"status":"blocked","userId":"user-1","email":"a@example.com","session":{"userId":"user-1","email":"a@example.com"}}`))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized even when identity fields are present", err)
	}
}

func TestParseUnauthenticatedSessionIsUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"status":"unauthenticated"}`))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestParseUnauthenticatedSessionWithIdentityFieldsIsUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"status":"unauthenticated","userId":"user-1","email":"a@example.com"}`))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized even when identity fields are present", err)
	}
}

func TestParseMissingIdentityWithoutStatusStaysGeneric(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{}`))
	if err == nil || errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want generic missing-identity error", err)
	}
}

func TestParseAuthenticatedSession(t *testing.T) {
	t.Parallel()
	identity, err := Parse([]byte(`{
		"status":"authenticated",
		"session":{"userId":"user-1","email":"a@example.com","organizationId":"org-1"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if identity.UserID != "user-1" || identity.Email != "a@example.com" || identity.TeamID != "org-1" {
		t.Fatalf("identity = %#v", identity)
	}
}
