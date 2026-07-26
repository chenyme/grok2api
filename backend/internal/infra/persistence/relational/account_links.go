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

const (
	accountLinkAdvisoryNamespace int32 = 0x47524F4B // GROK
	accountLinkAdvisoryOperation int32 = 0x4C4E4B44 // LNKD
	accountLinkLockTimeout             = 5 * time.Second
)

// lockAccountLinkMutation serializes link-table writes and account deletions on PostgreSQL.
// These maintenance operations are rare, and a shared transaction-scoped lock prevents
// link snapshot races and cyclic account/FK row-lock acquisition across mutation paths.
func lockAccountLinkMutation(tx *gorm.DB) error {
	return lockAccountLinkMutationWithTimeout(tx, accountLinkLockTimeout)
}

func lockAccountLinkMutationWithTimeout(tx *gorm.DB, timeout time.Duration) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	if timeout <= 0 {
		timeout = accountLinkLockTimeout
	}
	parentCtx := tx.Statement.Context
	lockCtx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()
	err := tx.WithContext(lockCtx).Exec("SELECT pg_advisory_xact_lock(?, ?)", accountLinkAdvisoryNamespace, accountLinkAdvisoryOperation).Error
	if err != nil && parentCtx.Err() != nil {
		return parentCtx.Err()
	}
	if err != nil && errors.Is(lockCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: 账号关联关系正在变更，请稍后重试", repository.ErrConflict)
	}
	return err
}

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
		if err := lockAccountLinkMutation(tx); err != nil {
			return err
		}
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

