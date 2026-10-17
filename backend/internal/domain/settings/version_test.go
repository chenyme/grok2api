package settings

import "testing"

func TestCompareSemanticVersion(t *testing.T) {
	tests := []struct {
		v1      string
		v2      string
		wantCmp int
		wantOK  bool
	}{
		{"1.0.4", "1.0.13", -1, true},
		{"1.0.13", "1.0.13", 0, true},
		{"1.0.40", "1.0.13", 1, true},
		{"0.2.102", "1.0.13", -1, true},
		{"v1.0.13", "1.0.13", 0, true},
		{"1.0.14", "1.0.13", 1, true},
		{"2.0.0", "1.0.13", 1, true},
		{"invalid", "1.0.13", 0, false},
		{"", "1.0.13", 0, false},
	}

	for _, tt := range tests {
		cmp, ok := CompareSemanticVersion(tt.v1, tt.v2)
		if ok != tt.wantOK {
			t.Errorf("CompareSemanticVersion(%q, %q) ok = %v, want %v", tt.v1, tt.v2, ok, tt.wantOK)
		}
		if ok && cmp != tt.wantCmp {
			t.Errorf("CompareSemanticVersion(%q, %q) = %d, want %d", tt.v1, tt.v2, cmp, tt.wantCmp)
		}
	}
}

func TestIsBuildClientVersionOutdated(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"", true},
		{"invalid", true},
		{"0.2.99", true},
		{"0.2.102", true},
		{"1.0.4", true},
		{"1.0.12", true},
		{"1.0.13", false},
		{"1.0.14", false},
		{"1.0.40", false},
		{"2.0.0", false},
	}

	for _, tt := range tests {
		if got := IsBuildClientVersionOutdated(tt.version); got != tt.want {
			t.Errorf("IsBuildClientVersionOutdated(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

func TestNormalizeBuildClientVersionAndUserAgent(t *testing.T) {
	// Outdated versions get upgraded to recommended
	v, ua := NormalizeBuildClientVersionAndUserAgent("1.0.4", "grok-shell/1.0.4 (linux; x86_64)")
	if v != RecommendedBuildClientVersion || ua != RecommendedBuildUserAgent {
		t.Fatalf("expected recommended, got %q, %q", v, ua)
	}

	v, ua = NormalizeBuildClientVersionAndUserAgent("0.2.102", "")
	if v != RecommendedBuildClientVersion || ua != RecommendedBuildUserAgent {
		t.Fatalf("expected recommended, got %q, %q", v, ua)
	}

	v, ua = NormalizeBuildClientVersionAndUserAgent("", "")
	if v != RecommendedBuildClientVersion || ua != RecommendedBuildUserAgent {
		t.Fatalf("expected recommended, got %q, %q", v, ua)
	}

	// Valid versions >= MinimumBuildClientVersion are preserved
	v, ua = NormalizeBuildClientVersionAndUserAgent("1.0.40", "custom-agent")
	if v != "1.0.40" || ua != "custom-agent" {
		t.Fatalf("expected custom preserved, got %q, %q", v, ua)
	}

	v, ua = NormalizeBuildClientVersionAndUserAgent("1.0.13", "")
	if v != "1.0.13" || ua != "grok-shell/1.0.13 (linux; x86_64)" {
		t.Fatalf("expected auto-filled user agent, got %q, %q", v, ua)
	}
}
