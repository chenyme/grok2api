package gateway

import (
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

func TestResolvePromptCacheIdentityIsStableAndIsolated(t *testing.T) {
	base := resolvePromptCacheIdentity(7, accountdomain.ProviderBuild, "grok-4.5", audit.OperationMessages, "", "session-1")
	if len(base) != 36 || base != resolvePromptCacheIdentity(7, accountdomain.ProviderBuild, "grok-4.5", audit.OperationMessages, "", "session-1") {
		t.Fatalf("unstable identity = %q", base)
	}
	for name, value := range map[string]string{
		"client":    resolvePromptCacheIdentity(8, accountdomain.ProviderBuild, "grok-4.5", audit.OperationMessages, "", "session-1"),
		"provider":  resolvePromptCacheIdentity(7, accountdomain.ProviderConsole, "grok-4.5", audit.OperationMessages, "", "session-1"),
		"model":     resolvePromptCacheIdentity(7, accountdomain.ProviderBuild, "grok-4.3", audit.OperationMessages, "", "session-1"),
		"operation": resolvePromptCacheIdentity(7, accountdomain.ProviderBuild, "grok-4.5", audit.OperationResponses, "", "session-1"),
		"session":   resolvePromptCacheIdentity(7, accountdomain.ProviderBuild, "grok-4.5", audit.OperationMessages, "", "session-2"),
	} {
		if value == base {
			t.Fatalf("%s was not isolated: %q", name, value)
		}
	}
}

func TestResolvePromptCacheIdentityPrefersExplicitKey(t *testing.T) {
	first := resolvePromptCacheIdentity(7, accountdomain.ProviderBuild, "grok-4.5", audit.OperationResponses, "client-key", "session-1")
	second := resolvePromptCacheIdentity(7, accountdomain.ProviderBuild, "grok-4.5", audit.OperationResponses, "client-key", "session-2")
	if first == "" || first != second {
		t.Fatalf("explicit key did not take precedence: first=%q second=%q", first, second)
	}
	if resolvePromptCacheIdentity(0, accountdomain.ProviderBuild, "grok-4.5", audit.OperationResponses, "client-key", "") != "" {
		t.Fatal("identity without client ownership should be empty")
	}
	if resolvePromptCacheIdentity(7, accountdomain.ProviderBuild, "grok-4.5", audit.OperationResponses, "", "") != "" {
		t.Fatal("identity without an explicit key or session should be empty")
	}
}
