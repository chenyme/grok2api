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
		ClientVersion: "0.2.110", UserAgent: "grok-shell/0.2.110 (linux; x86_64)",
	}})
	if response.RecommendedProviderBuild.ClientVersion != "0.2.110" || response.RecommendedProviderBuild.UserAgent == "" {
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

func TestLegacySettingsRequestMayOmitAccounts(t *testing.T) {
	var dto settingsConfigDTO
	if err := json.Unmarshal([]byte(`{"server":{"maxConcurrentRequests":64}}`), &dto); err != nil {
		t.Fatal(err)
	}
	input := dto.toApplication()
	if input.AccountsProvided {
		t.Fatal("missing accounts field was treated as an explicit update")
	}
}

func TestLegacySettingsRequestPreservesBuildForbiddenCodesWhenOmitted(t *testing.T) {
	var dto settingsConfigDTO
	if err := json.Unmarshal([]byte(`{"accounts":{"markBuildForbiddenReauth":true,"autoCleanReauthEnabled":false,"autoCleanReauthInterval":"10m","autoCleanReauthMinAge":"1h","autoCleanIncludeDisabled":false}}`), &dto); err != nil {
		t.Fatal(err)
	}
	input := dto.toApplication()
	if !input.AccountsProvided || !input.Accounts.MarkBuildForbiddenReauthProvided || input.Accounts.BuildForbiddenReauthCodesProvided {
		t.Fatalf("legacy field presence was not preserved: %#v", input.Accounts)
	}
}

func TestSettingsResponseIncludesBuildForbiddenCodes(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{
		Accounts: settingsapp.AccountsConfig{
			MarkBuildForbiddenReauth:  true,
			BuildForbiddenReauthCodes: []string{"permission-denied", "team-access-denied"},
		},
	}})
	if response.Config.Accounts == nil || response.Config.Accounts.BuildForbiddenReauthCodes == nil {
		t.Fatal("Build forbidden codes were omitted from the settings response")
	}
	codes := *response.Config.Accounts.BuildForbiddenReauthCodes
	if len(codes) != 2 || codes[0] != "permission-denied" || codes[1] != "team-access-denied" {
		t.Fatalf("Build forbidden codes = %#v", codes)
	}
}

func TestLegacySettingsRequestMayOmitSegmentedSelector(t *testing.T) {
	var dto settingsConfigDTO
	if err := json.Unmarshal([]byte(`{"routing":{"stickyTTL":"1h"}}`), &dto); err != nil {
		t.Fatal(err)
	}
	input := dto.toApplication()
	if input.Routing.SegmentedSelectorProvided {
		t.Fatal("missing segmented selector was treated as an explicit update")
	}
}

func TestSettingsResponseIncludesSegmentedSelector(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{
		Routing: settingsapp.RoutingConfig{SegmentedSelector: settingsapp.SegmentedSelectorConfig{
			Enabled: true, MinCandidates: 3000, WindowSize: 64,
		}},
	}})
	selector := response.Config.Routing.SegmentedSelector
	if selector == nil || !selector.Enabled || selector.MinCandidates != 3000 || selector.WindowSize != 64 {
		t.Fatalf("segmented selector = %#v", selector)
	}
}

func TestLegacySettingsRequestMayOmitManagedClearance(t *testing.T) {
	var dto settingsConfigDTO
	if err := json.Unmarshal([]byte(`{"providerWeb":{"baseURL":"https://grok.com"}}`), &dto); err != nil {
		t.Fatal(err)
	}
	input := dto.toApplication()
	if input.ProviderWeb.ClearanceProvided {
		t.Fatal("missing managed-clearance fields were treated as an explicit update")
	}
}
