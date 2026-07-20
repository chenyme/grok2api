package gateway

import (
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// 模拟 Sub2API → Grok2API 官方缓存亲和链路（不连真上游）。
func TestOfficialCache_Sub2APIPromptCacheKeyStickyAcrossTurns(t *testing.T) {
	// Sub2API 透传 body.prompt_cache_key
	explicit := "sub2api-stable-session-uuid-001"
	turn1Body := []byte(`{"model":"grok-4.5","prompt_cache_key":"sub2api-stable-session-uuid-001","messages":[{"role":"user","content":"hello"}]}`)
	turn2Body := []byte(`{"model":"grok-4.5","prompt_cache_key":"sub2api-stable-session-uuid-001","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"again"}]}`)

	id1 := resolveBuildSessionIdentity(42, accountdomain.ProviderBuild, "grok-4.5", explicit, "", turn1Body)
	id2 := resolveBuildSessionIdentity(42, accountdomain.ProviderBuild, "grok-4.5", explicit, "", turn2Body)
	if id1.upstreamID == "" || id1.upstreamID != id2.upstreamID {
		t.Fatalf("sub2api prompt_cache_key must yield stable upstream id: %#v %#v", id1, id2)
	}
	if id1.affinityKey == "" || id1.affinityKey != id2.affinityKey {
		t.Fatalf("affinity must stick across turns: %#v %#v", id1, id2)
	}
	// 不同租户（clientKey）必须隔离
	other := resolveBuildSessionIdentity(99, accountdomain.ProviderBuild, "grok-4.5", explicit, "", turn1Body)
	if other.upstreamID == id1.upstreamID {
		t.Fatal("tenant isolation broken for explicit prompt_cache_key")
	}
}

func TestOfficialCache_Sub2APIMissingSessionFallsBackToMessageHash(t *testing.T) {
	// Sub2API 没透传任何 session：靠首条 user 内容 soft 粘滞
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
	// 完全无信号：不得生成上游 ID（由 adapter 侧保证不写随机 conv-id）
	id := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", []byte(`{}`))
	if id.upstreamID != "" || id.affinityKey != "" {
		t.Fatalf("empty signal must not invent session: %#v", id)
	}
}

func TestOfficialCache_ClaudeCodeSessionHeaderPath(t *testing.T) {
	// 模拟 handler 已把 X-Claude-Code-Session-Id 抽成 seed
	seed := "123e4567-e89b-12d3-a456-426614174000"
	a := resolveBuildSessionIdentity(1, accountdomain.ProviderBuild, "grok-4.5", "", seed, nil)
	b := resolveBuildSessionIdentity(1, accountdomain.ProviderBuild, "grok-4.5", "", seed, []byte(`{"messages":[{"role":"user","content":"x"}]}`))
	if a.upstreamID == "" || a.upstreamID != b.upstreamID {
		t.Fatalf("claude session seed unstable: %#v %#v", a, b)
	}
}
