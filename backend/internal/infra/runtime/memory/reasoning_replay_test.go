package memory

import (
	"context"
	"testing"
	"time"
)

func TestReasoningReplayStoreSetGetDelete(t *testing.T) {
	store := NewReasoningReplayStore(8)
	ctx := context.Background()
	now := time.Now().UTC()
	items := [][]byte{[]byte(`{"type":"reasoning","encrypted_content":"abcdefghijklmnopqrstuvwxyz"}`)}
	if err := store.Set(ctx, "grok", "sess", items, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(ctx, "grok", "sess", now, time.Hour)
	if err != nil || !ok || len(got) != 1 {
		t.Fatalf("get = %v ok=%v err=%v", got, ok, err)
	}
	if err := store.Delete(ctx, "grok", "sess"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, "grok", "sess", now, time.Hour); err != nil || ok {
		t.Fatalf("after delete ok=%v err=%v", ok, err)
	}
}

func TestReasoningReplayStoreEvictsOldest(t *testing.T) {
	store := NewReasoningReplayStore(2)
	ctx := context.Background()
	now := time.Now().UTC()
	_ = store.Set(ctx, "m", "a", [][]byte{[]byte("1")}, now.Add(time.Hour))
	time.Sleep(2 * time.Millisecond)
	_ = store.Set(ctx, "m", "b", [][]byte{[]byte("2")}, now.Add(time.Hour))
	time.Sleep(2 * time.Millisecond)
	_ = store.Set(ctx, "m", "c", [][]byte{[]byte("3")}, now.Add(time.Hour))
	if _, ok, _ := store.Get(ctx, "m", "a", now, time.Hour); ok {
		t.Fatal("oldest entry should be evicted")
	}
	if _, ok, _ := store.Get(ctx, "m", "c", now, time.Hour); !ok {
		t.Fatal("newest entry should remain")
	}
}

func TestReasoningReplayStoreHitRestoresConfiguredTTL(t *testing.T) {
	store := NewReasoningReplayStore(8)
	ctx := context.Background()
	storedAt := time.Now()
	if err := store.Set(ctx, "grok", "sess", [][]byte{[]byte("state")}, storedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, "grok", "sess", storedAt.Add(30*time.Minute), 2*time.Hour); err != nil || !ok {
		t.Fatalf("first get ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.Get(ctx, "grok", "sess", storedAt.Add(90*time.Minute), 2*time.Hour); err != nil || !ok {
		t.Fatalf("sliding ttl was not restored: ok=%v err=%v", ok, err)
	}
}
