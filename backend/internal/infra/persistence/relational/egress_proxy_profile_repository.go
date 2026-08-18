package relational

import (
	"context"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type egressProxyProfileRow struct {
	ID                uint64
	Name              string
	EncryptedProxyURL string
	BoundNodeCount    int `gorm:"column:bound_node_count"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (r *EgressRepository) ListEgressProxyProfiles(ctx context.Context, page repository.PageQuery) ([]egress.ProxyProfile, int64, error) {
	query := r.db.db.WithContext(ctx).
		Table("egress_proxy_profiles AS profile")
	if search := strings.TrimSpace(page.Search); search != "" {
		query = query.Where("LOWER(profile.name) LIKE ?", "%"+strings.ToLower(search)+"%")
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []egressProxyProfileRow
	err := query.
		Select("profile.*, COUNT(node.id) AS bound_node_count").
		Joins("LEFT JOIN egress_nodes AS node ON node.proxy_profile_id = profile.id").
		Group("profile.id").
		Order("LOWER(profile.name) ASC, profile.id ASC").
		Offset(page.Offset).
		Limit(page.Limit).
		Scan(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	values := make([]egress.ProxyProfile, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressProxyProfileDomain(egressProxyProfileModel{
			ID: row.ID, Name: row.Name, EncryptedProxyURL: row.EncryptedProxyURL,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}, row.BoundNodeCount))
	}
	return values, total, nil
}

func (r *EgressRepository) GetEgressProxyProfile(ctx context.Context, id uint64) (egress.ProxyProfile, error) {
	var row egressProxyProfileModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return egress.ProxyProfile{}, mapError(err)
	}
	var count int64
	if err := r.db.db.WithContext(ctx).Model(&egressNodeModel{}).Where("proxy_profile_id = ?", id).Count(&count).Error; err != nil {
		return egress.ProxyProfile{}, err
	}
	return toEgressProxyProfileDomain(row, int(count)), nil
}

func (r *EgressRepository) CreateEgressProxyProfile(ctx context.Context, value egress.ProxyProfile) (egress.ProxyProfile, error) {
	row := fromEgressProxyProfileDomain(value)
	if err := r.db.db.WithContext(ctx).Create(&row).Error; err != nil {
		return egress.ProxyProfile{}, mapError(err)
	}
	return toEgressProxyProfileDomain(row, 0), nil
}

func (r *EgressRepository) UpdateEgressProxyProfile(ctx context.Context, value egress.ProxyProfile, proxyChanged bool) (egress.ProxyProfile, []uint64, error) {
	var updated egressProxyProfileModel
	var nodeIDs []uint64
	var boundNodeCount int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&updated, value.ID).Error; err != nil {
			return err
		}
		updated.Name = value.Name
		if proxyChanged {
			updated.EncryptedProxyURL = value.EncryptedProxyURL
		}
		if err := tx.Save(&updated).Error; err != nil {
			return err
		}
		if err := tx.Model(&egressNodeModel{}).Where("proxy_profile_id = ?", value.ID).Count(&boundNodeCount).Error; err != nil {
			return err
		}
		if !proxyChanged {
			return nil
		}
		if err := tx.Model(&egressNodeModel{}).Where("proxy_profile_id = ?", value.ID).Order("id ASC").Pluck("id", &nodeIDs).Error; err != nil {
			return err
		}
		if len(nodeIDs) == 0 {
			return nil
		}
		now := time.Now().UTC()
		return tx.Model(&egressNodeModel{}).Where("id IN ? AND proxy_profile_id = ?", nodeIDs, value.ID).Updates(map[string]any{
			"encrypted_proxy_url":           value.EncryptedProxyURL,
			"encrypted_cloudflare_cookie":   "",
			"clearance_refreshed_at":        nil,
			"clearance_fingerprint":         "",
			"clearance_binding_fingerprint": "",
			"health":                        1,
			"failure_count":                 0,
			"cooldown_until":                nil,
			"last_error":                    "",
			"probe_status":                  string(egress.ProbeStatusUnknown),
			"last_probed_at":                nil,
			"probe_latency_ms":              0,
			"exit_ip":                       "",
			"probe_error":                   "",
			"probe_provider":                "",
			"ipv4_probe_status":             string(egress.ProbeStatusUnknown),
			"ipv4_last_probed_at":           nil,
			"ipv4_probe_latency_ms":         0,
			"ipv4_exit_ip":                  "",
			"ipv4_probe_error":              "",
			"ipv6_probe_status":             string(egress.ProbeStatusUnknown),
			"ipv6_last_probed_at":           nil,
			"ipv6_probe_latency_ms":         0,
			"ipv6_exit_ip":                  "",
			"ipv6_probe_error":              "",
			"updated_at":                    now,
		}).Error
	})
	if err != nil {
		return egress.ProxyProfile{}, nil, mapError(err)
	}
	return toEgressProxyProfileDomain(updated, int(boundNodeCount)), nodeIDs, nil
}

func (r *EgressRepository) DeleteEgressProxyProfile(ctx context.Context, id uint64) error {
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row egressProxyProfileModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&egressNodeModel{}).Where("proxy_profile_id = ?", id).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return repository.ErrEgressProxyProfileInUse
		}
		return tx.Delete(&row).Error
	})
	return mapError(err)
}

func toEgressProxyProfileDomain(row egressProxyProfileModel, boundNodeCount int) egress.ProxyProfile {
	return egress.ProxyProfile{
		ID: row.ID, Name: row.Name, EncryptedProxyURL: row.EncryptedProxyURL,
		BoundNodeCount: boundNodeCount, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func fromEgressProxyProfileDomain(value egress.ProxyProfile) egressProxyProfileModel {
	return egressProxyProfileModel{
		ID: value.ID, Name: value.Name, EncryptedProxyURL: value.EncryptedProxyURL,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}
