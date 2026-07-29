package app

import (
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestClearanceConfigUsesImaginePage(t *testing.T) {
	cfg := config.Config{Provider: config.ProviderConfig{Web: config.WebProviderConfig{
		BaseURL: "https://grok.com/", ClearanceTimeout: config.Duration(time.Minute),
		ClearanceRefresh: config.Duration(10 * time.Minute),
	}}}
	actual := clearanceConfig(cfg)
	if actual.TargetURL != "https://grok.com/imagine" {
		t.Fatalf("clearance target = %q", actual.TargetURL)
	}
}
