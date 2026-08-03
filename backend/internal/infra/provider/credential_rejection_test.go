package provider

import (
	"net/http"
	"testing"
)

func TestClassifyCredentialRejectionSpendingLimit(t *testing.T) {
	t.Parallel()
	body := []byte(`{"code":"personal-team-blocked:spending-limit","error":"quota exhausted"}`)
	for _, status := range []int{http.StatusPaymentRequired, http.StatusForbidden} {
		result := ClassifyCredentialRejection(status, body, nil)
		if !result.SpendingLimitBlocked {
			t.Fatalf("status %d: SpendingLimitBlocked = false, want true", status)
		}
		if result.Rejected {
			t.Fatalf("status %d: Rejected = true, want false for spending-limit", status)
		}
	}
}

func TestClassifyCredentialRejectionUnauthorized(t *testing.T) {
	t.Parallel()
	result := ClassifyCredentialRejection(http.StatusUnauthorized, nil, nil)
	if !result.Rejected || result.SpendingLimitBlocked {
		t.Fatalf("unauthorized rejection = %#v", result)
	}
}

func TestClassifyCredentialRejectionPermanentDenial(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":"Access to the chat endpoint is denied"}`)
	result := ClassifyCredentialRejection(http.StatusForbidden, body, nil)
	if !result.PermanentAccountDenial || result.Rejected || result.SpendingLimitBlocked {
		t.Fatalf("permanent denial = %#v", result)
	}
}

func TestClassifyCredentialRejectionQuotaScopes(t *testing.T) {
	free := ClassifyCredentialRejection(http.StatusTooManyRequests, []byte(`{"code":"subscription:free-usage-exhausted","error":"tokens (actual/limit): 10/10"}`), nil)
	if !free.QuotaExhausted || !free.FreeQuotaExhausted || free.ModelQuotaExhausted || free.Rejected {
		t.Fatalf("free quota classification = %#v", free)
	}
	model := ClassifyCredentialRejection(http.StatusForbidden, []byte(`{"error":"You've used all the included free usage for model grok-4.5"}`), nil)
	if !model.QuotaExhausted || !model.FreeQuotaExhausted || !model.ModelQuotaExhausted || model.Rejected {
		t.Fatalf("model quota classification = %#v", model)
	}
}
