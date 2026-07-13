package egress

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

func TestToFHTTPRequestPreservesRequestFraming(t *testing.T) {
	payload := []byte(`{"message":"hello"}`)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://grok.com/rest/test", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "grok.com"
	request.Header.Set("Content-Type", "application/json")

	converted, err := toFHTTPRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if converted.ContentLength != int64(len(payload)) || len(converted.TransferEncoding) != 0 {
		t.Fatalf("contentLength=%d transferEncoding=%v", converted.ContentLength, converted.TransferEncoding)
	}
	if converted.Host != request.Host || converted.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("host=%q headers=%v", converted.Host, converted.Header)
	}
	if converted.GetBody == nil {
		t.Fatal("GetBody was not preserved")
	}
	body, err := converted.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("body=%q err=%v", got, err)
	}
}
