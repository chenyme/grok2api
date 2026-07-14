package signerurl

import "testing"

func TestValidateAllowsPublicHTTPSAndTrustedInternalSigner(t *testing.T) {
	for _, value := range []string{
		"https://grok.wodf.de/sign",
		"http://grok-signer-go:8788/sign",
		"http://localhost:8788/sign",
		"http://host.docker.internal:8788/sign",
		"http://127.0.0.1:8788/sign",
		"http://10.0.0.8:8788/sign",
		"https://grok-signer-go:8788/sign",
	} {
		if err := Validate(value); err != nil {
			t.Fatalf("%q rejected: %v", value, err)
		}
	}
}

func TestValidateRejectsUnsafeOrMalformedSigner(t *testing.T) {
	for _, value := range []string{
		"http://grok.wodf.de/sign",
		"https://grok.wodf.de:8443/sign",
		"https://user:pass@grok.wodf.de/sign",
		"https://grok.wodf.de/sign?token=value",
		"https://grok.wodf.de/sign#fragment",
		"ftp://grok-signer-go/sign",
		"http://8.8.8.8:8788/sign",
		"grok-signer-go:8788/sign",
	} {
		if err := Validate(value); err == nil {
			t.Fatalf("unsafe URL %q accepted", value)
		}
	}
}
