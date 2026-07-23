package provider

import (
	"errors"
	"strings"
	"testing"
)

type credentialJSONTestEntry struct {
	Token string `json:"token"`
}

func TestDecodeCredentialJSONEntriesAcceptsDocumentAndJSONLines(t *testing.T) {
	data := []byte("\xef\xbb\xbf{\n  \"provider\": \"grok_test\",\n  \"accounts\": [{\"token\":\"one\"}]\n}\r\n\r\n{\"token\":\"two\"}\r\n")
	values, err := DecodeCredentialJSONEntries[credentialJSONTestEntry](data, "grok_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Token != "one" || values[1].Token != "two" {
		t.Fatalf("values = %#v", values)
	}
}

func TestDecodeCredentialJSONEntriesReportsMalformedLineWithoutContent(t *testing.T) {
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte("{\"token\":\"safe\"}\n\nnot-json-secret\n"), "grok_test", 10)
	if err == nil || !strings.Contains(err.Error(), "第 3 行") {
		t.Fatalf("error = %v, want line number", err)
	}
	if strings.Contains(err.Error(), "not-json-secret") || strings.Contains(err.Error(), "safe") {
		t.Fatalf("error exposes import contents: %v", err)
	}
}

func TestDecodeCredentialJSONEntriesRejectsNonObjects(t *testing.T) {
	for _, data := range []string{`[{"token":"one"}]`, `"token"`, `null`} {
		if _, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(data), "grok_test", 10); err == nil || !strings.Contains(err.Error(), "必须是 JSON 对象") {
			t.Fatalf("data = %s, error = %v", data, err)
		}
	}
}

func TestDecodeCredentialJSONEntriesEnforcesLimitAcrossValues(t *testing.T) {
	data := []byte("{\"accounts\":[{\"token\":\"one\"}]}\n{\"token\":\"two\"}\n")
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry](data, "grok_test", 1)
	if !errors.Is(err, ErrCredentialLimit) {
		t.Fatalf("error = %v, want credential limit", err)
	}
}

func TestDecodeCredentialJSONEntriesRejectsWrongProvider(t *testing.T) {
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(`{"provider":"other","token":"one"}`), "grok_test", 10)
	if err == nil || !strings.Contains(err.Error(), "Provider 必须是 grok_test") {
		t.Fatalf("error = %v", err)
	}
}
