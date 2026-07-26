package relational

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func linkWebToConsole(tx *gorm.DB, webAccountID, consoleAccountID uint64) error {
	var webAccount, consoleAccount accountModel
	if err := tx.Select("id", "provider").First(&webAccount, webAccountID).Error; err != nil {
		return err
	}
	if err := tx.Select("id", "provider").First(&consoleAccount, consoleAccountID).Error; err != nil {
		return err
	}
	if webAccount.Provider != string(account.ProviderWeb) || consoleAccount.Provider != string(account.ProviderConsole) {
		return repository.ErrConflict
	}
	var existing webConsoleAccountLinkModel
	err := tx.Where("web_account_id = ? OR console_account_id = ?", webAccountID, consoleAccountID).First(&existing).Error
	if err == nil {
		if existing.WebAccountID == webAccountID && existing.ConsoleAccountID == consoleAccountID {
			return nil
		}
		slog.Debug("account_provider_link_reconcile_skipped",
			"relation", "web_console",
			"reason", "existing_relation_conflict",
			"web_account_id", webAccountID,
			"console_account_id", consoleAccountID,
			"existing_web_account_id", existing.WebAccountID,
			"existing_console_account_id", existing.ConsoleAccountID,
		)
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return tx.Create(&webConsoleAccountLinkModel{WebAccountID: webAccountID, ConsoleAccountID: consoleAccountID, CreatedAt: time.Now().UTC()}).Error
}

func (r *AccountRepository) UpdateIdentityMetadata(ctx context.Context, accountID uint64, email, userID, teamID string) error {
	if accountID == 0 {
		return repository.ErrNotFound
	}
	updates := make(map[string]any, 3)
	if email = strings.TrimSpace(email); email != "" {
		updates["email"] = email
	}
	if userID = strings.TrimSpace(userID); userID != "" {
		updates["user_id"] = userID
	}
	if teamID = strings.TrimSpace(teamID); teamID != "" {
		updates["team_id"] = teamID
	}
	if len(updates) == 0 {
		return nil
	}
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("id = ?", accountID).Updates(updates)
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, AccountID: accountID})
	return nil
}

// ReconcileProviderLinks 只建立无歧义的高可信关系；已有不同关系和多候选均保持不变。
func (r *AccountRepository) ReconcileProviderLinks(ctx context.Context, accountID uint64) error {
	if accountID == 0 {
		return repository.ErrNotFound
	}
	err := mapError(r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var value accountModel
		if err := tx.Select("id", "provider", "source_key", "user_id", "team_id").First(&value, accountID).Error; err != nil {
			return err
		}
		switch account.Provider(value.Provider) {
		case account.ProviderWeb:
			if consoleSource, ok := matchingConsoleSourceKey(value.SourceKey); ok {
				if candidate, found, err := uniqueLinkCandidate(tx, value.ID, "web_console", account.ProviderConsole, "source_key = ?", consoleSource); err != nil {
					return err
				} else if found {
					if err := linkWebToConsole(tx, value.ID, candidate.ID); err != nil {
						return err
					}
				}
			}
			if err := reconcileWebConsoleByUserID(tx, value, true); err != nil {
				return err
			}
			return reconcileWebBuildByUserID(tx, value, true)
		case account.ProviderConsole:
			if webSource, ok := matchingWebSourceKey(value.SourceKey); ok {
				if candidate, found, err := uniqueLinkCandidate(tx, value.ID, "web_console", account.ProviderWeb, "source_key = ?", webSource); err != nil {
					return err
				} else if found {
					return linkWebToConsole(tx, candidate.ID, value.ID)
				}
			}
			return reconcileWebConsoleByUserID(tx, value, false)
		case account.ProviderBuild:
			return reconcileWebBuildByUserID(tx, value, false)
		}
		return nil
	}))
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged, AccountID: accountID})
	}
	return err
}

func reconcileWebConsoleByUserID(tx *gorm.DB, value accountModel, valueIsWeb bool) error {
	userID := strings.TrimSpace(value.UserID)
	if userID == "" {
		return nil
	}
	provider := account.ProviderWeb
	if valueIsWeb {
		provider = account.ProviderConsole
	}
	candidate, found, err := uniqueLinkCandidate(tx, value.ID, "web_console", provider, "user_id = ?", userID)
	if err != nil || !found {
		return err
	}
	webID, consoleID := candidate.ID, value.ID
	if valueIsWeb {
		webID, consoleID = value.ID, candidate.ID
	}
	return linkWebToConsole(tx, webID, consoleID)
}

