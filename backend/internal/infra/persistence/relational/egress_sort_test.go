package relational

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type legacyAllEgressNode struct {
	ID    uint64 `gorm:"primaryKey;autoIncrement"`
	Scope string `gorm:"size:32;not null"`
}

func (legacyAllEgressNode) TableName() string { return "egress_nodes" }

type legacyEgressNodeWithoutProxyPool struct {
	ID                          uint64 `gorm:"primaryKey;autoIncrement"`
	Name                        string `gorm:"size:160;not null"`
	Scope                       string `gorm:"size:32;not null"`
	Enabled                     bool   `gorm:"not null;default:true"`
	EncryptedProxyURL           string `gorm:"type:text;not null;default:''"`
	UserAgent                   string `gorm:"size:512;not null;default:''"`
	EncryptedCloudflareCookie   string `gorm:"type:text;not null;default:''"`
	ClearanceRefreshedAt        *time.Time
	ClearanceFingerprint        string  `gorm:"size:64;not null;default:''"`
	ClearanceBindingFingerprint string  `gorm:"size:64;not null;default:''"`
	Health                      float64 `gorm:"not null;default:1"`
	FailureCount                int     `gorm:"not null;default:0"`
	CooldownUntil               *time.Time
	LastError                   string    `gorm:"size:512"`
	CreatedAt                   time.Time `gorm:"not null"`
	UpdatedAt                   time.Time `gorm:"not null"`
}

func (legacyEgressNodeWithoutProxyPool) TableName() string { return "egress_nodes" }

func TestEgressRepositorySortsInDatabase(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-sort.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(database)
	for _, value := range []egress.Node{
		{Name: "slow", Scope: egress.ScopeBuild, Enabled: true, Health: 0.2},
		{Name: "healthy", Scope: egress.ScopeWeb, Enabled: true, Health: 0.9},
		{Name: "middle", Scope: egress.ScopeWebAsset, Enabled: true, Health: 0.5},
	} {
		if _, err := repo.CreateEgressNode(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	values, err := repo.ListEgressNodes(ctx, "", repository.SortQuery{Field: "health", Direction: repository.SortDescending})
	if err != nil || len(values) != 3 || values[0].Name != "healthy" || values[2].Name != "slow" {
		t.Fatalf("health sort = %#v, err = %v", values, err)
	}
}

func TestEgressStateUpdatesDoNotOverwriteClearanceOrHealth(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(database)
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "web", Scope: egress.ScopeWeb, Enabled: true, Health: 1, UserAgent: "old", EncryptedCloudflareCookie: "old-cookie"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateEgressNodeHealth(ctx, node.ID, 0.4, 2, nil, "anti-bot rejection"); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateEgressNodeClearance(ctx, node.ID, "new-cookie", "new-agent", strings.Repeat("a", 64), strings.Repeat("b", 64), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	actual, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Health != 0.4 || actual.FailureCount != 2 || actual.EncryptedCloudflareCookie != "new-cookie" || actual.UserAgent != "new-agent" || actual.ClearanceBindingFingerprint != strings.Repeat("b", 64) {
		t.Fatalf("partial updates overwrote state: %#v", actual)
	}
}

func TestInitializeSchemaRemovesAndRejectsLegacyAllEgressNodes(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-egress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.db.WithContext(ctx).AutoMigrate(&legacyAllEgressNode{}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&legacyAllEgressNode{Scope: "all"}).Error; err != nil {
		t.Fatal(err)
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := database.db.WithContext(ctx).Model(&egressNodeModel{}).Where("scope = ?", "all").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("legacy all-scope nodes = %d", count)
	}
	if _, err := NewEgressRepository(database).CreateEgressNode(ctx, egress.Node{Name: "invalid", Scope: egress.Scope("all"), Enabled: true}); err == nil {
		t.Fatal("all-scope node passed the database constraint")
	}
}

func TestInitializeSchemaAddsProxyPoolWithoutChangingExistingRows(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-proxy-pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.db.WithContext(ctx).AutoMigrate(&legacyEgressNodeWithoutProxyPool{}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&legacyEgressNodeWithoutProxyPool{Name: "existing", Scope: string(egress.ScopeBuild), Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("repeated schema initialization failed: %v", err)
	}
	repo := NewEgressRepository(database)
	existing, err := repo.GetEgressNode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if existing.ProxyPool || existing.Name != "existing" {
		t.Fatalf("legacy row changed during migration: %#v", existing)
	}
	existing.ProxyPool = true
	updated, err := repo.UpdateEgressNode(ctx, existing)
	if err != nil || !updated.ProxyPool {
		t.Fatalf("proxy pool did not round trip: %#v, err=%v", updated, err)
	}
}
