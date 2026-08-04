import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isRecordOf, isString } from "@/shared/api/decoder";

export type QualityGuardPolicy = {
  mode: "active" | "passive" | "hybrid";
  activeIntervalSeconds: number;
  passivePollSeconds: number;
  softTPS: number;
  hardTPS: number;
  consecutiveSoft: number;
  consecutiveErrors: number;
  quarantineSeconds: number;
  minHealthyNodes: number;
};

export type QualityGuardNodeState = {
  active_soft_strikes: number;
  passive_soft_strikes: number;
  error_strikes: number;
  quarantined_until: number;
  disabled_by_guard: boolean;
  last_reason: string;
  last_probe_at: number;
  last_observed_at: number;
  last_source: string;
  last_classification: string;
  last_output_tps: number;
  last_output_tokens: number;
  last_first_token_ms: number;
  last_duration_ms: number;
};

export type QualityGuardEvent = {
  ts: number;
  event: string;
  node_id: string;
  node_name: string;
  reason: string;
  classification: string;
  output_tps: number;
};

export type QualityGuardDetectionStats = {
  total: number;
  healthy: number;
  soft: number;
  hard: number;
  errors: number;
  output_tokens: number;
};

export type QualityGuardStatistics = {
  started_at: number;
  active: QualityGuardDetectionStats;
  passive: QualityGuardDetectionStats;
  actions: { quarantined: number; restored: number; suppressed: number };
};

export type QualityGuardStatus = {
  available: boolean;
  editable?: boolean;
  startedAt?: number;
  updatedAt?: number;
  lastActiveCycleAt?: number;
  lastPassivePollAt?: number;
  config?: {
    mode: "active" | "passive" | "hybrid";
    model: string;
    node_ids: string[];
    active_interval_seconds: number;
    passive_poll_seconds: number;
    soft_tps: number;
    hard_tps: number;
    consecutive_soft: number;
    consecutive_errors: number;
    quarantine_seconds: number;
    min_healthy_nodes: number;
    max_output_tokens: number;
  };
  nodes?: Record<string, QualityGuardNodeState>;
  protectedNodeIds?: string[];
  recentEvents?: QualityGuardEvent[];
  statistics?: QualityGuardStatistics;
};

export type QualityTestResult = {
  nodeId: string;
  statusCode: number;
  firstTokenMs: number;
  durationMs: number;
  outputTokens: number;
  visibleTokens: number;
  outputTokensPerSecond: number;
  expectedMatched: boolean;
};

const nodeStateValidator = hasShape({
  active_soft_strikes: isNumber, passive_soft_strikes: isNumber, error_strikes: isNumber,
  quarantined_until: isNumber, disabled_by_guard: isBoolean, last_reason: isString,
  last_probe_at: isNumber, last_observed_at: isNumber, last_source: isString,
  last_classification: isString, last_output_tps: isNumber, last_output_tokens: isNumber,
  last_first_token_ms: isNumber, last_duration_ms: isNumber,
});
const eventValidator = hasShape({
  ts: isNumber, event: isString, node_id: isString, node_name: isString,
  reason: isString, classification: isString, output_tps: isNumber,
});
const configValidator = hasShape({
  mode: isOneOf("active", "passive", "hybrid"), model: isString,
  node_ids: isArrayOf(isString), active_interval_seconds: isNumber, passive_poll_seconds: isNumber,
  soft_tps: isNumber, hard_tps: isNumber, consecutive_soft: isNumber, consecutive_errors: isNumber,
  quarantine_seconds: isNumber, min_healthy_nodes: isNumber, max_output_tokens: isNumber,
});
const detectionStatsValidator = hasShape({
  total: isNumber, healthy: isNumber, soft: isNumber, hard: isNumber,
  errors: isNumber, output_tokens: isNumber,
});
const statisticsValidator = hasShape({
  started_at: isNumber,
  active: detectionStatsValidator,
  passive: detectionStatsValidator,
  actions: hasShape({ quarantined: isNumber, restored: isNumber, suppressed: isNumber }),
});

const decodeStatus = (value: unknown): QualityGuardStatus => {
  if (hasShape({ available: isBoolean })(value) && (value as QualityGuardStatus).available === false) {
    return value as QualityGuardStatus;
  }
  return createObjectDecoder<QualityGuardStatus>("quality guard", {
    available: isBoolean, editable: isOptional(isBoolean), startedAt: isNumber, updatedAt: isNumber, lastActiveCycleAt: isNumber,
    lastPassivePollAt: isNumber, config: configValidator, nodes: isRecordOf(nodeStateValidator),
    protectedNodeIds: isOptional(isArrayOf(isString)),
    recentEvents: isArrayOf(eventValidator), statistics: isOptional(statisticsValidator),
  })(value);
};

const decodeQualityTest = createObjectDecoder<QualityTestResult>("quality test", {
  nodeId: isString, statusCode: isNumber, firstTokenMs: isNumber, durationMs: isNumber,
  outputTokens: isNumber, visibleTokens: isNumber, outputTokensPerSecond: isNumber, expectedMatched: isBoolean,
});

export function getQualityGuardStatus(): Promise<QualityGuardStatus> {
  return apiRequest("/api/admin/v1/egress-quality-guard", {}, decodeStatus);
}

export function runQualityTest(nodeId: string, status: QualityGuardStatus): Promise<QualityTestResult> {
  if (!status.config) throw new Error("Quality guard configuration is unavailable");
  return apiRequest(`/api/admin/v1/egress-quality-guard/nodes/${nodeId}/test`, { method: "POST" }, decodeQualityTest);
}

export function updateQualityGuardPolicy(policy: QualityGuardPolicy): Promise<{ saved: boolean }> {
  return apiRequest("/api/admin/v1/egress-quality-guard/config", { method: "PUT", body: policy }, createObjectDecoder("quality guard config update", { saved: isBoolean }));
}
