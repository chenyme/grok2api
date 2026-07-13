package model

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

type Capability string

const (
	CapabilityResponses Capability = "responses"
	CapabilityChat      Capability = "chat"
	CapabilityImage     Capability = "image"
	CapabilityImageEdit Capability = "image_edit"
	CapabilityVideo     Capability = "video"
)

// Route 表示公开模型名到上游模型名的稳定映射。
type Route struct {
	ID                uint64
	PublicID          string
	Provider          account.Provider
	UpstreamModel     string
	Capability        Capability
	Enabled           bool
	SupportedAccounts int
	SyncedAccounts    int
	TotalAccounts     int
	LastSyncedAt      *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
