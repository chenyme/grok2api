package buildtransport

import (
	"net/http"
	"testing"
)

func TestConfigureHTTP2HealthEnablesActivePing(t *testing.T) {
	transport := &http.Transport{ForceAttemptHTTP2: true}
	h2, err := ConfigureHTTP2Health(transport)
	if err != nil {
		t.Fatal(err)
	}
	if h2.ReadIdleTimeout != HTTP2ReadIdleTimeout || h2.PingTimeout != HTTP2PingTimeout {
		t.Fatalf("HTTP/2 health = (%s, %s)", h2.ReadIdleTimeout, h2.PingTimeout)
	}
	if transport.TLSNextProto["h2"] == nil {
		t.Fatal("HTTP/2 transport was not installed")
	}
}

func TestConfigureHTTP2HealthRejectsNilTransport(t *testing.T) {
	if _, err := ConfigureHTTP2Health(nil); err == nil {
		t.Fatal("nil transport was accepted")
	}
}
