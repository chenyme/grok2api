package repository

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
)

type EgressRepository interface {
	ListEgressNodes(ctx context.Context, scope egress.Scope, sort SortQuery) ([]egress.Node, error)
	GetEgressNode(ctx context.Context, id uint64) (egress.Node, error)
	CreateEgressNode(ctx context.Context, value egress.Node) (egress.Node, error)
	UpdateEgressNode(ctx context.Context, value egress.Node) (egress.Node, error)
	DeleteEgressNode(ctx context.Context, id uint64) error
}
