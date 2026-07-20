package settings

import (
	"encoding/json"
	"strings"
	"testing"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
)

func TestSettingsDTOExcludesBrowserIdentityFields(t *testing.T) {
	data, err := json.Marshal(settingsConfigDTO{})
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(data))
	for _, forbidden := range []string{"grok_device_id", "x-anonuserid", "x-userid", "x-challenge", "x-signature"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("settings response contains forbidden field %q", forbidden)
		}
	}
}

func TestSettingsResponseDoesNotExposeManualStatsigValue(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{ProviderWeb: settingsapp.ProviderWebConfig{
		StatsigMode: "manual", StatsigManualValue: "must-not-leak", StatsigManualConfigured: true,
	}}})
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "must-not-leak") || strings.Contains(string(data), "statsigManualValue") {
		t.Fatalf("settings response leaked manual Statsig: %s", data)
	}
}

func TestSettingsResponseIncludesBuildTokenAuth(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{ProviderBuild: settingsapp.ProviderBuildConfig{TokenAuth: "xai-grok-cli"}}})
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"tokenAuth":"xai-grok-cli"`) || !strings.Contains(string(data), `"tokenAuthConfigured":true`) {
		t.Fatalf("settings response lost Build token auth: %s", data)
	}
}

func TestSettingsResponseIncludesRecommendedBuildBaseline(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{RecommendedProviderBuild: settingsapp.ProviderBuildRecommendation{
		ClientVersion: "0.2.106", UserAgent: "grok-shell/0.2.106 (linux; x86_64)",
	}})
	if response.RecommendedProviderBuild.ClientVersion != "0.2.106" || response.RecommendedProviderBuild.UserAgent == "" {
		t.Fatalf("recommended build = %#v", response.RecommendedProviderBuild)
	}
}

func TestSettingsResponseIncludesPreferFreeBuild(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{
		Routing: settingsapp.RoutingConfig{PreferFreeBuild: true},
	}})
	if !response.Config.Routing.PreferFreeBuild {
		t.Fatal("preferFreeBuild was lost from settings response")
	}
}
