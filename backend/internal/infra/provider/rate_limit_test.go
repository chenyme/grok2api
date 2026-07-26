package provider

import (
	"testing"
	"time"
)

func TestParseRateLimitMetadataBuildTeamRPS(t *testing.T) {
	body := []byte(`{"code":"resource-exhausted","error":"Too many requests for team f1692451-874f-4765-ab9b-5285f6c6ff65 and model grok-4.5-build-free. Your team's rate limit is — Requests per Second (actual/limit): 2/2."}`)
	metadata := ParseRateLimitMetadata(body)
	if metadata == nil {
		t.Fatal("expected metadata")
	}
	if metadata.Scope != RateLimitScopeRPS {
		t.Fatalf("scope = %q", metadata.Scope)
	}
	if metadata.TeamID != "f1692451-874f-4765-ab9b-5285f6c6ff65" {
		t.Fatalf("team = %q", metadata.TeamID)
	}
	if metadata.Model != "grok-4.5-build-free" {
		t.Fatalf("model = %q", metadata.Model)
	}
	if metadata.Actual != 2 || metadata.Limit != 2 {
		t.Fatalf("actual/limit = %d/%d", metadata.Actual, metadata.Limit)
	}
	if metadata.RetryAfter != 2*time.Second {
		t.Fatalf("retryAfter = %s, want 2s for RPS without resets-in", metadata.RetryAfter)
	}
}

func TestParseRateLimitMetadataOrdinary429(t *testing.T) {
	if metadata := ParseRateLimitMetadata([]byte(`{"error":"You are sending requests too quickly"}`)); metadata != nil {
		t.Fatalf("ordinary 429 must not parse as team rate limit: %#v", metadata)
	}
}
