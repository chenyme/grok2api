package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func makeResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 上游 400 含 maximum prompt length → 改写成 /compact 引导，保留精确 token 数。
func TestRewriteContextLimitResponse(t *testing.T) {
	up := `{"error":{"message":"This model's maximum prompt length is 500000 but the request contains 500724 tokens"}}`
	resp, ok := rewriteContextLimitResponse(makeResp(400, up))
	if !ok {
		t.Fatal("should rewrite context-limit 400")
	}
	data, _ := io.ReadAll(resp.Body)
	s := string(data)
	if !strings.Contains(s, "/compact") {
		t.Fatalf("rewritten message must guide /compact: %s", s)
	}
	if !strings.Contains(s, "500724") {
		t.Fatalf("must preserve upstream token count: %s", s)
	}
	if !strings.Contains(s, "invalid_request_error") {
		t.Fatalf("must be invalid_request_error: %s", s)
	}
}

// 无关的 400 不应被改写，body 原样保留。
func TestRewriteContextLimitIgnoresOther400(t *testing.T) {
	up := `{"error":{"message":"some other bad request"}}`
	_, ok := rewriteContextLimitResponse(makeResp(400, up))
	if ok {
		t.Fatal("non-context-limit 400 must not be rewritten")
	}
}

func TestExtractUpstreamTokenCount(t *testing.T) {
	n := extractUpstreamTokenCount([]byte("the request contains 500724 tokens"))
	if n != 500724 {
		t.Fatalf("extracted %d, want 500724", n)
	}
	if extractUpstreamTokenCount([]byte("no number here")) != 0 {
		t.Fatal("should return 0 when no count")
	}
}
