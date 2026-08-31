package gateway

import "errors"

// ErrUpstreamNotSubmitted is returned only when every image attempt failed at
// a provider boundary that positively proved the generation payload was never
// submitted. The HTTP layer uses it to expose an opt-in safe-failover signal.
var ErrUpstreamNotSubmitted = errors.New("upstream image request was not submitted")

type imageSubmissionState struct {
	mayHaveSubmitted bool
}

// markMayHaveSubmitted is the state machine's only transition. Once provider
// I/O is ambiguous or a response exists, no later safe refusal can make replay
// safe again.
func (s *imageSubmissionState) markMayHaveSubmitted() {
	s.mayHaveSubmitted = true
}

func (s imageSubmissionState) canProveNotSubmitted() bool {
	return !s.mayHaveSubmitted
}