// resolveLinkedDeleteIDs is the shared expander used by preview and by the delete transactions.
// It also returns RootGroups for group-level skips and PeerProviders for per-provider delete counts.
func resolveLinkedDeleteIDs(db *gorm.DB, providerValue account.Provider, rootIDs []uint64, targets []account.Provider) (repository.LinkedDeleteResolution, error) {
	result := repository.LinkedDeleteResolution{
		LinkedByProvider: map[account.Provider]int{},
		RootGroups:       map[uint64][]uint64{},
		PeerProviders:    map[uint64]account.Provider{},
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
	linkedCounts := map[account.Provider]int{}
	addPairs := func(peerProvider account.Provider, pairs []accountLinkPair) {
		for _, pair := range pairs {
			if pair.RootID == 0 || pair.PeerID == 0 {
				continue
			}
			// Skip roots and peers already claimed by an earlier 1:1 group.
			if _, exists := finalSet[pair.PeerID]; exists {
				continue
			}
			finalSet[pair.PeerID] = struct{}{}
			linkedCounts[peerProvider]++
			result.RootGroups[pair.RootID] = append(result.RootGroups[pair.RootID], pair.PeerID)
			result.PeerProviders[pair.PeerID] = peerProvider
		}
	}

	switch providerValue {
	case account.ProviderWeb:
		if _, ok := targetSet[account.ProviderBuild]; ok {
			pairs, err := listWebBuildPairs(db, roots)
			if err != nil {
				return result, err
			}
			addPairs(account.ProviderBuild, pairs)
		}
		if _, ok := targetSet[account.ProviderConsole]; ok {
			pairs, err := listWebConsolePairs(db, roots)
			if err != nil {
				return result, err
			}
			addPairs(account.ProviderConsole, pairs)
		}
	case account.ProviderBuild:
		if _, ok := targetSet[account.ProviderWeb]; ok {
			pairs, err := listBuildWebPairs(db, roots)
			if err != nil {
				return result, err
			}
			addPairs(account.ProviderWeb, pairs)
		}
		if _, ok := targetSet[account.ProviderConsole]; ok {
			pairs, err := listBuildConsolePairs(db, roots)
			if err != nil {
				return result, err
			}
			addPairs(account.ProviderConsole, pairs)
		}
	case account.ProviderConsole:
		if _, ok := targetSet[account.ProviderWeb]; ok {
			pairs, err := listConsoleWebPairs(db, roots)
			if err != nil {
				return result, err
			}
			addPairs(account.ProviderWeb, pairs)
		}
		if _, ok := targetSet[account.ProviderBuild]; ok {
			pairs, err := listConsoleBuildPairs(db, roots)
			if err != nil {
				return result, err
			}
			addPairs(account.ProviderBuild, pairs)
		}
	default:
		return result, fmt.Errorf("账号来源无效")
	}

	for provider, count := range linkedCounts {
		if count > 0 {
			result.LinkedByProvider[provider] = count
		}
	}
	result.FinalIDs = make([]uint64, 0, len(finalSet))
	for id := range finalSet {
		result.FinalIDs = append(result.FinalIDs, id)
	}
	sort.Slice(result.FinalIDs, func(i, j int) bool { return result.FinalIDs[i] < result.FinalIDs[j] })
	return result, nil
}

// deleteLinkedGroupsTx expands links, locks the final set, checks media jobs, and deletes rows while roots are locked.
// skipMedia=false rejects the operation; skipMedia=true skips each blocked root group as a unit.
func deleteLinkedGroupsTx(tx *gorm.DB, providerValue account.Provider, lockedRoots []uint64, targets []account.Provider, skipMedia bool) (repository.LinkedDeleteOutcome, error) {
	outcome := repository.LinkedDeleteOutcome{
		LinkedDeletedByProvider: map[account.Provider]int64{},
	}

	// 1) Expand links while the caller holds root row locks.
	var resolution repository.LinkedDeleteResolution
	var err error
	if len(targets) == 0 {
		resolution = repository.LinkedDeleteResolution{
			RootIDs:          append([]uint64(nil), lockedRoots...),
			FinalIDs:         append([]uint64(nil), lockedRoots...),
			LinkedByProvider: map[account.Provider]int{},
			RootGroups:       map[uint64][]uint64{},
			PeerProviders:    map[uint64]account.Provider{},
		}
	} else {
		if !providerValue.IsValid() {
			return outcome, fmt.Errorf("账号来源无效")
		}
		resolution, err = resolveLinkedDeleteIDs(tx, providerValue, lockedRoots, targets)
		if err != nil {
			return outcome, err
		}
	}
	outcome.Resolution = resolution
	if len(resolution.FinalIDs) == 0 {
		return outcome, nil
	}

	// 2) Lock roots and linked peers in the final set.
	var lockedFinal []uint64
	if err := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", resolution.FinalIDs).Order("id ASC").Pluck("id", &lockedFinal).Error; err != nil {
		return outcome, err
	}
	if len(lockedFinal) == 0 {
		return outcome, nil
	}
	outcome.Resolution.FinalIDs = append([]uint64(nil), lockedFinal...)

	// 3) Apply active-media protection.
	deletable := lockedFinal
	if skipMedia {
		var blocked []uint64
		if err := tx.Model(&mediaJobModel{}).
			Where("account_id IN ? AND status IN ?", lockedFinal, activeMediaJobStatuses()).
			Pluck("account_id", &blocked).Error; err != nil {
			return outcome, err
		}
		if len(blocked) > 0 {
			// Map blocked peers back to roots and remove each complete group.
			peerRoot := make(map[uint64]uint64, len(resolution.PeerProviders))
			for root, peers := range resolution.RootGroups {
				for _, peer := range peers {
					peerRoot[peer] = root
				}
			}
			skippedRootSet := map[uint64]struct{}{}
			for _, id := range blocked {
				if root, ok := peerRoot[id]; ok {
					skippedRootSet[root] = struct{}{}
				} else {
					skippedRootSet[id] = struct{}{}
				}
			}
			removed := map[uint64]struct{}{}
			for root := range skippedRootSet {
				removed[root] = struct{}{}
				for _, peer := range resolution.RootGroups[root] {
					removed[peer] = struct{}{}
				}
				outcome.SkippedRoots = append(outcome.SkippedRoots, root)
			}
			sort.Slice(outcome.SkippedRoots, func(i, j int) bool { return outcome.SkippedRoots[i] < outcome.SkippedRoots[j] })
			filtered := make([]uint64, 0, len(lockedFinal))
			for _, id := range lockedFinal {
				if _, skip := removed[id]; !skip {
					filtered = append(filtered, id)
				}
			}
			deletable = filtered
		}
	} else {
		if err := rejectAccountsWithMediaJobs(tx, lockedFinal); err != nil {
			return outcome, err
		}
	}
	if len(deletable) == 0 {
		return outcome, nil
	}

	// 4) Delete remaining rows and count them by provider.
	result := tx.Where("id IN ?", deletable).Delete(&accountModel{})
	if result.Error != nil {
		return outcome, result.Error
	}
	outcome.Deleted = result.RowsAffected
	outcome.DeletedIDs = append([]uint64(nil), deletable...)
	rootSet := make(map[uint64]struct{}, len(resolution.RootIDs))
	for _, id := range resolution.RootIDs {
		rootSet[id] = struct{}{}
	}
	for _, id := range deletable {
		if _, isRoot := rootSet[id]; isRoot {
			outcome.RootsDeleted++
		} else if provider, ok := resolution.PeerProviders[id]; ok {
			outcome.LinkedDeletedByProvider[provider]++
		}
	}
	return outcome, nil
}