func reconcileWebBuildByUserID(tx *gorm.DB, value accountModel, valueIsWeb bool) error {
	userID := strings.TrimSpace(value.UserID)
	if userID == "" {
		return nil
	}
	provider := account.ProviderWeb
	if valueIsWeb {
		provider = account.ProviderBuild
	}
	candidate, found, err := uniqueLinkCandidate(tx, value.ID, "web_build", provider, "user_id = ?", userID)
	if err != nil || !found {
		return err
	}
	webID, buildID := candidate.ID, value.ID
	if valueIsWeb {
		webID, buildID = value.ID, candidate.ID
	}
	return linkWebToBuildIfUnambiguous(tx, webID, buildID)
}

func linkWebToBuildIfUnambiguous(tx *gorm.DB, webAccountID, buildAccountID uint64) error {
	var existing accountProviderLinkModel
	err := tx.Where("web_account_id = ? OR build_account_id = ?", webAccountID, buildAccountID).First(&existing).Error
	if err == nil {
		if existing.WebAccountID != webAccountID || existing.BuildAccountID != buildAccountID {
			slog.Debug("account_provider_link_reconcile_skipped",
				"relation", "web_build",
				"reason", "existing_relation_conflict",
				"web_account_id", webAccountID,
				"build_account_id", buildAccountID,
				"existing_web_account_id", existing.WebAccountID,
				"existing_build_account_id", existing.BuildAccountID,
			)
		}
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return tx.Create(&accountProviderLinkModel{WebAccountID: webAccountID, BuildAccountID: buildAccountID, CreatedAt: time.Now().UTC()}).Error
}

func uniqueLinkCandidate(tx *gorm.DB, sourceAccountID uint64, relation string, provider account.Provider, predicate string, args ...any) (accountModel, bool, error) {
	var values []accountModel
	err := tx.Select("id", "provider", "source_key", "user_id", "team_id").
		Where("provider = ?", provider).Where(predicate, args...).Limit(2).Find(&values).Error
	if err != nil {
		return accountModel{}, false, err
	}
	if len(values) > 1 {
		slog.Debug("account_provider_link_reconcile_skipped",
			"relation", relation,
			"reason", "ambiguous_candidates",
			"account_id", sourceAccountID,
			"candidate_provider", provider,
			"candidate_count", len(values),
		)
	}
	if len(values) != 1 {
		return accountModel{}, false, nil
	}
	return values[0], true, nil
}

func matchingConsoleSourceKey(webSourceKey string) (string, bool) {
	if _, ok := egressIdentityFromWebSourceKey(webSourceKey); !ok {
		return "", false
	}
	return "console-" + strings.TrimSpace(webSourceKey), true
}

func matchingWebSourceKey(consoleSourceKey string) (string, bool) {
	value := strings.TrimSpace(consoleSourceKey)
	if !strings.HasPrefix(value, "console-") {
		return "", false
	}
	webSource := strings.TrimPrefix(value, "console-")
	if _, ok := egressIdentityFromWebSourceKey(webSource); !ok {
		return "", false
	}
	return webSource, true
}

// ResolveLinkedDeleteIDs expands root IDs with linked peers from binding tables only.
// Paths are fixed: Web↔Build and Web↔Console one hop; Build↔Console is two hops via Web
// without deleting the intermediate Web unless it is also a target/root.
// Preview path only — deletes must use DeleteManyWithLinked so resolve+delete share one transaction.
func (r *AccountRepository) ResolveLinkedDeleteIDs(ctx context.Context, providerValue account.Provider, rootIDs []uint64, targets []account.Provider) (repository.LinkedDeleteResolution, error) {
	return resolveLinkedDeleteIDs(r.db.db.WithContext(ctx), providerValue, rootIDs, targets)
}

// resolveLinkedDeleteIDs is the shared expander used by preview and by DeleteManyWithLinked (tx).
func resolveLinkedDeleteIDs(db *gorm.DB, providerValue account.Provider, rootIDs []uint64, targets []account.Provider) (repository.LinkedDeleteResolution, error) {
	result := repository.LinkedDeleteResolution{
		LinkedByProvider: map[account.Provider]int{},
	}
	if !providerValue.IsValid() {
		return result, fmt.Errorf("账号来源无效")
	}
	roots := uniqueSortedIDs(rootIDs)
	result.RootIDs = append([]uint64(nil), roots...)
	if len(roots) == 0 {
		result.FinalIDs = nil
		return result, nil
	}
	targetSet := make(map[account.Provider]struct{}, len(targets))
	for _, target := range targets {
		if !target.IsValid() {
			return result, fmt.Errorf("关联删除目标无效")
		}
		if target == providerValue {
			return result, fmt.Errorf("关联删除目标不能包含当前号池")
		}
		targetSet[target] = struct{}{}
	}
	if len(targetSet) == 0 {
		result.FinalIDs = append([]uint64(nil), roots...)
		return result, nil
	}

	finalSet := make(map[uint64]struct{}, len(roots)*2)
	for _, id := range roots {
		finalSet[id] = struct{}{}
	}
	linkedSets := map[account.Provider]map[uint64]struct{}{
		account.ProviderWeb:     {},
		account.ProviderBuild:   {},
		account.ProviderConsole: {},
	}
	addLinked := func(provider account.Provider, ids []uint64) {
		if len(ids) == 0 {
			return
		}
		set := linkedSets[provider]
		for _, id := range ids {
			if id == 0 {
				continue
			}
			if _, isRoot := finalSet[id]; isRoot {
				continue
			}
			if _, exists := set[id]; exists {
				continue
			}
			set[id] = struct{}{}
			finalSet[id] = struct{}{}
		}
	}

	switch providerValue {
	case account.ProviderWeb:
		if _, ok := targetSet[account.ProviderBuild]; ok {
			ids, err := listBuildIDsForWebs(db, roots)
			if err != nil {
				return result, err
			}
			addLinked(account.ProviderBuild, ids)
		}
		if _, ok := targetSet[account.ProviderConsole]; ok {
			ids, err := listConsoleIDsForWebs(db, roots)
			if err != nil {
				return result, err
			}
			addLinked(account.ProviderConsole, ids)
		}
	case account.ProviderBuild:
		webIDs, err := listWebIDsForBuilds(db, roots)
		if err != nil {
			return result, err
		}
		if _, ok := targetSet[account.ProviderWeb]; ok {
			addLinked(account.ProviderWeb, webIDs)
		}
		if _, ok := targetSet[account.ProviderConsole]; ok {
			ids, err := listConsoleIDsForWebs(db, webIDs)
			if err != nil {
				return result, err
			}
			addLinked(account.ProviderConsole, ids)
		}
	case account.ProviderConsole:
		webIDs, err := listWebIDsForConsoles(db, roots)
		if err != nil {
			return result, err
		}
		if _, ok := targetSet[account.ProviderWeb]; ok {
			addLinked(account.ProviderWeb, webIDs)
		}
		if _, ok := targetSet[account.ProviderBuild]; ok {
			ids, err := listBuildIDsForWebs(db, webIDs)
			if err != nil {
				return result, err
			}
			addLinked(account.ProviderBuild, ids)
		}
	default:
		return result, fmt.Errorf("账号来源无效")
	}

	for provider, set := range linkedSets {
		if len(set) == 0 {
			continue
		}
		result.LinkedByProvider[provider] = len(set)
	}
	result.FinalIDs = make([]uint64, 0, len(finalSet))
	for id := range finalSet {
		result.FinalIDs = append(result.FinalIDs, id)
	}
	sort.Slice(result.FinalIDs, func(i, j int) bool { return result.FinalIDs[i] < result.FinalIDs[j] })
	return result, nil
}

// DeleteManyWithLinked locks existing roots (optionally filtered by provider), expands linked
// peers from binding tables, re-locks the final set, rejects active media jobs, and deletes
// everything in one transaction so concurrent re-link/unlink cannot race the snapshot.
func (r *AccountRepository) DeleteManyWithLinked(ctx context.Context, providerValue account.Provider, rootIDs []uint64, targets []account.Provider) (repository.LinkedDeleteResolution, int64, error) {
	var resolution repository.LinkedDeleteResolution
	var deleted int64
	roots := uniqueSortedIDs(rootIDs)
	if len(roots) == 0 {
		resolution.LinkedByProvider = map[account.Provider]int{}
		return resolution, 0, nil
	}

	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) Lock currently existing root rows. When provider is set, only roots in that pool.
		rootQuery := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", roots)
		if providerValue.IsValid() {
			rootQuery = rootQuery.Where("provider = ?", providerValue)
		}
		var lockedRoots []uint64
		if err := rootQuery.Order("id ASC").Pluck("id", &lockedRoots).Error; err != nil {
			return err
		}
		if len(lockedRoots) == 0 {
			resolution = repository.LinkedDeleteResolution{
				RootIDs:          nil,
				FinalIDs:         nil,
				LinkedByProvider: map[account.Provider]int{},
			}
			deleted = 0
			return nil
		}

		// 2) Expand peers inside the same transaction (consistent link-table read after root locks).
		var err error
		if len(targets) == 0 {
			resolution = repository.LinkedDeleteResolution{
				RootIDs:          append([]uint64(nil), lockedRoots...),
				FinalIDs:         append([]uint64(nil), lockedRoots...),
				LinkedByProvider: map[account.Provider]int{},
			}
		} else {
			if !providerValue.IsValid() {
				return fmt.Errorf("账号来源无效")
			}
			resolution, err = resolveLinkedDeleteIDs(tx, providerValue, lockedRoots, targets)
			if err != nil {
				return err
			}
		}
		if len(resolution.FinalIDs) == 0 {
			deleted = 0
			return nil
		}

		// 3) Lock the full final set (roots + peers) before media check / delete.
		var lockedFinal []uint64
		if err := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id IN ?", resolution.FinalIDs).Order("id ASC").Pluck("id", &lockedFinal).Error; err != nil {
			return err
		}
		if len(lockedFinal) == 0 {
			deleted = 0
			return nil
		}

		// 4) Media jobs on any final id block the whole transaction.
		if err := rejectAccountsWithMediaJobs(tx, lockedFinal); err != nil {
			return err
		}

		// 5) Delete locked final rows.
		result := tx.Where("id IN ?", lockedFinal).Delete(&accountModel{})
		deleted = result.RowsAffected
		// Align reported final set with rows that actually existed under lock.
		resolution.FinalIDs = append([]uint64(nil), lockedFinal...)
		return result.Error
	})
	if err != nil {
		return repository.LinkedDeleteResolution{}, 0, err
	}
	if deleted > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return resolution, deleted, nil
}

