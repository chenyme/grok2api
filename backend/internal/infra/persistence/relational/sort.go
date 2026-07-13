package relational

import (
	"strings"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type sortSpec struct {
	expression       string
	nullsLast        bool
	defaultDirection repository.SortDirection
}

func applyStableSort(query *gorm.DB, sort repository.SortQuery, fields map[string]sortSpec, fallback sortSpec, idColumn string) *gorm.DB {
	spec, ok := fields[sort.Field]
	if !ok || strings.TrimSpace(spec.expression) == "" {
		spec = fallback
	}
	direction := "ASC"
	resolvedDirection := sort.Direction
	if resolvedDirection != repository.SortAscending && resolvedDirection != repository.SortDescending {
		resolvedDirection = spec.defaultDirection
	}
	if resolvedDirection == repository.SortDescending {
		direction = "DESC"
	}
	if spec.nullsLast {
		query = query.Order("CASE WHEN " + spec.expression + " IS NULL THEN 1 ELSE 0 END ASC")
	}
	return query.Order(spec.expression + " " + direction).Order(idColumn + " " + direction)
}

func stableSortSpec(sort repository.SortQuery, fields map[string]sortSpec, fallback sortSpec) (sortSpec, string) {
	spec, ok := fields[sort.Field]
	if !ok || strings.TrimSpace(spec.expression) == "" {
		spec = fallback
	}
	direction := "ASC"
	resolvedDirection := sort.Direction
	if resolvedDirection != repository.SortAscending && resolvedDirection != repository.SortDescending {
		resolvedDirection = spec.defaultDirection
	}
	if resolvedDirection == repository.SortDescending {
		direction = "DESC"
	}
	return spec, direction
}
