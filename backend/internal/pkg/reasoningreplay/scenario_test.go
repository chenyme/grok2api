package reasoningreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// 模拟真实 Grok Build Responses 完成体 + Claude Code 式第二轮（不带 encrypted）。
func TestScenario_RealTwoTurnHit(t *testing.T) {
	store := memory.NewReasoningReplayStore(1024)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	ctx := context.Background()

	// 与 resolvePromptCacheIdentity 形态接近的隔离 key
	session := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	model := "grok-4.5"
	enc := validEncrypted(13)

	// Turn1 上游成功响应（非流式）
	turn1Upstream := fmt.Sprintf(`{
	  "id": "resp_turn1",
	  "object": "response",
	  "model": "%s",
	  "output": [
	    {
	      "type": "reasoning",
	      "id": "rs_1",
	      "summary": [{"type": "summary_text", "text": "用户在问互斥锁"}],
	      "content": null,
	      "encrypted_content": %q
	    },
	    {
	      "type": "message",
	      "id": "msg_1",
	      "role": "assistant",
	      "content": [{"type": "output_text", "text": "互斥锁用于保护共享资源，同一时刻只允许一个线程进入临界区。"}]
	    }
	  ],
	  "usage": {"input_tokens": 20, "output_tokens": 30, "total_tokens": 50}
	}`, model, enc)

	// 模拟 adapter CaptureBody：读完再 Close
	body := io.NopCloser(bytes.NewReader([]byte(turn1Upstream)))
	captured := replay.CaptureBody(body, model, session, false, false)
	if _, err := io.Copy(io.Discard, captured); err != nil {
		t.Fatal(err)
	}
	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}

	// Turn2 客户端故意不带 encrypted_content，只带明文历史
	turn2Client := []byte(`{
	  "model": "grok-4.5",
	  "prompt_cache_key": "client-visible-key",
	  "input": [
	    {"type": "message", "role": "user", "content": "用一句话解释什么是互斥锁"},
	    {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "互斥锁用于保护共享资源，同一时刻只允许一个线程进入临界区。"}]},
	    {"type": "message", "role": "user", "content": "那死锁呢？一句话"}
	  ]
	}`)

	// 模拟 adapter：inject prompt_cache_key 之后 Apply
	outbound := replay.Apply(ctx, model, session, turn2Client)

	var got struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(outbound, &got); err != nil {
		t.Fatal(err)
	}

	// 硬标准：出站 input 含客户端没发的 encrypted_content
	if strings.Contains(string(turn2Client), enc) {
		t.Fatal("precondition failed: client body should not contain encrypted")
	}
	if !strings.Contains(string(outbound), enc) {
		t.Fatalf("HIT failed: outbound missing encrypted\nclient=%s\noutbound=%s", turn2Client, outbound)
	}

	// reasoning 应插在最后一条 user 之前（最后一个 assistant 之后）
	var types []string
	for _, item := range got.Input {
		types = append(types, fmt.Sprint(item["type"]))
	}
	// 期望至少包含 reasoning
	hasReasoning := false
	for _, typ := range types {
		if typ == "reasoning" {
			hasReasoning = true
			break
		}
	}
	if !hasReasoning {
		t.Fatalf("types=%v", types)
	}
	t.Logf("PASS real two-turn HIT; input types=%v", types)
}

