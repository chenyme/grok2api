package model

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

type Capability string

type Origin string

const (
	CapabilityResponses Capability = "responses"
	CapabilityChat      Capability = "chat"
	CapabilityImage     Capability = "image"
	CapabilityImageEdit Capability = "image_edit"
	CapabilityVideo     Capability = "video"
)

const (
	OriginCatalog    Origin = "catalog"
	OriginDiscovered Origin = "discovered"
	OriginManual     Origin = "manual"
)

// Route 表示公开模型名到上游模型名的稳定映射。
type Route struct {
	ID                uint64
	PublicID          string
	Provider          account.Provider
	UpstreamModel     string
	Capability        Capability
	Origin            Origin
	Enabled           bool
	BoundAccountIDs   []uint64
	SupportedAccounts int
	SyncedAccounts    int
	TotalAccounts     int
	LastSyncedAt      *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
