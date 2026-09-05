package conversation

import (
	"testing"
	"time"
)

func TestReasoningCacheSetAndGet(t *testing.T) {
	cache := newReasoningCache(10, time.Hour)
	sample := responseItem{
		ID:        "rs_123",
		Type:      "reasoning",
		Status:    "completed",
		Encrypted: "encrypted-content-sample",
	}

	cache.Set("call-1", sample)
	got, found := cache.Get("call-1")
	if !found {
		t.Fatalf("expected call-1 to be found in cache")
	}
	if got.Encrypted != sample.Encrypted || got.ID != sample.ID {
		t.Fatalf("expected %v, got %v", sample, got)
	}

	_, found2 := cache.Get("non-existent")
	if found2 {
		t.Fatalf("expected non-existent to return false")
	}
}

func TestRememberReasoningForEnvelope(t *testing.T) {
	env := responseEnvelope{
		Output: []responseItem{
			{
				ID:        "rs_abc",
				Type:      "reasoning",
				Status:    "completed",
				Encrypted: "encrypted_abc",
			},
			{
				ID:     "fc_1",
				Type:   "function_call",
				CallID: "call_abc_1",
				Name:   "test_fn",
			},
			{
				ID:     "fc_2",
				Type:   "function_call",
				CallID: "call_abc_2",
				Name:   "test_fn2",
			},
		},
	}

	RememberReasoningForEnvelope(env)

	r1, found1 := GetReasoningForCall("call_abc_1")
	if !found1 || r1.Encrypted != "encrypted_abc" {
		t.Fatalf("expected call_abc_1 to be cached, got %v", r1)
	}

	r2, found2 := GetReasoningForCall("call_abc_2")
	if !found2 || r2.Encrypted != "encrypted_abc" {
		t.Fatalf("expected call_abc_2 to be cached, got %v", r2)
	}
}