func uniqueSortedIDs(ids []uint64) []uint64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func listBuildIDsForWebs(db *gorm.DB, webIDs []uint64) ([]uint64, error) {
	if len(webIDs) == 0 {
		return nil, nil
	}
	var ids []uint64
	err := db.Table("account_provider_links").Where("web_account_id IN ?", webIDs).Pluck("build_account_id", &ids).Error
	return ids, err
}

func listWebIDsForBuilds(db *gorm.DB, buildIDs []uint64) ([]uint64, error) {
	if len(buildIDs) == 0 {
		return nil, nil
	}
	var ids []uint64
	err := db.Table("account_provider_links").Where("build_account_id IN ?", buildIDs).Pluck("web_account_id", &ids).Error
	return ids, err
}

func listConsoleIDsForWebs(db *gorm.DB, webIDs []uint64) ([]uint64, error) {
	if len(webIDs) == 0 {
		return nil, nil
	}
	var ids []uint64
	err := db.Table("web_console_account_links").Where("web_account_id IN ?", webIDs).Pluck("console_account_id", &ids).Error
	return ids, err
}

func listWebIDsForConsoles(db *gorm.DB, consoleIDs []uint64) ([]uint64, error) {
	if len(consoleIDs) == 0 {
		return nil, nil
	}
	var ids []uint64
	err := db.Table("web_console_account_links").Where("console_account_id IN ?", consoleIDs).Pluck("web_account_id", &ids).Error
	return ids, err
}
