package relational

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AddRouteAccounts performs a guarded set-union. Locking the route serializes
// concurrent membership writers, while ON CONFLICT makes duplicate additions
// idempotent. Every expectation and account reference is checked in the same
// transaction before any binding is written.
func (r *ModelRepository) AddRouteAccounts(
	ctx context.Context,
	routeID uint64,
	expected repository.ModelRouteExpectation,
	accountIDs []uint64,
) (model.Route, error) {
	var changed bool
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var route modelRouteModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&route, routeID).Error; err != nil {
			return mapError(err)
		}
		if !routeMatchesExpectation(route, expected) {
			return repository.ErrConflict
		}
		// No rows means implicit provider/capability routing, not an empty set.
		// Refuse to turn that wildcard into a partial explicit set through an
		// endpoint whose contract is add-only.
		var existingBindings int64
		if err := tx.Model(&modelRouteAccountModel{}).
			Where("model_route_id = ?", routeID).
			Count(&existingBindings).Error; err != nil {
			return err
		}
		if existingBindings == 0 {
			return repository.ErrConflict
		}

		var matchingAccounts int64
		if err := tx.Model(&accountModel{}).
			Where("id IN ? AND provider = ?", accountIDs, expected.Provider).
			Count(&matchingAccounts).Error; err != nil {
			return err
		}
		if matchingAccounts != int64(len(accountIDs)) {
			return repository.ErrConflict
		}

		rows := make([]modelRouteAccountModel, 0, len(accountIDs))
		for _, accountID := range accountIDs {
			rows = append(rows, modelRouteAccountModel{ModelRouteID: routeID, AccountID: accountID})
		}
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "model_route_id"}, {Name: "account_id"}},
			DoNothing: true,
		}).CreateInBatches(rows, 200)
		changed = result.Error == nil && result.RowsAffected > 0
		return result.Error
	})
	if err != nil {
		return model.Route{}, err
	}
	if changed {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{
			Kind:          repository.InvalidationModelBindingChanged,
			Provider:      expected.Provider,
			UpstreamModel: expected.UpstreamModel,
		})
	}
	return r.Get(ctx, routeID)
}

func routeMatchesExpectation(route modelRouteModel, expected repository.ModelRouteExpectation) bool {
	return route.PublicID == expected.PublicID &&
		account.Provider(route.Provider) == expected.Provider &&
		route.UpstreamModel == expected.UpstreamModel &&
		model.Capability(route.Capability) == expected.Capability &&
		route.Enabled == expected.Enabled
}