// DeleteManyWithLinked locks existing roots (optionally filtered by provider), expands linked
// peers from binding tables, re-locks the final set, handles media jobs, and deletes
// everything in one transaction so concurrent re-link/unlink cannot race the snapshot.
func (r *AccountRepository) DeleteManyWithLinked(ctx context.Context, providerValue account.Provider, rootIDs []uint64, targets []account.Provider, skipMedia bool) (repository.LinkedDeleteOutcome, error) {
	outcome := repository.LinkedDeleteOutcome{
		Resolution:              repository.LinkedDeleteResolution{LinkedByProvider: map[account.Provider]int{}},
		LinkedDeletedByProvider: map[account.Provider]int64{},
	}
	roots := uniqueSortedIDs(rootIDs)
	if len(roots) == 0 {
		return outcome, nil
	}

	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAccountLinkMutation(tx); err != nil {
			return err
		}

		// Lock roots in one stable order, then validate their provider without masking missing IDs.
		var lockedRows []struct {
			ID       uint64
			Provider string
		}
		if err := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "provider").Where("id IN ?", roots).Order("id ASC").Find(&lockedRows).Error; err != nil {
			return err
		}
		if len(lockedRows) == 0 {
			return nil
		}
		lockedRoots := make([]uint64, 0, len(lockedRows))
		for _, row := range lockedRows {
			if providerValue.IsValid() && account.Provider(row.Provider) != providerValue {
				return fmt.Errorf("%w: 账号不属于指定号池", repository.ErrConflict)
			}
			lockedRoots = append(lockedRoots, row.ID)
		}
		inner, err := deleteLinkedGroupsTx(tx, providerValue, lockedRoots, targets, skipMedia)
		if err != nil {
			return err
		}
		outcome = inner
		return nil
	})
	if err != nil {
		return repository.LinkedDeleteOutcome{}, err
	}
	if outcome.Deleted > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return outcome, nil
}

