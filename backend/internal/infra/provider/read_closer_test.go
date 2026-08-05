package provider

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCompletionReadCloserReportsCompletedTransferOnce(t *testing.T) {
	calls := 0
	var finishedErr error
	completed := false
	body := NewCompletionReadCloser(io.NopCloser(strings.NewReader("video")), func(err error, complete bool) {
		calls++
		finishedErr, completed = err, complete
	})
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || finishedErr != nil || !completed {
		t.Fatalf("finish calls=%d err=%v completed=%v", calls, finishedErr, completed)
	}
}

func TestCompletionReadCloserDoesNotReportEarlyCloseAsSuccess(t *testing.T) {
	calls := 0
	completed := true
	body := NewCompletionReadCloser(io.NopCloser(strings.NewReader("video")), func(_ error, complete bool) {
		calls++
		completed = complete
	})
	buffer := make([]byte, 1)
	if _, err := body.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || completed {
		t.Fatalf("finish calls=%d completed=%v", calls, completed)
	}
}

type failingReadCloser struct{ err error }

func (value failingReadCloser) Read([]byte) (int, error) { return 0, value.err }
func (failingReadCloser) Close() error                   { return nil }

func TestCompletionReadCloserReportsReadFailure(t *testing.T) {
	want := errors.New("stream interrupted")
	var finishedErr error
	body := NewCompletionReadCloser(failingReadCloser{err: want}, func(err error, _ bool) { finishedErr = err })
	if _, err := io.ReadAll(body); !errors.Is(err, want) {
		t.Fatalf("read error = %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(finishedErr, want) {
		t.Fatalf("finish error = %v", finishedErr)
	}
}
