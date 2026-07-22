package egress

import (
	"context"
	"errors"
	"sort"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	autoAssignmentMigrationCooldown = 5 * time.Minute
	maxAutomaticReassignments       = 200
)

type RebalanceResult struct {
	Assigned   int
	Rebalanced int
	Unplaced   int
}

// RebalanceAccounts allocates only accounts that are either unbound or
// explicitly marked auto. Manual bindings are never changed, even when their
// node is unhealthy or over capacity.
func (s *Service) RebalanceAccounts(ctx context.Context, autoAssign, autoBalance bool, probeInterval time.Duration) (RebalanceResult, error) {
	if s.accounts == nil {
		return RebalanceResult{}, ErrOperationsUnavailable
	}
	if !autoAssign && !autoBalance {
		return RebalanceResult{}, nil
	}
	s.assignmentMu.Lock()
	defer s.assignmentMu.Unlock()
	now := time.Now().UTC()
	if probeInterval <= 0 {
		probeInterval = defaultProbeIntervalSeconds * time.Second
	}
	result := RebalanceResult{}
	for _, provider := range accountdomain.Providers() {
		// Node capacity is global across Provider pools. Refresh the counts after
		// each provider so Web and Console assignments cannot overfill a shared
		// Web node during the same maintenance pass.
		nodes, err := s.repository.ListEgressNodes(ctx, "", repository.SortQuery{})
		if err != nil {
			return result, err
		}
		providerResult, providerErr := s.rebalanceProvider(ctx, provider, nodes, autoAssign, autoBalance, probeInterval, now)
		result.Assigned += providerResult.Assigned
		result.Rebalanced += providerResult.Rebalanced
		result.Unplaced += providerResult.Unplaced
		if providerErr != nil {
			return result, providerErr
		}
	}
	return result, nil
}

