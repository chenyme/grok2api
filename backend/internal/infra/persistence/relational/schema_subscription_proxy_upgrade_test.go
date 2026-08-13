package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

type legacyOperationsConfigWithSubscriptionProxy struct {
	ID                            uint64    `gorm:"primaryKey"`
	ProbeProvider                 string    `gorm:"size:16;not null;default:cloudflare"`
	ProbeIntervalSeconds          int       `gorm:"not null;default:900"`
	AutoAssignEnabled             bool      `gorm:"not null;default:false"`
	AutoBalanceEnabled            bool      `gorm:"not null;default:false"`
	AssignmentIntervalSeconds     int       `gorm:"not null;default:300"`
	EncryptedSubscriptionProxyURL string    `gorm:"type:text;not null;default:''"`
	BuildFallbackMode             string    `gorm:"size:16;not null;default:none"`
	BuildFallbackNodeID           uint64    `gorm:"not null;default:0"`
	WebFallbackMode               string    `gorm:"size:16;not null;default:none"`
	WebFallbackNodeID             uint64    `gorm:"not null;default:0"`
	ConsoleFallbackMode           string    `gorm:"size:16;not null;default:none"`
	ConsoleFallbackNodeID         uint64    `gorm:"not null;default:0"`
	WebAssetFallbackMode          string    `gorm:"size:16;not null;default:none"`
	WebAssetFallbackNodeID        uint64    `gorm:"not null;default:0"`
	ConsoleAssetFallbackMode      string    `gorm:"size:16;not null;default:none"`
	ConsoleAssetFallbackNodeID    uint64    `gorm:"not null;default:0"`
	UpdatedAt                     time.Time `gorm:"not null"`
}

func (legacyOperationsConfigWithSubscriptionProxy) TableName() string {
	return "egress_operations_config"
}

type legacySubscriptionSourceWithoutProxy struct {
	ID                     uint64 `gorm:"primaryKey"`
	Name                   string `gorm:"size:160;not null"`
	Scope                  string `gorm:"size:32;not null"`
	Enabled                bool   `gorm:"not null;default:true"`
	EncryptedURL           string `gorm:"type:text;not null;default:''"`
	RefreshIntervalSeconds int    `gorm:"not null;default:900"`
	DefaultAccountCapacity int    `gorm:"not null;default:0"`
	LastSyncedAt           *time.Time
	NextSyncAt             *time.Time
	LastSyncImported       int       `gorm:"not null;default:0"`
	LastSyncError          string    `gorm:"size:512;not null;default:''"`
	CreatedAt              time.Time `gorm:"not null"`
	UpdatedAt              time.Time `gorm:"not null"`
}

func (legacySubscriptionSourceWithoutProxy) TableName() string {
	return "egress_subscription_sources"
}

func TestSchemaMigratesGlobalSubscriptionProxyToEverySource(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "subscription-proxy-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := database.db.AutoMigrate(&legacyOperationsConfigWithSubscriptionProxy{}, &legacySubscriptionSourceWithoutProxy{}); err != nil {
		t.Fatal(err)
	}
	const encryptedProxy = "encrypted-global-subscription-proxy"
	if err := database.db.Create(&legacyOperationsConfigWithSubscriptionProxy{ID: 1, EncryptedSubscriptionProxyURL: encryptedProxy}).Error; err != nil {
		t.Fatal(err)
	}
	for _, source := range []legacySubscriptionSourceWithoutProxy{
		{ID: 1, Name: "domestic", Scope: "grok_build", Enabled: true},
		{ID: 2, Name: "overseas", Scope: "grok_web", Enabled: true},
	} {
		if err := database.db.Create(&source).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var sources []egressSubscriptionSourceModel
	if err := database.db.Order("id").Find(&sources).Error; err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("migrated sources = %d", len(sources))
	}
	for _, source := range sources {
		if source.EncryptedProxyURL != encryptedProxy || source.NextSyncAt != nil {
			t.Fatalf("migrated source = %#v", source)
		}
	}
	var current egressOperationsConfigModel
	if err := database.db.First(&current, 1).Error; err != nil {
		t.Fatal(err)
	}
	if !current.SubscriptionProxyMigrationCompleted {
		t.Fatal("subscription proxy migration was not marked complete")
	}
	repository := NewEgressRepository(database)
	if _, err := repository.SaveEgressOperationsConfig(ctx, egressdomain.DefaultOperationsConfig()); err != nil {
		t.Fatal(err)
	}
	if err := database.db.First(&current, 1).Error; err != nil {
		t.Fatal(err)
	}
	if !current.SubscriptionProxyMigrationCompleted {
		t.Fatal("ordinary operations update reset the subscription proxy migration marker")
	}
	var legacy legacyOperationsConfigWithSubscriptionProxy
	if err := database.db.First(&legacy, 1).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.EncryptedSubscriptionProxyURL != encryptedProxy {
		t.Fatal("legacy global subscription proxy was not retained for rollback")
	}

	// Sources created after the one-time migration must remain direct. Keeping
	// the legacy ciphertext for rollback must not cause it to be copied again.
	direct := legacySubscriptionSourceWithoutProxy{ID: 3, Name: "direct", Scope: "grok_console", Enabled: true}
	if err := database.db.Create(&direct).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("repeated migration failed: %v", err)
	}
	var directAfterRestart egressSubscriptionSourceModel
	if err := database.db.First(&directAfterRestart, direct.ID).Error; err != nil {
		t.Fatal(err)
	}
	if directAfterRestart.EncryptedProxyURL != "" {
		t.Fatal("repeated migration applied the legacy proxy to a new direct source")
	}
}
