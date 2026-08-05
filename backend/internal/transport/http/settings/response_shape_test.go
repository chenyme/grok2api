package settings

import (
	"encoding/json"
	"testing"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
)

func TestSettingsResponseNeverEmitsNullModelIDList(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{
		Config: settingsapp.EditableConfig{
			Routing: settingsapp.RoutingConfig{
				StickyTTL: "1h", CooldownBase: "30s", CooldownMax: "30m", CapacityWait: "500ms",
				MaxAttempts: 3,
				// nil slice must still serialize as [] for the admin UI decoder.
				BuildHighTokenSpeedModelIDs: nil,
				SegmentedSelector:           settingsapp.SegmentedSelectorConfig{MinCandidates: 3000, WindowSize: 64},
			},
			ProviderWeb: settingsapp.ProviderWebConfig{
				StatsigMode: "url", ClearanceMode: "manual",
			},
			Accounts: settingsapp.AccountsConfig{
				BuildForbiddenReauthCodes: []string{"permission-denied"},
			},
		},
		RecommendedProviderBuild: settingsapp.ProviderBuildRecommendation{ClientVersion: "0.2.119", UserAgent: "ua"},
		RestartRequired:          []string{},
		Revision:                 1,
	})
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	routing := raw["config"].(map[string]any)["routing"].(map[string]any)
	value, ok := routing["buildHighTokenSpeedModelIDs"]
	if !ok {
		t.Fatal("buildHighTokenSpeedModelIDs missing from settings response")
	}
	if value == nil {
		t.Fatal("buildHighTokenSpeedModelIDs must not be JSON null")
	}
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("buildHighTokenSpeedModelIDs type = %T, want array", value)
	}
	if len(list) != 0 {
		t.Fatalf("empty model list = %#v", list)
	}
}

func TestStringSlicePointerNeverJSONNull(t *testing.T) {
	for _, input := range [][]string{nil, {}, {"grok-4.20"}} {
		data, err := json.Marshal(stringSlicePointer(input))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) == "null" {
			t.Fatalf("input %#v encoded as null", input)
		}
	}
}
