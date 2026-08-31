package repository

import (
	"context"
	"time"
)

type SuccessfulImageCoverageQuery struct {
	ClientKeyID  uint64
	ModelRouteID uint64
	Since        time.Time
	Until        time.Time
	RunMarker    string
}

type SuccessfulImageCoverage struct {
	AccountIDs           []uint64
	TerminalSuccessCount int64
}

type SuccessfulImageCoverageRepository interface {
	SummarizeSuccessfulImageCoverage(context.Context, SuccessfulImageCoverageQuery) (SuccessfulImageCoverage, error)
}
