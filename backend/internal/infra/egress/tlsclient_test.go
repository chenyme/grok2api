package egress

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
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

func TestFromFHTTPResponseNormalizesAutoDecompressedHeaders(t *testing.T) {
	response := fromFHTTPResponse(&fhttp.Response{
		Status: "200 OK", StatusCode: http.StatusOK, Proto: "HTTP/2.0", ProtoMajor: 2,
		Header: fhttp.Header{
			"Content-Encoding": []string{"gzip"},
			"Content-Length":   []string{"128"},
			"Content-Type":     []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(`{"status":"completed"}`)), ContentLength: 128, Uncompressed: true,
	})
	if response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Length") != "" {
		t.Fatalf("decoded response headers = %#v", response.Header)
	}
	if response.ContentLength != -1 || !response.Uncompressed {
		t.Fatalf("contentLength=%d uncompressed=%v", response.ContentLength, response.Uncompressed)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(data, []byte(`{"status":"completed"}`)) {
		t.Fatalf("body=%q err=%v", data, err)
	}
}

func TestFromFHTTPResponsePreservesCompressedHeaders(t *testing.T) {
	response := fromFHTTPResponse(&fhttp.Response{
		Status: "200 OK", StatusCode: http.StatusOK,
		Header: fhttp.Header{"Content-Encoding": []string{"gzip"}, "Content-Length": []string{"128"}},
		Body:   io.NopCloser(bytes.NewReader(nil)), ContentLength: 128,
	})
	if response.Header.Get("Content-Encoding") != "gzip" || response.Header.Get("Content-Length") != "128" || response.ContentLength != 128 {
		t.Fatalf("compressed response = headers=%#v contentLength=%d", response.Header, response.ContentLength)
	}
}
