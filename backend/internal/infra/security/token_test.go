package security

import "testing"

func TestClientKeyFormat(t *testing.T) {
	raw := FormatClientKey("abc123", "secret_value")
	if raw != "g2a_abc123_secret_value" {
		t.Fatalf("formatted key = %q", raw)
	}
	prefix, ok := SplitClientKey(raw)
	if !ok || prefix != "abc123" {
		t.Fatalf("SplitClientKey(%q) = %q, %v", raw, prefix, ok)
	}
	for _, value := range []string{"", "g2a_", "g2a__secret", "other_abc123_secret", "gbp_abc123_old_secret"} {
		if _, ok := SplitClientKey(value); ok {
			t.Fatalf("SplitClientKey(%q) unexpectedly succeeded", value)
		}
	}
}

func TestClientKeyFingerprintContract(t *testing.T) {
	const raw = "g2a_abc123_secret_value"
	const expected = "5693f66e0ebd7c354afba4941329b6d3f8387e77d196eb00e19cb8f15a5e3302"
	if actual := ClientKeyFingerprint(raw); actual != expected {
		t.Fatalf("fingerprint = %q, want %q", actual, expected)
	}
	if ClientKeyFingerprint(raw) == HashToken(raw) {
		t.Fatal("fingerprint must remain domain-separated from the verifier hash")
	}
}
