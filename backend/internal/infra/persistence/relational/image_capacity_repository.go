package relational

import (
	"context"
	"errors"
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// SummarizeSuccessfulImageCoverage projects only aggregate evidence for one
// exact client key, explicit image route, trusted time boundary, and run-marker
// namespace. Names, request IDs, and raw attempts never leave the repository.
func (r *AuditRepository) SummarizeSuccessfulImageCoverage(ctx context.Context, input repository.SuccessfulImageCoverageQuery) (repository.SuccessfulImageCoverage, error) {
	if input.ClientKeyID == 0 || input.ModelRouteID == 0 || input.Since.IsZero() || input.Until.IsZero() || input.Since.After(input.Until) || input.RunMarker == "" {
		return repository.SuccessfulImageCoverage{}, errors.New("successful image coverage query is incomplete")
	}
	type coverageRow struct {
		AccountID uint64
		Successes int64
	}
	var rows []coverageRow
	err := r.db.db.WithContext(ctx).
		Table("request_audits").
		Select("account_id, COUNT(*) AS successes").
		Where(
			"client_key_id = ? AND model_route_id = ? AND provider = ? AND operation = ? AND status_code >= ? AND status_code < ? AND (error_code IS NULL OR error_code = '') AND media_output_images > 0 AND account_id IS NOT NULL AND created_at >= ? AND created_at <= ? AND SUBSTR(request_id, 1, ?) = ?",
			input.ClientKeyID,
			input.ModelRouteID,
			string(account.ProviderWeb),
			string(audit.OperationImage),
			http.StatusOK,
			http.StatusMultipleChoices,
			input.Since.UTC(),
			input.Until.UTC(),
			len(input.RunMarker)+1,
			input.RunMarker+":",
		).
		Group("account_id").
		Order("account_id ASC").
		Scan(&rows).Error
	if err != nil {
		return repository.SuccessfulImageCoverage{}, err
	}
	result := repository.SuccessfulImageCoverage{AccountIDs: make([]uint64, 0, len(rows))}
	for _, row := range rows {
		if row.AccountID == 0 || row.Successes <= 0 {
			continue
		}
		result.AccountIDs = append(result.AccountIDs, row.AccountID)
		result.TerminalSuccessCount += row.Successes
	}
	return result, nil
}
