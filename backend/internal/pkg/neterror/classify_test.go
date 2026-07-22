package neterror

import (
	"context"
	"errors"
	"net/url"
	"testing"
)

type timeoutError string

func (e timeoutError) Error() string { return string(e) }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIsResponseHeaderTimeout(t *testing.T) {
	wrapped := &url.Error{Op: "Post", URL: "https://example.test/v1/responses", Err: timeoutError("http2: timeout awaiting response headers")}
	if !IsResponseHeaderTimeout(wrapped) {
		t.Fatal("HTTP/2 response-header timeout was not recognized")
	}
	for _, err := range []error{
		context.DeadlineExceeded,
		timeoutError("TLS handshake timeout"),
		errors.New("timeout awaiting response headers"),
	} {
		if IsResponseHeaderTimeout(err) {
			t.Fatalf("unexpected response-header timeout classification for %v", err)
		}
	}
}
