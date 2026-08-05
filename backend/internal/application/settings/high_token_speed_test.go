package settings

import "testing"

func TestNormalizeBuildHighTokenSpeedModelIDs(t *testing.T) {
	got := normalizeBuildHighTokenSpeedModelIDs([]string{" grok-4.20 ", "Grok-4.20", "", "grok-4.5"})
	if len(got) != 2 || got[0] != "grok-4.20" || got[1] != "grok-4.5" {
		t.Fatalf("got %#v", got)
	}
	if normalizeBuildHighTokenSpeedModelIDs(nil) != nil {
		t.Fatal("nil input should stay nil")
	}
}