// DeleteAccountStatusBatchWithLinked selects at most limit roots by state and ID cursor,
// expands links, skips active-media groups, and returns the candidate count and next cursor.
func (r *AccountRepository) DeleteAccountStatusBatchWithLinked(ctx context.Context, providerValue account.Provider, status string, now time.Time, afterID uint64, limit int, targets []account.Provider) (repository.LinkedDeleteOutcome, int, uint64, error) {
	outcome := repository.LinkedDeleteOutcome{
		Resolution:              repository.LinkedDeleteResolution{LinkedByProvider: map[account.Provider]int{}},
		LinkedDeletedByProvider: map[account.Provider]int64{},
	}
	if limit < 1 {
		return outcome, 0, afterID, nil
	}
	if status != "disabled" && status != "reauthRequired" && status != "cooldown" {
		return outcome, 0, afterID, fmt.Errorf("不支持清理账号状态 %q", status)
	}
	candidateCount := 0
	maxCandidateID := afterID
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAccountLinkMutation(tx); err != nil {
			return err
		}
		// Select, filter, and lock roots in one query to revalidate state inside the batch.
		selection := applyAccountStatusFilter(
			tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).Where("provider = ?", providerValue),
			status, now,
		)
		if afterID > 0 {
			selection = selection.Where("id > ?", afterID)
		}
		var candidates []uint64
		if err := selection.Order("id ASC").Limit(limit).Pluck("id", &candidates).Error; err != nil {
			return err
		}
		if len(candidates) == 0 {
			return nil
		}
		candidateCount = len(candidates)
		maxCandidateID = candidates[len(candidates)-1]
		inner, err := deleteLinkedGroupsTx(tx, providerValue, candidates, targets, true)
		if err != nil {
			return err
		}
		outcome = inner
		return nil
	})
	if err != nil {
		return repository.LinkedDeleteOutcome{}, 0, afterID, err
	}
	if outcome.Deleted > 0 {
		// Linked deletion crosses providers, so invalidate all candidate caches.
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return outcome, candidateCount, maxCandidateID, nil
}

