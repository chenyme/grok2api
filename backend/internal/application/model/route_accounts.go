package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const maxRouteAccountBindings = 1000

type AddRouteAccountsExpected struct {
	PublicID      string
	Provider      account.Provider
	UpstreamModel string
	Capability    modeldomain.Capability
	Enabled       bool
}

type AddRouteAccountsInput struct {
	AccountIDs []uint64
	Expected   AddRouteAccountsExpected
}

// AddRouteAccounts atomically adds explicit members to one enabled image
// route. The complete expected tuple prevents a stale control plane from
// wiring accounts to a route whose identity changed after it was selected.
func (s *Service) AddRouteAccounts(ctx context.Context, routeID uint64, input AddRouteAccountsInput) (modeldomain.Route, error) {
	if routeID == 0 {
		return modeldomain.Route{}, invalidInput("模型 ID 无效")
	}
	if input.Expected.Provider == "" || !input.Expected.Provider.IsValid() {
		return modeldomain.Route{}, invalidInput("expected.provider 无效")
	}
	if input.Expected.Capability != modeldomain.CapabilityImage {
		return modeldomain.Route{}, invalidInput("仅允许原子绑定 image 路由")
	}
	if !input.Expected.Enabled {
		return modeldomain.Route{}, invalidInput("expected.enabled 必须为 true")
	}
	publicID, ok := modeldomain.NormalizeExternalPublicID(input.Expected.Provider, input.Expected.PublicID)
	if !ok {
		return modeldomain.Route{}, invalidInput("expected.publicId 无效")
	}
	upstreamModel, ok := modeldomain.NormalizeUpstreamModel(input.Expected.Provider, input.Expected.UpstreamModel)
	if !ok {
		return modeldomain.Route{}, invalidInput("expected.upstreamModel 无效")
	}
	if len(input.AccountIDs) == 0 {
		return modeldomain.Route{}, invalidInput("accountIds 不能为空")
	}
	accountIDs, err := normalizeRouteAccountIDs(input.AccountIDs)
	if err != nil {
		return modeldomain.Route{}, err
	}

	value, err := s.models.AddRouteAccounts(ctx, routeID, repository.ModelRouteExpectation{
		PublicID: publicID, Provider: input.Expected.Provider, UpstreamModel: upstreamModel,
		Capability: input.Expected.Capability, Enabled: input.Expected.Enabled,
	}, accountIDs)
	if errors.Is(err, repository.ErrNotFound) {
		return modeldomain.Route{}, ErrNotFound
	}
	if errors.Is(err, repository.ErrConflict) {
		return modeldomain.Route{}, fmt.Errorf("%w: expected 路由状态不匹配或账号来源已变化", ErrConflict)
	}
	return value, err
}

func normalizeRouteAccountIDs(ids []uint64) ([]uint64, error) {
	if len(ids) > maxRouteAccountBindings {
		return nil, invalidInput(fmt.Sprintf("单次最多绑定 %d 个账号", maxRouteAccountBindings))
	}
	unique := make(map[uint64]struct{}, len(ids))
	result := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, invalidInput("绑定账号 ID 无效")
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		result = append(result, id)
	}
	return result, nil
}
