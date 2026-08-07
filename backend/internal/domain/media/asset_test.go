package media

import "testing"

func TestInputReferenceRoundTripAndValidation(t *testing.T) {
	fileID := "input_abcdefghijklmnopqrstuvwxyz012345"
	if !IsInputAssetID(fileID) {
		t.Fatalf("generated-shape input ID rejected: %q", fileID)
	}
	parsed, ok := ParseInputReference(InputReference(fileID))
	if !ok || parsed != fileID {
		t.Fatalf("parsed=%q ok=%t", parsed, ok)
	}
	for _, invalid := range []string{"input_short", "img_abcdefghijklmnopqrstuvwxyz012345", "input_../../etc/passwd", "grok2api-input:"} {
		if IsInputAssetID(invalid) {
			t.Errorf("invalid input ID accepted: %q", invalid)
		}
	}
}