// CountCleanupWithLinked computes preview counts without materializing account IDs.
// Cleanup states are disjoint; linked counts use indexed subqueries and joins for two-hop paths.
func (r *AccountRepository) CountCleanupWithLinked(ctx context.Context, providerValue account.Provider, statuses []string, now time.Time, targets []account.Provider) (repository.CleanupPreview, error) {
	preview := repository.CleanupPreview{
		RootsByStatus:    map[string]int64{},
		LinkedByProvider: map[account.Provider]int64{},
	}
	if !providerValue.IsValid() {
		return preview, fmt.Errorf("账号来源无效")
	}
	db := r.db.db.WithContext(ctx)
	for _, status := range statuses {
		if status != "disabled" && status != "reauthRequired" && status != "cooldown" {
			return preview, fmt.Errorf("不支持清理账号状态 %q", status)
		}
		var rootCount int64
		if err := applyAccountStatusFilter(
			db.Session(&gorm.Session{NewDB: true}).Model(&accountModel{}).Where("provider = ?", providerValue),
			status, now,
		).Count(&rootCount).Error; err != nil {
			return preview, err
		}
		preview.RootsByStatus[status] = rootCount
		preview.RootCount += rootCount

		rootsSub := func() *gorm.DB {
			return applyAccountStatusFilter(
				db.Session(&gorm.Session{NewDB: true}).Model(&accountModel{}).Select("id").Where("provider = ?", providerValue),
				status, now,
			)
		}
		for _, target := range targets {
			if !target.IsValid() || target == providerValue {
				return preview, fmt.Errorf("关联删除目标无效")
			}
			var linked int64
			var err error
			switch {
			case providerValue == account.ProviderWeb && target == account.ProviderBuild:
				err = db.Session(&gorm.Session{NewDB: true}).Table("account_provider_links").
					Where("web_account_id IN (?)", rootsSub()).Count(&linked).Error
			case providerValue == account.ProviderWeb && target == account.ProviderConsole:
				err = db.Session(&gorm.Session{NewDB: true}).Table("web_console_account_links").
					Where("web_account_id IN (?)", rootsSub()).Count(&linked).Error
			case providerValue == account.ProviderBuild && target == account.ProviderWeb:
				err = db.Session(&gorm.Session{NewDB: true}).Table("account_provider_links").
					Where("build_account_id IN (?)", rootsSub()).Count(&linked).Error
			case providerValue == account.ProviderBuild && target == account.ProviderConsole:
				err = db.Session(&gorm.Session{NewDB: true}).Table("account_provider_links apl").
					Joins("JOIN web_console_account_links wcal ON wcal.web_account_id = apl.web_account_id").
					Where("apl.build_account_id IN (?)", rootsSub()).Count(&linked).Error
			case providerValue == account.ProviderConsole && target == account.ProviderWeb:
				err = db.Session(&gorm.Session{NewDB: true}).Table("web_console_account_links").
					Where("console_account_id IN (?)", rootsSub()).Count(&linked).Error
			case providerValue == account.ProviderConsole && target == account.ProviderBuild:
				err = db.Session(&gorm.Session{NewDB: true}).Table("web_console_account_links wcal").
					Joins("JOIN account_provider_links apl ON apl.web_account_id = wcal.web_account_id").
					Where("wcal.console_account_id IN (?)", rootsSub()).Count(&linked).Error
			default:
				return preview, fmt.Errorf("关联删除目标无效")
			}
			if err != nil {
				return preview, err
			}
			preview.LinkedByProvider[target] += linked
		}
	}
	preview.Total = preview.RootCount
	for _, linked := range preview.LinkedByProvider {
		preview.Total += linked
	}
	return preview, nil
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

// accountLinkPair maps a root account to its final peer, including two-hop paths.
type accountLinkPair struct {
	RootID uint64
	PeerID uint64
}

func listWebBuildPairs(db *gorm.DB, webIDs []uint64) ([]accountLinkPair, error) {
	if len(webIDs) == 0 {
		return nil, nil
	}
	var pairs []accountLinkPair
	err := db.Table("account_provider_links").
		Select("web_account_id AS root_id, build_account_id AS peer_id").
		Where("web_account_id IN ?", webIDs).Scan(&pairs).Error
	return pairs, err
}

func listWebConsolePairs(db *gorm.DB, webIDs []uint64) ([]accountLinkPair, error) {
	if len(webIDs) == 0 {
		return nil, nil
	}
	var pairs []accountLinkPair
	err := db.Table("web_console_account_links").
		Select("web_account_id AS root_id, console_account_id AS peer_id").
		Where("web_account_id IN ?", webIDs).Scan(&pairs).Error
	return pairs, err
}

func listBuildWebPairs(db *gorm.DB, buildIDs []uint64) ([]accountLinkPair, error) {
	if len(buildIDs) == 0 {
		return nil, nil
	}
	var pairs []accountLinkPair
	err := db.Table("account_provider_links").
		Select("build_account_id AS root_id, web_account_id AS peer_id").
		Where("build_account_id IN ?", buildIDs).Scan(&pairs).Error
	return pairs, err
}

// listBuildConsolePairs follows Build to Web to Console without returning the intermediate Web.
func listBuildConsolePairs(db *gorm.DB, buildIDs []uint64) ([]accountLinkPair, error) {
	if len(buildIDs) == 0 {
		return nil, nil
	}
	var pairs []accountLinkPair
	err := db.Table("account_provider_links apl").
		Select("apl.build_account_id AS root_id, wcal.console_account_id AS peer_id").
		Joins("JOIN web_console_account_links wcal ON wcal.web_account_id = apl.web_account_id").
		Where("apl.build_account_id IN ?", buildIDs).Scan(&pairs).Error
	return pairs, err
}

func listConsoleWebPairs(db *gorm.DB, consoleIDs []uint64) ([]accountLinkPair, error) {
	if len(consoleIDs) == 0 {
		return nil, nil
	}
	var pairs []accountLinkPair
	err := db.Table("web_console_account_links").
		Select("console_account_id AS root_id, web_account_id AS peer_id").
		Where("console_account_id IN ?", consoleIDs).Scan(&pairs).Error
	return pairs, err
}

// listConsoleBuildPairs follows Console to Web to Build without returning the intermediate Web.
func listConsoleBuildPairs(db *gorm.DB, consoleIDs []uint64) ([]accountLinkPair, error) {
	if len(consoleIDs) == 0 {
		return nil, nil
	}
	var pairs []accountLinkPair
	err := db.Table("web_console_account_links wcal").
		Select("wcal.console_account_id AS root_id, apl.build_account_id AS peer_id").
		Joins("JOIN account_provider_links apl ON apl.web_account_id = wcal.web_account_id").
		Where("wcal.console_account_id IN ?", consoleIDs).Scan(&pairs).Error
	return pairs, err
}