func (s *Service) rebalanceProvider(ctx context.Context, provider accountdomain.Provider, allNodes []domain.Node, autoAssign, autoBalance bool, probeInterval time.Duration, now time.Time) (RebalanceResult, error) {
	accounts, err := s.accounts.ListEgressAssignments(ctx, provider)
	if err != nil {
		return RebalanceResult{}, err
	}
	nodes := s.eligibleNodesForProvider(allNodes, provider, probeInterval, now)
	if len(nodes) == 0 {
		return RebalanceResult{Unplaced: countAutoAssignable(accounts, autoAssign, autoBalance)}, nil
	}
	loads := make(map[uint64]int, len(nodes))
	byID := make(map[uint64]domain.Node, len(nodes))
	for _, node := range nodes {
		loads[node.ID] = node.AssignedAccountCount
		byID[node.ID] = node
	}
	original := make(map[uint64]uint64, len(accounts))
	assignment := make(map[uint64]uint64, len(accounts))
	freshMove := make(map[uint64]bool)
	result := RebalanceResult{}

	for _, credential := range accounts {
		original[credential.ID] = credential.EgressNodeID
		assignment[credential.ID] = credential.EgressNodeID
		if !isAutoAssignable(credential, autoAssign, autoBalance) {
			continue
		}
		_, currentHealthy := byID[credential.EgressNodeID]
		needsPlacement := credential.EgressNodeID == 0 || !currentHealthy
		if !needsPlacement {
			continue
		}
		if credential.EgressNodeID != 0 && credential.EgressAssignmentMode != accountdomain.EgressAssignmentAuto {
			continue
		}
		if credential.EgressNodeID == 0 && !autoAssign {
			continue
		}
		target, found := leastLoadedNode(nodes, loads)
		if !found {
			result.Unplaced++
			continue
		}
		assignment[credential.ID] = target.ID
		loads[target.ID]++
		freshMove[credential.ID] = true
		if credential.EgressNodeID == 0 {
			result.Assigned++
		} else {
			result.Rebalanced++
		}
	}

	// Capacity is a placement constraint, not an optional balancing preference.
	// Repair automatic assignments that became over capacity even when ordinary
	// load balancing is disabled. Manual assignments remain immovable.
	moves := 0
	blockedCapacitySources := make(map[uint64]bool)
	for moves < maxAutomaticReassignments {
		source, destination, found := overCapacityPair(nodes, loads, blockedCapacitySources)
		if !found {
			break
		}
		candidateID, movable := findMovableAccount(accounts, assignment, freshMove, source.ID, now)
		if !movable {
			blockedCapacitySources[source.ID] = true
			continue
		}
		assignment[candidateID] = destination.ID
		loads[source.ID]--
		loads[destination.ID]++
		freshMove[candidateID] = true
		moves++
		result.Rebalanced++
	}

	if autoBalance {
		blocked := make(map[uint64]bool)
		for moves < maxAutomaticReassignments {
			source, destination, found := rebalancePair(nodes, loads, blocked)
			if !found {
				break
			}
			candidateID, movable := findMovableAccount(accounts, assignment, freshMove, source.ID, now)
			if !movable {
				blocked[source.ID] = true
				continue
			}
			assignment[candidateID] = destination.ID
			loads[source.ID]--
			loads[destination.ID]++
			freshMove[candidateID] = true
			moves++
			result.Rebalanced++
		}
	}

	updates := make(map[uint64][]uint64)
	for _, credential := range accounts {
		target := assignment[credential.ID]
		if target == 0 || target == original[credential.ID] {
			continue
		}
		updates[target] = append(updates[target], credential.ID)
	}
	for nodeID, ids := range updates {
		if _, err := s.accounts.UpdateEgressBindings(ctx, provider, ids, &nodeID, accountdomain.EgressAssignmentAuto, now); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) eligibleNodesForProvider(values []domain.Node, provider accountdomain.Provider, probeInterval time.Duration, now time.Time) []domain.Node {
	values = append([]domain.Node(nil), values...)
	result := make([]domain.Node, 0, len(values))
	maxAge := max(probeInterval*2, time.Minute)
	for _, value := range values {
		if !value.Enabled || value.EncryptedProxyURL == "" || !scopeSupportsProvider(value.Scope, provider) || value.ProbeStatus != domain.ProbeStatusHealthy || value.LastProbedAt == nil || now.Sub(value.LastProbedAt.UTC()) > maxAge {
			continue
		}
		if value.CooldownUntil != nil && now.Before(value.CooldownUntil.UTC()) && !value.ProxyPool && !s.accountBoundProxy(value) {
			continue
		}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func isAutoAssignable(credential accountdomain.Credential, autoAssign, autoBalance bool) bool {
	if !credential.Enabled || credential.AuthStatus != accountdomain.AuthStatusActive {
		return false
	}
	if credential.EgressNodeID == 0 {
		return autoAssign
	}
	// Once auto assignment owns a binding, it must continue repairing unhealthy
	// or over-capacity placement whenever either automatic mode is running.
	// autoBalance only controls optional movement between otherwise valid nodes.
	return credential.EgressAssignmentMode == accountdomain.EgressAssignmentAuto && (autoAssign || autoBalance)
}

func countAutoAssignable(values []accountdomain.Credential, autoAssign, autoBalance bool) int {
	count := 0
	for _, value := range values {
		if isAutoAssignable(value, autoAssign, autoBalance) {
			count++
		}
	}
	return count
}

func leastLoadedNode(values []domain.Node, loads map[uint64]int) (domain.Node, bool) {
	return leastLoadedNodeExcept(values, loads, 0)
}

func leastLoadedNodeExcept(values []domain.Node, loads map[uint64]int, excludedID uint64) (domain.Node, bool) {
	var selected domain.Node
	found := false
	for _, value := range values {
		if value.ID == excludedID {
			continue
		}
		if value.AccountCapacity > 0 && loads[value.ID] >= value.AccountCapacity {
			continue
		}
		if !found || loads[value.ID] < loads[selected.ID] || (loads[value.ID] == loads[selected.ID] && value.ID < selected.ID) {
			selected, found = value, true
		}
	}
	return selected, found
}

func overCapacityPair(values []domain.Node, loads map[uint64]int, blocked map[uint64]bool) (domain.Node, domain.Node, bool) {
	ordered := append([]domain.Node(nil), values...)
	sort.Slice(ordered, func(i, j int) bool {
		iOverflow := loads[ordered[i].ID] - ordered[i].AccountCapacity
		jOverflow := loads[ordered[j].ID] - ordered[j].AccountCapacity
		if iOverflow == jOverflow {
			return ordered[i].ID < ordered[j].ID
		}
		return iOverflow > jOverflow
	})
	for _, source := range ordered {
		if blocked[source.ID] || source.AccountCapacity <= 0 || loads[source.ID] <= source.AccountCapacity {
			continue
		}
		destination, found := leastLoadedNodeExcept(values, loads, source.ID)
		if found {
			return source, destination, true
		}
	}
	return domain.Node{}, domain.Node{}, false
}

func rebalancePair(values []domain.Node, loads map[uint64]int, blocked map[uint64]bool) (domain.Node, domain.Node, bool) {
	ordered := append([]domain.Node(nil), values...)
	sort.Slice(ordered, func(i, j int) bool {
		if loads[ordered[i].ID] == loads[ordered[j].ID] {
			return ordered[i].ID < ordered[j].ID
		}
		return loads[ordered[i].ID] < loads[ordered[j].ID]
	})
	for _, destination := range ordered {
		if destination.AccountCapacity > 0 && loads[destination.ID] >= destination.AccountCapacity {
			continue
		}
		for index := len(ordered) - 1; index >= 0; index-- {
			source := ordered[index]
			if source.ID == destination.ID || blocked[source.ID] || loads[source.ID] <= loads[destination.ID]+1 {
				continue
			}
			return source, destination, true
		}
	}
	return domain.Node{}, domain.Node{}, false
}

func findMovableAccount(values []accountdomain.Credential, assignment map[uint64]uint64, freshMove map[uint64]bool, sourceID uint64, now time.Time) (uint64, bool) {
	for _, value := range values {
		if assignment[value.ID] != sourceID || freshMove[value.ID] || !value.Enabled || value.AuthStatus != accountdomain.AuthStatusActive || value.EgressAssignmentMode != accountdomain.EgressAssignmentAuto {
			continue
		}
		if value.EgressAssignedAt != nil && now.Sub(value.EgressAssignedAt.UTC()) < autoAssignmentMigrationCooldown {
			continue
		}
		return value.ID, true
	}
	return 0, false
}

func (s *Service) RunMaintenance(ctx context.Context) error {
	operations, err := s.operationsRepository()
	if err != nil {
		return err
	}
	config, err := operations.GetEgressOperationsConfig(ctx)
	if err != nil {
		return err
	}
	var resultErr error
	sources, err := operations.ListDueEgressSources(ctx, time.Now().UTC(), 3)
	if err != nil {
		resultErr = errors.Join(resultErr, err)
	} else {
		for _, source := range sources {
			if _, syncErr := s.syncSource(ctx, operations, source); syncErr != nil {
				resultErr = errors.Join(resultErr, syncErr)
			}
		}
	}
	nodes, err := operations.ListDueEgressNodes(ctx, time.Now().UTC(), time.Duration(config.ProbeIntervalSeconds)*time.Second, 32)
	if err != nil {
		resultErr = errors.Join(resultErr, err)
	} else if len(nodes) > 0 {
		ids := make([]uint64, 0, len(nodes))
		for _, node := range nodes {
			ids = append(ids, node.ID)
		}
		if _, probeErr := s.TestNodes(ctx, ids); probeErr != nil {
			resultErr = errors.Join(resultErr, probeErr)
		}
	}
	if config.AutoAssignEnabled || config.AutoBalanceEnabled {
		s.mu.Lock()
		due := !s.assignmentRunning && (s.lastAssignmentRun.IsZero() || time.Since(s.lastAssignmentRun) >= time.Duration(config.AssignmentIntervalSeconds)*time.Second)
		if due {
			s.assignmentRunning = true
		}
		s.mu.Unlock()
		if due {
			_, balanceErr := s.RebalanceAccounts(ctx, config.AutoAssignEnabled, config.AutoBalanceEnabled, time.Duration(config.ProbeIntervalSeconds)*time.Second)
			s.mu.Lock()
			s.assignmentRunning = false
			if balanceErr == nil {
				s.lastAssignmentRun = time.Now().UTC()
			}
			s.mu.Unlock()
			if balanceErr != nil {
				resultErr = errors.Join(resultErr, balanceErr)
			}
		}
	}
	return resultErr
}
