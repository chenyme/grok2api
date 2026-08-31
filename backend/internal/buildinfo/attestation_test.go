package buildinfo

import "testing"

func TestCurrentAttestationFailsClosedAndAcceptsExactReleaseIdentity(t *testing.T) {
	oldVersion, oldCommit, oldFingerprint := Version, SourceCommit, BuildFingerprint
	t.Cleanup(func() { Version, SourceCommit, BuildFingerprint = oldVersion, oldCommit, oldFingerprint })
	t.Setenv(runtimeImageDigestEnv, "")
	Version, SourceCommit, BuildFingerprint = "", "", ""
	if _, err := CurrentAttestation(); err == nil {
		t.Fatal("missing build identity was accepted")
	}
	Version = "v3.1.4+cheapai.2"
	SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	BuildFingerprint = expectedBuildFingerprint(SourceCommit, Version)
	if BuildFingerprint != "3cabb872cd68fbca5a8b412974453a9ef932c8f1a5260c7c3a158c844d166ecf" {
		t.Fatalf("build fingerprint contract = %q", BuildFingerprint)
	}
	t.Setenv(runtimeImageDigestEnv, "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	value, err := CurrentAttestation()
	if err != nil {
		t.Fatal(err)
	}
	if value.SourceCommit != SourceCommit || value.BuildFingerprint != BuildFingerprint || value.RuntimeImageDigest == "" {
		t.Fatalf("attestation = %#v", value)
	}
	t.Setenv(runtimeImageDigestEnv, "sha256:ABCDEF")
	if _, err := CurrentAttestation(); err == nil {
		t.Fatal("malformed image digest was accepted")
	}
	t.Setenv(runtimeImageDigestEnv, "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	BuildFingerprint = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if _, err := CurrentAttestation(); err == nil {
		t.Fatal("unbound build fingerprint was accepted")
	}
	BuildFingerprint = expectedBuildFingerprint(SourceCommit, Version)
	SourceCommit = " " + SourceCommit
	if _, err := CurrentAttestation(); err == nil {
		t.Fatal("whitespace-normalized source commit was accepted")
	}
}
