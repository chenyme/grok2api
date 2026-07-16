package web

import (
	"net/http"
	"testing"
)

func TestResponseUpstreamURLHandlesResponsesWithoutRequestMetadata(t *testing.T) {
	response := &http.Response{}
	if got := responseUpstreamURL(response); got != "" {
		t.Fatalf("responseUpstreamURL() = %q, want empty string", got)
	}
}

func TestResponseUpstreamURLReturnsRequestURL(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "https://grok.com/rest/app-chat/conversations/new", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := &http.Response{Request: request}
	if got := responseUpstreamURL(response); got != request.URL.String() {
		t.Fatalf("responseUpstreamURL() = %q, want %q", got, request.URL.String())
	}
}