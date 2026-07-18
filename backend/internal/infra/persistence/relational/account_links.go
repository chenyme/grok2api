package relational

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
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
	return nil
}

// ReconcileProviderLinks 只建立无歧义的高可信关系；已有不同关系和多候选均保持不变。
func (r *AccountRepository) ReconcileProviderLinks(ctx context.Context, accountID uint64) error {
	if accountID == 0 {
		return repository.ErrNotFound
	}
	return mapError(r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
