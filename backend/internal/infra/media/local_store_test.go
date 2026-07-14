package media

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

func TestLocalStoreWritesAndRejectsTraversal(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.SaveImage(context.Background(), "img_abcdefghijklmnopqrstuvwxyz", "image/jpeg", []byte("image"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := store.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(data) != "image" {
		t.Fatalf("stored image = %q, err=%v", data, err)
	}
	if _, err := store.SaveImage(context.Background(), "img_abcdefghijklmnopqrstuvwxyz", "image/jpeg", []byte("replacement")); err == nil {
		t.Fatal("existing image was overwritten")
	}
	if _, err := store.Open(context.Background(), "../outside.jpg"); err == nil {
		t.Fatal("path traversal was accepted")
	}
	if err := store.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing object delete error = %v", err)
	}
}
