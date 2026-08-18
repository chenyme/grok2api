package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

const (
	ErrorQualityDegraded      = "quality_degraded"
	qualityRetryFailOpen      = "fail_open"
	qualityRetryFailClosed    = "fail_closed"
	defaultQualityMaxAttempts = 6
	defaultQualityHoldTimeout = 3 * time.Second
	defaultQualityMinOutput   = int64(32)
)

var errQualityDegraded = errors.New("上游响应缺少推理")

// QualityRetryRuntime is the isolated request-path withhold/retry policy.
// Zero Enabled leaves production behavior unchanged.
type QualityRetryRuntime struct {
	Enabled         bool
	MaxAttempts     int
	HoldTimeout     time.Duration
	MinOutputTokens int64
	OnExhausted     string
}

// QualityStreamSignals is the hold classifier input. Tests drive this
// directly and via ObserveQualityChunk on SSE fixtures.
type QualityStreamSignals struct {
	HasThinking     bool
	VisibleTokens   int64
	ReasoningTokens int64
	OutputTokens    int64
	Terminal        bool
	HoldExpired     bool
}

// QualityVerdict is the hold decision for one upstream stream.
type QualityVerdict string

const (
	QualityWait     QualityVerdict = "wait"
	QualityDeliver  QualityVerdict = "deliver"
	QualityWithhold QualityVerdict = "withhold"
)

// QualityRetryAction is what the attempt loop does with a withhold verdict.
type QualityRetryAction string

const (
	QualityActionDeliver     QualityRetryAction = "deliver"
	QualityActionDeliverLast QualityRetryAction = "deliver_last"
	QualityActionRetry       QualityRetryAction = "retry"
	QualityActionReject      QualityRetryAction = "reject"
)

func normalizeQualityRetry(cfg QualityRetryRuntime) QualityRetryRuntime {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultQualityMaxAttempts
	}
	if cfg.HoldTimeout <= 0 {
		cfg.HoldTimeout = defaultQualityHoldTimeout
	}
	if cfg.MinOutputTokens <= 0 {
		cfg.MinOutputTokens = defaultQualityMinOutput
	}
	cfg.OnExhausted = normalizeQualityExhaustionPolicy(cfg.OnExhausted)
	return cfg
}

func (s *Service) UpdateQualityRetry(cfg QualityRetryRuntime) {
	normalized := normalizeQualityRetry(cfg)
	s.qualityRetry.Store(&normalized)
}

func (s *Service) qualityRetryConfig() QualityRetryRuntime {
	if s == nil {
		return normalizeQualityRetry(QualityRetryRuntime{})
	}
	if value := s.qualityRetry.Load(); value != nil {
		return *value
	}
	return normalizeQualityRetry(QualityRetryRuntime{})
}

// ClassifyQualityHold decides whether a held stream may be forwarded.
// Thinking (or reasoning tokens) always delivers. A finished or expired
// sample with enough visible output and no reasoning is 降智 and withheld.
// Short replies below minOutput are delivered so "ok"/"yes" is not retried.
func ClassifyQualityHold(sig QualityStreamSignals, minOutput int64) QualityVerdict {
	if minOutput <= 0 {
		minOutput = defaultQualityMinOutput
	}
	if sig.HasThinking || sig.ReasoningTokens > 0 {
		return QualityDeliver
	}
	output := sig.OutputTokens
	if output < sig.VisibleTokens {
		output = sig.VisibleTokens
	}
	enough := output >= minOutput
	if sig.Terminal {
		if enough {
			return QualityWithhold
		}
		return QualityDeliver
	}
	if enough {
		return QualityWithhold
	}
	if sig.HoldExpired {
		return QualityDeliver
	}
	return QualityWait
}

// DecideQualityRetry caps withhold recovery at maxAttempts (default 6:
// original + five extra accounts). The last withhold
// (attemptIndex == maxAttempts-1) is fail-open unless OnExhausted is fail_closed.
func DecideQualityRetry(verdict QualityVerdict, attemptIndex, maxAttempts int, onExhausted string) QualityRetryAction {
	if verdict != QualityWithhold {
		return QualityActionDeliver
	}
	if maxAttempts <= 0 {
		maxAttempts = defaultQualityMaxAttempts
	}
	if attemptIndex < 0 {
		attemptIndex = 0
	}
	if attemptIndex < maxAttempts-1 {
		return QualityActionRetry
	}
	// attemptIndex == maxAttempts-1 (or past it): do not retry again.
	if normalizeQualityExhaustionPolicy(onExhausted) == qualityRetryFailClosed {
		return QualityActionReject
	}
	return QualityActionDeliverLast
}

// BoundQualityRetry turns a Retry into DeliverLast/Reject when the routing
// loop has no remaining account slot, so the already-held body is not dropped
// on continue-into-exhausted-loop.
func BoundQualityRetry(action QualityRetryAction, hasNextRoutingAttempt bool, onExhausted string) QualityRetryAction {
	if action != QualityActionRetry || hasNextRoutingAttempt {
		return action
	}
	if normalizeQualityExhaustionPolicy(onExhausted) == qualityRetryFailClosed {
		return QualityActionReject
	}
	return QualityActionDeliverLast
}

