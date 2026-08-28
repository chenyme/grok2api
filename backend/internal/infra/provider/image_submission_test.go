package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestImagePreSubmissionErrorIsExplicitAndPreservesCause(t *testing.T) {
	cause := errors.New("egress unavailable")
	err := NewImagePreSubmissionError(cause)
	if err == nil || !IsImagePreSubmissionError(err) || !errors.Is(err, cause) {
		t.Fatalf("pre-submission error = %#v", err)
	}
	if !IsImagePreSubmissionError(fmt.Errorf("outer: %w", err)) {
		t.Fatal("wrapped pre-submission error was not detected")
	}
	if NewImagePreSubmissionError(nil) != nil {
		t.Fatal("nil cause created a pre-submission marker")
	}
	if IsImagePreSubmissionError(cause) {
		t.Fatal("untyped error was classified as pre-submission")
	}
}
