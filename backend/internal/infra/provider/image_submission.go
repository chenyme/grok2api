package provider

import (
	"errors"
	"fmt"
)

// imagePreSubmissionError marks an image request failure that happened before
// the adapter started writing the generation payload upstream. Only adapters
// that can positively prove that boundary may create this error.
type imagePreSubmissionError struct {
	cause error
}

func (e *imagePreSubmissionError) Error() string {
	if e == nil || e.cause == nil {
		return "image request was not submitted upstream"
	}
	return fmt.Sprintf("image request was not submitted upstream: %v", e.cause)
}

func (e *imagePreSubmissionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// NewImagePreSubmissionError annotates a positively proven pre-submission
// image failure. A nil cause remains nil so callers cannot manufacture a
// retryable failure without an underlying error.
func NewImagePreSubmissionError(cause error) error {
	if cause == nil {
		return nil
	}
	return &imagePreSubmissionError{cause: cause}
}

// IsImagePreSubmissionError reports whether an image error is positively known
// to have happened before the generation payload was written upstream.
func IsImagePreSubmissionError(err error) bool {
	var target *imagePreSubmissionError
	return errors.As(err, &target)
}
