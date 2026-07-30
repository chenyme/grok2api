package account

import (
	"context"
	"errors"
	"slices"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type batchUpdateRepository struct {
	repository.AccountRepository
	updateErr  error
	updatedIDs []uint64
}

func (r *batchUpdateRepository) UpdateMany(_ context.Context, providerValue accountdomain.Provider, ids []uint64, _ repository.AccountUpdates) (int64, error) {
	if providerValue != accountdomain.ProviderBuild {
		return 0, errors.New("unexpected provider")
	}
	r.updatedIDs = append([]uint64(nil), ids...)
	if r.updateErr != nil {
		return 0, r.updateErr
	}
	return int64(len(ids)), nil
}

func TestBatchUpdateSupportsMoreThanAdminPageLimit(t *testing.T) {
	ids := make([]uint64, 2501)
	for index := range ids {
		ids[index] = uint64(index + 1)
	}
	repo := &batchUpdateRepository{}
	service := NewService(repo, nil, nil, nil, nil, nil, nil)
	maxConcurrent := 3

	updated, err := service.BatchUpdate(context.Background(), accountdomain.ProviderBuild, ids, UpdateInput{MaxConcurrent: &maxConcurrent})
	if err != nil {
		t.Fatal(err)
	}
	if updated != int64(len(ids)) || !slices.Equal(repo.updatedIDs, ids) {
		t.Fatalf("updated = %d, ids = %d", updated, len(repo.updatedIDs))
	}
}

func TestBatchUpdatePreservesProviderMismatchSemantics(t *testing.T) {
	ids := make([]uint64, 501)
	for index := range ids {
		ids[index] = uint64(index + 1)
	}
	repo := &batchUpdateRepository{updateErr: repository.ErrAccountPoolMismatch}
	service := NewService(repo, nil, nil, nil, nil, nil, nil)
	maxConcurrent := 3

	_, err := service.BatchUpdate(context.Background(), accountdomain.ProviderBuild, ids, UpdateInput{MaxConcurrent: &maxConcurrent})
	if !errors.Is(err, ErrAccountPoolMismatch) {
		t.Fatalf("error = %v, want account pool mismatch", err)
	}
}

func TestBatchUpdateRetainsBoundedRequestSize(t *testing.T) {
	ids := make([]uint64, maxBatchUpdateAccounts+1)
	for index := range ids {
		ids[index] = uint64(index + 1)
	}
	repo := &batchUpdateRepository{}
	service := NewService(repo, nil, nil, nil, nil, nil, nil)
	maxConcurrent := 3

	_, err := service.BatchUpdate(context.Background(), accountdomain.ProviderBuild, ids, UpdateInput{MaxConcurrent: &maxConcurrent})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want invalid input", err)
	}
	if len(repo.updatedIDs) != 0 {
		t.Fatal("oversized update reached repository")
	}
}
