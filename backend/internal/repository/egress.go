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

// EgressNodePageRepository is the bounded management-list contract. Runtime
// routing repositories only need EgressRepository's full-list operations.
type EgressNodePageRepository interface {
	ListEgressNodePage(ctx context.Context, input EgressNodeListQuery) ([]egress.Node, int64, error)
}

type EgressNodeCleanupPreview struct {
	Nodes               int64
	BoundAccounts       int64
	SubscriptionManaged int64
}

// EgressNodeUnhealthyCleaner provides an atomic cleanup path for nodes whose
// latest IPv4 and IPv6 probes both failed.
type EgressNodeUnhealthyCleaner interface {
	PreviewUnhealthyEgressNodes(context.Context) (EgressNodeCleanupPreview, error)
	DeleteUnhealthyEgressNodes(context.Context) ([]uint64, error)
}

type EgressNodeListFilter struct {
	Scope       egress.Scope
	Enabled     *bool
	ProbeStatus egress.ProbeStatus
	Assignment  string
}

type EgressNodeListQuery struct {
	Page   PageQuery
	Filter EgressNodeListFilter
}

type EgressSourceListFilter struct {
	Scope egress.Scope
}

type EgressSourceListQuery struct {
	Page   PageQuery
	Filter EgressSourceListFilter
}
