package egress

import "testing"

func TestParseEgressProbeTrace(t *testing.T) {
	address, country, err := parseEgressProbeTrace([]byte("fl=abc\nip=2001:db8::1\nloc=de\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := address.String(); got != "2001:db8::1" {
		t.Fatalf("address = %q", got)
	}
	if country != "DE" {
		t.Fatalf("country = %q", country)
	}
}

func TestParseEgressProbeTraceRejectsMissingIP(t *testing.T) {
	if _, _, err := parseEgressProbeTrace([]byte("loc=US\n")); err == nil {
		t.Fatal("expected missing IP to fail")
	}
}
