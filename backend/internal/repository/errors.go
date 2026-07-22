package repository

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound      = errors.New("repository: not found")
	ErrConflict      = errors.New("repository: conflict")
	ErrLimitExceeded = errors.New("repository: limit exceeded")
	ErrInvalidRecord = errors.New("repository: invalid record")
)

// InvalidBatchRecordError identifies a deterministic invalid record without
// classifying transient database failures as record-local errors.
type InvalidBatchRecordError struct {
	Index int
	Err   error
}

func (e *InvalidBatchRecordError) Error() string {
	if e == nil {
		return ErrInvalidRecord.Error()
	}
	if e.Err == nil {
		return fmt.Sprintf("%s at batch index %d", ErrInvalidRecord, e.Index)
	}
	return fmt.Sprintf("%s at batch index %d: %v", ErrInvalidRecord, e.Index, e.Err)
}

func (e *InvalidBatchRecordError) Unwrap() error {
	if e == nil || e.Err == nil {
		return ErrInvalidRecord
	}
	return e.Err
}

func (e *InvalidBatchRecordError) Is(target error) bool {
	return target == ErrInvalidRecord
}