func TestScenario_SSEStreamCompletedStores(t *testing.T) {
	store := memory.NewReasoningReplayStore(256)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	ctx := context.Background()
	enc := validEncrypted(7)
	session, model := "sse-session", "grok-4.5"

	// 真实 SSE：完整 output items 在 done 事件中，completed.output 为空。
	sse := strings.Join([]string{
		`event: response.output_item.done`,
		fmt.Sprintf(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":%q}}`, enc),
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_sse","output":[]}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	wrapped := replay.CaptureBody(io.NopCloser(strings.NewReader(sse)), model, session, true, false)
	if _, err := io.Copy(io.Discard, wrapped); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.Close(); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"input":[{"type":"message","role":"user","content":"continue"}]}`)
	out := replay.Apply(ctx, model, session, body)
	if !strings.Contains(string(out), enc) {
		t.Fatalf("SSE completed should store; out=%s", out)
	}
	t.Log("PASS SSE stream store+apply HIT")
}

func TestScenario_TenantIsolation(t *testing.T) {
	store := memory.NewReasoningReplayStore(256)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	ctx := context.Background()
	encA, encB := validEncrypted(8), validEncrypted(9)
	model := "grok-4.5"

	replay.StoreFromCompleted(ctx, model, "tenant-A-key", []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+encA+`"}]}`))
	replay.StoreFromCompleted(ctx, model, "tenant-B-key", []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+encB+`"}]}`))

	outA := replay.Apply(ctx, model, "tenant-A-key", []byte(`{"input":[{"type":"message","role":"user","content":"x"}]}`))
	outB := replay.Apply(ctx, model, "tenant-B-key", []byte(`{"input":[{"type":"message","role":"user","content":"x"}]}`))

	if !strings.Contains(string(outA), encA) || strings.Contains(string(outA), encB) {
		t.Fatalf("A isolation failed: %s", outA)
	}
	if !strings.Contains(string(outB), encB) || strings.Contains(string(outB), encA) {
		t.Fatalf("B isolation failed: %s", outB)
	}
	t.Log("PASS tenant isolation")
}

func TestScenario_CompactClearsCache(t *testing.T) {
	store := memory.NewReasoningReplayStore(64)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	ctx := context.Background()
	enc := validEncrypted(10)
	model, session := "grok-4.5", "compact-sess"

	replay.StoreFromCompleted(ctx, model, session, []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+enc+`"}]}`))

	// compact 成功响应经过 CaptureBody(compact=true)
	compactBody := `{"id":"resp_compact","output":[{"type":"compaction","encrypted_content":"opaque"}]}`
	wrapped := replay.CaptureBody(io.NopCloser(strings.NewReader(compactBody)), model, session, false, true)
	_, _ = io.Copy(io.Discard, wrapped)
	_ = wrapped.Close()

	out := replay.Apply(ctx, model, session, []byte(`{"input":[{"type":"message","role":"user","content":"after compact"}]}`))
	if strings.Contains(string(out), enc) {
		t.Fatalf("compact should clear replay: %s", out)
	}
	t.Log("PASS compact clears")
}

func TestScenario_ToolCallInjectOnlyWithMatchingOutput(t *testing.T) {
	store := memory.NewReasoningReplayStore(64)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	ctx := context.Background()
	enc := validEncrypted(11)
	model, session := "grok-4.5", "tool-sess"

	// 上一轮：reasoning + function_call（合法 JSON）
	payload, err := json.Marshal(map[string]any{
		"output": []any{
			map[string]any{"type": "reasoning", "encrypted_content": enc},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	replay.StoreFromCompleted(ctx, model, session, payload)
	stored, ok, storeErr := store.Get(ctx, model, session, time.Now().UTC(), time.Hour)
	if storeErr != nil || !ok || len(stored) == 0 {
		t.Fatalf("store miss: ok=%v err=%v items=%d payload=%s", ok, storeErr, len(stored), payload)
	}
	t.Logf("stored %d items", len(stored))

	// 客户端带回 tool 结果，但没带 function_call 与 reasoning
	body, err := json.Marshal(map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "查一下"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "result"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := replay.Apply(ctx, model, session, body)
	if !strings.Contains(string(out), enc) {
		t.Fatalf("should inject reasoning: stored=%d out=%s", len(stored), out)
	}
	if !strings.Contains(string(out), `"type":"function_call"`) && !strings.Contains(string(out), `"type": "function_call"`) {
		// json.Marshal 可能无空格
		if !strings.Contains(string(out), "function_call") || !strings.Contains(string(out), "lookup") {
			t.Fatalf("should inject function_call before output: %s", out)
		}
	}

	// 无 matching output 时 function_call 不应注入（filter 要求有 output）
	body2, _ := json.Marshal(map[string]any{
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "no tool result"}},
	})
	out2 := replay.Apply(ctx, model, session, body2)
	if strings.Contains(string(out2), "lookup") {
		t.Fatalf("function_call without output must not inject: %s", out2)
	}
	// reasoning 仍应可注入
	if !strings.Contains(string(out2), enc) {
		t.Fatalf("reasoning should still inject without tool output: %s", out2)
	}
	t.Log("PASS tool-call filter rules")
}

func TestScenario_WrongSessionMiss(t *testing.T) {
	store := memory.NewReasoningReplayStore(32)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	ctx := context.Background()
	enc := validEncrypted(12)
	replay.StoreFromCompleted(ctx, "grok-4.5", "sess-1", []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+enc+`"}]}`))
	out := replay.Apply(ctx, "grok-4.5", "sess-2", []byte(`{"input":[{"type":"message","role":"user","content":"x"}]}`))
	if strings.Contains(string(out), enc) {
		t.Fatal("wrong session must miss")
	}
	t.Log("PASS wrong session miss")
}

func TestScenario_ConfigDefaultsEnableReplay(t *testing.T) {
	// 与 config.defaultConfig 一致：Enabled 默认 true
	store := memory.NewReasoningReplayStore(16)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	if !replay.Enabled() {
		t.Fatal("expected enabled")
	}
	t.Log("PASS default enabled")
}
