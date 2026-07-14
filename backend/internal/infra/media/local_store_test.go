package media

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func TestLocalStoreRetriesTemporaryCleanupWithoutDeletingCommittedImage(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	removeTemporary := store.removeTemporary
	removeAttempts := 0
	store.removeTemporary = func(path string) error {
		removeAttempts++
		if removeAttempts == 1 {
			return errors.New("transient cleanup failure")
		}
		return removeTemporary(path)
	}

	key, err := store.SaveImage(context.Background(), "img_cleanup_retry_0000000001", "image/png", []byte("image"))
	if err != nil {
		t.Fatal(err)
	}
	if removeAttempts != 2 {
		t.Fatalf("temporary cleanup attempts = %d, want 2", removeAttempts)
	}
	body, err := store.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || string(data) != "image" {
		t.Fatalf("committed image = %q, err = %v", data, readErr)
	}
	temporaryFiles, err := filepath.Glob(filepath.Join(root, "images", "im", ".image-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporaryFiles) != 0 {
		t.Fatalf("temporary files were not cleaned: %#v", temporaryFiles)
	}
}
