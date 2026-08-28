package gateway

import "errors"

// ErrUpstreamNotSubmitted is returned only when every image attempt failed at
// a provider boundary that positively proved the generation payload was never
// submitted. The HTTP layer uses it to expose an opt-in safe-failover signal.
var ErrUpstreamNotSubmitted = errors.New("upstream image request was not submitted")

type imageSubmissionDisposition uint8

const (
	imageSubmissionUnknown imageSubmissionDisposition = iota
	imageSubmissionProvenAbsent
	imageSubmissionObserved
)

func (d *imageSubmissionDisposition) recordPreSubmissionFailure() {
	if *d == imageSubmissionUnknown {
		*d = imageSubmissionProvenAbsent
	}
}

func (d *imageSubmissionDisposition) recordResponse() {
	*d = imageSubmissionObserved
}

func (d imageSubmissionDisposition) provenAbsent() bool {
	return d == imageSubmissionProvenAbsent
}
