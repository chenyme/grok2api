package web

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLimitedVideoBodyRejectsUnknownLengthOverflow(t *testing.T) {
	body := newLimitedVideoBody(io.NopCloser(strings.NewReader("123456")), 5)
	defer body.Close()

	raw, err := io.ReadAll(body)
	if !errors.Is(err, errVideoDownloadTooLarge) {
		t.Fatalf("ReadAll error = %v", err)
	}
	if string(raw) != "12345" {
		t.Fatalf("ReadAll body = %q", raw)
	}
}

func TestLimitedVideoBodyAllowsExactLimit(t *testing.T) {
	body := newLimitedVideoBody(io.NopCloser(strings.NewReader("12345")), 5)
	defer body.Close()

	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "12345" {
		t.Fatalf("ReadAll body = %q", raw)
	}
}
