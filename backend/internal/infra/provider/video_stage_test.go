package provider

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

type videoStageStatusError struct{ status int }

func (e videoStageStatusError) Error() string       { return http.StatusText(e.status) }
func (e videoStageStatusError) HTTPStatusCode() int { return e.status }

func TestVideoCreateFailureStageIsFailClosed(t *testing.T) {
	if stage := VideoCreateFailureStage(errors.New("connection reset after write")); stage != VideoStageSubmitted {
		t.Fatalf("transport failure stage = %q", stage)
	}
	if stage := VideoCreateFailureStage(fmt.Errorf("wrapped: %w", videoStageStatusError{status: http.StatusTooManyRequests})); stage != VideoStageCreate {
		t.Fatalf("explicit 429 stage = %q", stage)
	}
	if stage := VideoCreateFailureStage(ErrUnauthorized); stage != VideoStageCreate {
		t.Fatalf("explicit unauthorized stage = %q", stage)
	}
	if stage := VideoCreateFailureStage(videoStageStatusError{status: http.StatusInternalServerError}); stage != VideoStageSubmitted {
		t.Fatalf("explicit 500 stage = %q", stage)
	}
}
