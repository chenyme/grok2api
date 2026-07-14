package gateway

import (
	"errors"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// 构造一个估算超过 50 万 token 的 body，断言 EstimateRequestInputTokens 判定超限。
func TestContextOverLimitDetection(t *testing.T) {
	// 约 200 万字符 → 估算远超 50 万 token(~3 字符/token)。
	huge := strings.Repeat("word ", 400000)
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"` + huge + `"}]}`)
	est := audit.EstimateRequestInputTokens(body)
	if est <= maxUpstreamContextTokens {
		t.Fatalf("expected estimate > %d, got %d", maxUpstreamContextTokens, est)
	}
}

// 正常大小 body 不应被判定超限。
func TestContextUnderLimitPasses(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hello world"}]}`)
	est := audit.EstimateRequestInputTokens(body)
	if est > maxUpstreamContextTokens {
		t.Fatalf("normal request wrongly flagged: est=%d", est)
	}
}

// ContextOverLimitError 应携带估算值且 errors.As 可提取。
func TestContextOverLimitErrorCarriesTokens(t *testing.T) {
	err := error(&ContextOverLimitError{EstimatedTokens: 500724, Limit: maxUpstreamContextTokens})
	var target *ContextOverLimitError
	if !errors.As(err, &target) {
		t.Fatal("errors.As should extract ContextOverLimitError")
	}
	if target.EstimatedTokens != 500724 {
		t.Fatalf("estimated tokens = %d, want 500724", target.EstimatedTokens)
	}
	if !strings.Contains(err.Error(), "500724") {
		t.Fatalf("error message should contain token count: %s", err.Error())
	}
}
