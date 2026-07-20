package cli

import "testing"

func TestGrokSessionIDEmptyWhenNoKey(t *testing.T) {
	got, err := grokSessionID("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("empty key must not invent session id, got %q", got)
	}
	// 两次调用仍为空，证明无无随机漂移
	again, err := grokSessionID("   ")
	if err != nil || again != "" {
		t.Fatalf("blank key = %q err=%v", again, err)
	}
}

func TestGrokSessionIDStableForSameKey(t *testing.T) {
	first, err := grokSessionID("client-session")
	if err != nil || first == "" {
		t.Fatalf("first = %q err=%v", first, err)
	}
	second, err := grokSessionID("client-session")
	if err != nil || second != first {
		t.Fatalf("unstable: first=%q second=%q err=%v", first, second, err)
	}
}
