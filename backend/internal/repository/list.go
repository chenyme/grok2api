package repository

import "time"

const (
	// DefaultPageSize is the default size for admin list endpoints.
	DefaultPageSize = 20
	// MaxPageSize bounds an individual admin query. It is intentionally finite
	// so large page requests cannot turn into unbounded database or JSON work.
	MaxPageSize = 2000
	// DefaultCursorPageSize is the default batch size for cursor-based audit reads.
	DefaultCursorPageSize = 50
)

// NormalizePage applies the common bounded pagination contract used by all
// admin list endpoints. Keeping this in one place prevents transport and
// application layers from returning different effective page sizes.
func NormalizePage(page, pageSize, defaultPageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if defaultPageSize < 1 || defaultPageSize > MaxPageSize {
		defaultPageSize = DefaultPageSize
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	return page, pageSize
}

type SortDirection string

const (
	SortAscending  SortDirection = "asc"
	SortDescending SortDirection = "desc"
)

// SortQuery 只携带应用层验证后的领域字段名，持久化层负责映射为固定 SQL 表达式。
type SortQuery struct {
	Field     string
	Direction SortDirection
}

func IsValidSort(sort SortQuery, fields ...string) bool {
	if sort.Field == "" {
		return sort.Direction == ""
	}
	if sort.Direction != SortAscending && sort.Direction != SortDescending {
		return false
	}
	for _, field := range fields {
		if sort.Field == field {
			return true
		}
	}
	return false
}

// PageQuery 表示管理端页码列表的稳定查询边界。
type PageQuery struct {
	Offset int
	Limit  int
	Search string
	Sort   SortQuery
}

type AccountListFilter struct {
	Provider    string
	QuotaType   string
	Status      string
	Egress      string
	Refreshable *bool
	AccountIDs  []uint64
	RestrictIDs bool
	ExcludeIDs  []uint64
	Now         time.Time
}

type AccountListQuery struct {
	Page   PageQuery
	Filter AccountListFilter
}

type AccountSummary struct {
	Provider       string
	Total          int64
	Available      int64
	Cooldown       int64
	WaitingReset   int64
	Probing        int64
	Disabled       int64
	ReauthRequired int64
}

type ModelListFilter struct {
	Provider string
	Enabled  *bool
}

type ModelListQuery struct {
	Page   PageQuery
	Filter ModelListFilter
}

type ClientKeyListFilter struct {
	Status     string
	ModelScope string
	Now        time.Time
}

type ClientKeyListQuery struct {
	Page   PageQuery
	Filter ClientKeyListFilter
}

type AuditListFilter struct {
	Model   string
	Status  string
	Mode    string
	Key     string
	Account string
}

type AuditCursorQuery struct {
	Cursor *SortCursor
	Limit  int
	Search string
	Start  time.Time
	End    time.Time
	Sort   SortQuery
	Filter AuditListFilter
}

// SortCursor 是复合游标的持久化边界。Value 类型由 SortQuery.Field 对应的固定映射决定。
type SortCursor struct {
	ID    uint64
	Value any
}

type AuditSummaryQuery struct {
	Search string
	Start  time.Time
	End    time.Time
	Filter AuditListFilter
}
