package reasoningreplay

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func validEncrypted(seed byte) string {
	return strings.Repeat(string(rune('A'+seed%26)), 40)
}

func TestNormalizeAndStoreRoundTrip(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(1)
	payload := []byte(`{"id":"resp_1","output":[{"type":"reasoning","encrypted_content":"` + enc + `","summary":[{"type":"summary_text","text":"think"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`)
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-a", payload)

	body := []byte(`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"next"}]}`)
	updated := replay.Apply(context.Background(), "grok-4.5", "session-a", body)
	var got struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(updated, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Input) < 2 {
		t.Fatalf("input len = %d, body=%s", len(got.Input), updated)
	}
	found := false
	for _, item := range got.Input {
		if item["type"] == "reasoning" && item["encrypted_content"] == enc {
			found = true
		}
	}
	if !found {
		t.Fatalf("encrypted reasoning not injected: %s", updated)
	}
}

func TestApplySkipsPreviousResponseID(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(2)
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-b", []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+enc+`"}]}`))
	body := []byte(`{"previous_response_id":"resp_old","input":[{"type":"message","role":"user","content":"x"}]}`)
	updated := replay.Apply(context.Background(), "grok-4.5", "session-b", body)
	if strings.Contains(string(updated), enc) {
		t.Fatalf("should skip when previous_response_id present: %s", updated)
	}
}

func TestFilterRejectsAssistantMismatch(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(3)
	payload := []byte(`{"output":[{"type":"reasoning","encrypted_content":"` + enc + `"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"cached-answer"}]}]}`)
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-c", payload)
	body := []byte(`{"input":[{"type":"message","role":"user","content":"q"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"different"}]},{"type":"message","role":"user","content":"next"}]}`)
	updated := replay.Apply(context.Background(), "grok-4.5", "session-c", body)
	if strings.Contains(string(updated), enc) {
		t.Fatalf("mismatched assistant should block inject: %s", updated)
	}
}

func TestNoAnchorDeletesPrevious(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(4)
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-d", []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+enc+`"}]}`))
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-d", []byte(`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"only message"}]}]}`))
	body := []byte(`{"input":[{"type":"message","role":"user","content":"x"}]}`)
	updated := replay.Apply(context.Background(), "grok-4.5", "session-d", body)
	if strings.Contains(string(updated), enc) {
		t.Fatalf("no-anchor completion should clear cache: %s", updated)
	}
}

func TestCaptureBodyStoresNonStream(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(5)
	raw := []byte(`{"output":[{"type":"reasoning","encrypted_content":"` + enc + `"}]}`)
	body := io.NopCloser(strings.NewReader(string(raw)))
	wrapped := replay.CaptureBody(body, "grok-4.5", "session-e", false, false)
	data, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrapped.Close(); err != nil {
		t.Fatal(err)
	}
	if string(data) != string(raw) {
		t.Fatalf("body mutated: %s", data)
	}
	updated := replay.Apply(context.Background(), "grok-4.5", "session-e", []byte(`{"input":[{"type":"message","role":"user","content":"n"}]}`))
	if !strings.Contains(string(updated), enc) {
		t.Fatalf("capture did not store: %s", updated)
	}
}

func TestDisabledNoOp(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: false, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(6)
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-f", []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+enc+`"}]}`))
	body := []byte(`{"input":[{"type":"message","role":"user","content":"x"}]}`)
	if updated := replay.Apply(context.Background(), "grok-4.5", "session-f", body); string(updated) != string(body) {
		t.Fatalf("disabled should no-op: %s", updated)
	}
}

func TestMemoryTTLExpire(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	now := time.Now().UTC()
	if err := store.Set(context.Background(), "m", "s", [][]byte{[]byte(`{"type":"reasoning"}`)}, now.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok, err := store.Get(context.Background(), "m", "s", time.Now().UTC()); err != nil || ok {
		t.Fatalf("expected expired miss ok=%v err=%v", ok, err)
	}
}
