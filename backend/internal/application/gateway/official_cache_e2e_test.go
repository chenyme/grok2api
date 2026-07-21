package gateway

import (
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// Simulate Grok Build cache affinity when an OpenAI-compatible client forwards prompt_cache_key.
func TestOfficialCache_ClientPromptCacheKeyStickyAcrossTurns(t *testing.T) {
	// The client forwards body.prompt_cache_key.
	explicit := "client-stable-session-uuid-001"
	turn1Body := []byte(`{"model":"grok-4.5","prompt_cache_key":"client-stable-session-uuid-001","messages":[{"role":"user","content":"hello"}]}`)
	turn2Body := []byte(`{"model":"grok-4.5","prompt_cache_key":"client-stable-session-uuid-001","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"again"}]}`)

	id1 := resolveBuildSessionIdentity(42, accountdomain.ProviderBuild, "grok-4.5", explicit, "", turn1Body)
	id2 := resolveBuildSessionIdentity(42, accountdomain.ProviderBuild, "grok-4.5", explicit, "", turn2Body)
	if id1.upstreamID == "" || id1.upstreamID != id2.upstreamID {
		t.Fatalf("客户端 prompt_cache_key 必须生成稳定的上游身份: %#v %#v", id1, id2)
	}
	if id1.affinityKey == "" || id1.affinityKey != id2.affinityKey {
		t.Fatalf("affinity must stick across turns: %#v %#v", id1, id2)
	}
	// Different tenants (clientKey) must remain isolated.
	other := resolveBuildSessionIdentity(99, accountdomain.ProviderBuild, "grok-4.5", explicit, "", turn1Body)
	if other.upstreamID == id1.upstreamID {
		t.Fatal("tenant isolation broken for explicit prompt_cache_key")
	}
}

func TestOfficialCache_MissingSessionFallsBackToMessageHash(t *testing.T) {
	// Fall back to the first user message when the client does not forward a session signal.
	turn1 := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"what is mutex"}]}`)
	turn2 := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"what is mutex"},{"role":"assistant","content":"A lock."},{"role":"user","content":"and deadlock?"}]}`)
	id1 := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", turn1)
	id2 := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", turn2)
	if !id1.soft || id1.upstreamID == "" {
		t.Fatalf("expected soft session, got %#v", id1)
	}
	if id1.upstreamID != id2.upstreamID {
		t.Fatalf("soft session drifted: %s vs %s", id1.upstreamID, id2.upstreamID)
	}
}

func TestOfficialCache_NoSignalMeansNoRandomConvID(t *testing.T) {
	// No signal: do not generate an upstream ID; the adapter must not write a random conv-id.
	id := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", []byte(`{}`))
	if id.upstreamID != "" || id.affinityKey != "" {
		t.Fatalf("empty signal must not invent session: %#v", id)
	}
}

func TestOfficialCache_ClaudeCodeSessionHeaderPath(t *testing.T) {
	// Simulate the handler extracting X-Claude-Code-Session-Id into the session seed.
	seed := "123e4567-e89b-12d3-a456-426614174000"
	a := resolveBuildSessionIdentity(1, accountdomain.ProviderBuild, "grok-4.5", "", seed, nil)
	b := resolveBuildSessionIdentity(1, accountdomain.ProviderBuild, "grok-4.5", "", seed, []byte(`{"messages":[{"role":"user","content":"x"}]}`))
	if a.upstreamID == "" || a.upstreamID != b.upstreamID {
		t.Fatalf("claude session seed unstable: %#v %#v", a, b)
	}
}