func normalizeQualityExhaustionPolicy(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), qualityRetryFailOpen) {
		return qualityRetryFailOpen
	}
	return qualityRetryFailClosed
}

// QualityCommit is the single attempt-loop decision for a held stream.
type QualityCommit struct {
	Action   QualityRetryAction
	Audit    bool
	KeepBody bool
}

// CommitQualityHold is the shipped withhold/retry/commit unit. The attempt
// loop must not re-derive this from Decide+Bound+switch.
func CommitQualityHold(verdict QualityVerdict, qualityAttempt, maxAttempts int, hasNextRouting bool, onExhausted string) QualityCommit {
	action := BoundQualityRetry(
		DecideQualityRetry(verdict, qualityAttempt, maxAttempts, onExhausted),
		hasNextRouting,
		onExhausted,
	)
	switch action {
	case QualityActionRetry, QualityActionReject:
		return QualityCommit{Action: action, Audit: true, KeepBody: false}
	case QualityActionDeliverLast:
		return QualityCommit{Action: action, Audit: false, KeepBody: true}
	default:
		return QualityCommit{Action: QualityActionDeliver, Audit: false, KeepBody: true}
	}
}

func shouldHoldQualityStream(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg QualityRetryRuntime) bool {
	if !cfg.Enabled || !input.Streaming || input.ForcedEgressNodeID != 0 || ownership != nil {
		return false
	}
	switch operation {
	case audit.OperationChat, audit.OperationResponses, audit.OperationMessages, "":
	default:
		return false
	}
	if route.Provider != accountdomain.ProviderBuild && route.Provider != accountdomain.ProviderConsole {
		return false
	}
	// Retrying a tool-capable request can repeat external side effects. Tool
	// requests remain outside this feature until they have their own replay
	// safety contract.
	if qualityRequestUsesTools(input.Body) {
		return false
	}
	// Aliases are rewritten before this gate, so inspect the effective request
	// body instead of only the reasoning-capable base model. In particular,
	// grok-4.3-none becomes grok-4.3 plus an explicit disabled setting.
	if qualityRequestDisablesReasoning(input.Body) {
		return false
	}
	if modeldomain.SupportsReasoningForProvider(route.Provider, input.PublicModel) {
		return true
	}
	return modeldomain.SupportsReasoningForProvider(route.Provider, route.UpstreamModel)
}

func qualityRequestUsesTools(body []byte) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	return nonEmptyJSONCollection(payload["tools"]) || nonEmptyJSONCollection(payload["functions"])
}

func qualityRequestDisablesReasoning(body []byte) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	if jsonStringEquals(payload["reasoning_effort"], modeldomain.ReasoningEffortNone) {
		return true
	}
	for _, key := range []string{"reasoning", "output_config", "thinking"} {
		var nested map[string]json.RawMessage
		if json.Unmarshal(payload[key], &nested) != nil {
			continue
		}
		if jsonStringEquals(nested["effort"], modeldomain.ReasoningEffortNone) || jsonStringEquals(nested["type"], "disabled") {
			return true
		}
		var budget int64
		if raw, ok := nested["budget_tokens"]; ok && json.Unmarshal(raw, &budget) == nil && budget == 0 {
			return true
		}
	}
	return jsonStringEquals(payload["thinking"], "disabled")
}

func nonEmptyJSONCollection(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "[]" || trimmed == "{}" {
		return false
	}
	return strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{")
}

func jsonStringEquals(raw json.RawMessage, want string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.EqualFold(strings.TrimSpace(value), want)
}

func (s *Service) recordQualityDegraded(ctx context.Context, base audit.Record, credential accountdomain.Credential, usage Usage, startedAt time.Time, trace *infraegress.Trace, provider accountdomain.Provider) {
	record := base
	record.EventID = newAuditEventID()
	accountID := credential.ID
	record.AccountID = &accountID
	record.AccountName = credential.Name
	record.StatusCode = http.StatusOK
	record.ErrorCode = ErrorQualityDegraded
	record.OutputTokens = usage.OutputTokens
	record.ReasoningTokens = usage.ReasoningTokens
	record.TotalTokens = usage.TotalTokens
	record.InputTokens = usage.InputTokens
	if usage.Reported {
		record.UsageSource = audit.UsageSourceUpstream
	}
	record.DurationMS = time.Since(startedAt).Milliseconds()
	record.CreatedAt = time.Now().UTC()
	applyAuditEgress(&record, trace, provider)
	if err := s.audits.Create(ctx, record); err != nil {
		s.logger.Error("quality_degraded_audit_failed", "event_id", record.EventID, "request_id", record.RequestID, "error", err)
	}
}
