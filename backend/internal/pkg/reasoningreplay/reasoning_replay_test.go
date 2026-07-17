package reasoningreplay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func validEncrypted(seed byte) string {
	buffer := make([]byte, 0, 256)
	for index := 0; len(buffer) < 256; index++ {
		digest := sha256.Sum256([]byte{seed, byte(index), byte(index >> 8)})
		buffer = append(buffer, digest[:]...)
	}
	return base64.RawStdEncoding.EncodeToString(buffer[:256])
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
	if len(got.Input) != 3 {
		t.Fatalf("input len = %d, body=%s", len(got.Input), updated)
	}
	if got.Input[0]["type"] != "reasoning" || got.Input[0]["encrypted_content"] != enc || got.Input[1]["role"] != "assistant" || got.Input[2]["role"] != "user" {
		t.Fatalf("unexpected replay order: %s", updated)
	}
}

func TestApplyInsertsReasoningBeforeMatchingAssistant(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(14)
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-order", []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+enc+`"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`))
	body := []byte(`{"input":[{"type":"message","role":"user","content":"first"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"message","role":"user","content":"next"}]}`)
	updated := replay.Apply(context.Background(), "grok-4.5", "session-order", body)
	var got struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(updated, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 4 || got.Input[1]["type"] != "reasoning" || got.Input[2]["role"] != "assistant" || got.Input[3]["role"] != "user" {
		t.Fatalf("unexpected replay order: %s", updated)
	}
}

func TestApplyAlignsAnthropicVisibleToolCallID(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(15)
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-toolu", []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+enc+`"},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]}`))
	body := []byte(`{"input":[{"type":"function_call_output","call_id":"toolu_call_1","output":"result"}]}`)
	updated := replay.Apply(context.Background(), "grok-4.5", "session-toolu", body)
	var got struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(updated, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 3 || got.Input[0]["type"] != "reasoning" || got.Input[1]["type"] != "function_call" || got.Input[1]["call_id"] != "toolu_call_1" || got.Input[2]["type"] != "function_call_output" {
		t.Fatalf("unexpected tool replay: %s", updated)
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

func TestIncompletePayloadRetainsPreviousReplay(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(16)
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-partial", []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+enc+`"}]}`))
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-partial", []byte(`{"output":[`))
	updated := replay.Apply(context.Background(), "grok-4.5", "session-partial", []byte(`{"input":[{"type":"message","role":"user","content":"next"}]}`))
	if !strings.Contains(string(updated), enc) {
		t.Fatalf("incomplete payload deleted previous replay: %s", updated)
	}
}

func TestCaptureBodyClosedBeforeEOFRetainsPreviousReplay(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(17)
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-short-read", []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+enc+`"}]}`))
	wrapped := replay.CaptureBody(io.NopCloser(strings.NewReader(`{"output":[]}`)), "grok-4.5", "session-short-read", false, false)
	buffer := make([]byte, 4)
	if _, err := wrapped.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.Close(); err != nil {
		t.Fatal(err)
	}
	updated := replay.Apply(context.Background(), "grok-4.5", "session-short-read", []byte(`{"input":[{"type":"message","role":"user","content":"next"}]}`))
	if !strings.Contains(string(updated), enc) {
		t.Fatalf("short read deleted previous replay: %s", updated)
	}
}

func TestUpdateConfigConcurrentWithReplay(t *testing.T) {
	store := memory.NewReasoningReplayStore(100)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(18)
	payload := []byte(`{"output":[{"type":"reasoning","encrypted_content":"` + enc + `"}]}`)
	body := []byte(`{"input":[{"type":"message","role":"user","content":"next"}]}`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < 1000; index++ {
			replay.UpdateConfig(Config{Enabled: index%2 == 0, TTL: time.Hour})
		}
	}()
	for index := 0; index < 1000; index++ {
		replay.StoreFromCompleted(context.Background(), "grok-4.5", "session-config", payload)
		_ = replay.Apply(context.Background(), "grok-4.5", "session-config", body)
	}
	<-done
}

func TestNormalizeReasoningRejectsForeignEncryptedContent(t *testing.T) {
	for _, value := range []string{
		"gAAAAABcodex-shaped-signature-with-padding==",
		base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 128)),
	} {
		if validGrokReplayEncryptedContent(value) {
			t.Fatalf("foreign encrypted content accepted: %q", value)
		}
	}
	if value := validEncrypted(19); !validGrokReplayEncryptedContent(value) {
		t.Fatal("valid Grok-shaped encrypted content rejected")
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
	if _, ok, err := store.Get(context.Background(), "m", "s", time.Now().UTC(), time.Hour); err != nil || ok {
		t.Fatalf("expected expired miss ok=%v err=%v", ok, err)
	}
}
