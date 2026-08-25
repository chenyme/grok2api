import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isBoolean, isNumber, isObject, isOneOf, isOptional, isRecordOf, isString } from "@/shared/api/decoder";
import { qualityGuardStatusPath } from "./quality-guard-query";

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
  observe_only?: boolean;
  observe_only_reason?: string;
  quarantined_lease_count?: number;
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
  account_id?: string;
  request_id?: string;
  reason: string;
  classification: string;
  output_tps: number;
  cooldown_until?: number;
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

export type ProbeProfileSummary = {
  id: string;
  name: string;
  built_in: boolean;
  match_mode: string;
  has_expected: boolean;
  require_thinking: boolean;
};

export type ProbeProfile = {
  id: string;
  name: string;
  built_in: boolean;
  prompt: string;
  expected_text?: string;
  match_mode: string;
  require_thinking: boolean;
  max_output_tokens?: number;
};

export type QualityGuardStatus = {
  available: boolean;
  editable?: boolean;
  startedAt?: number;
  updatedAt?: number;
  lastActiveCycleAt?: number;
  lastPassivePollAt?: number;
  activeProfileId?: string;
  profiles?: ProbeProfileSummary[];
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
    fail_closed: boolean;
    min_generation_ms: number;
  };
  nodes?: Record<string, QualityGuardNodeState>;
  nodeSummary?: { total: number; quarantined: number; quarantinedLeases: number };
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
  reasoningTokens: number;
  visibleTokens: number;
  outputTokensPerSecond: number;
  generationMs: number;
  expectedMatched: boolean;
  thinkingRequired: boolean;
};

const nodeStateValidator = hasShape({
  observe_only: isOptional(isBoolean), observe_only_reason: isOptional(isString), quarantined_lease_count: isOptional(isNumber),
  active_soft_strikes: isNumber, passive_soft_strikes: isNumber, error_strikes: isNumber,
  quarantined_until: isNumber, disabled_by_guard: isBoolean, last_reason: isString,
  last_probe_at: isNumber, last_observed_at: isNumber, last_source: isString,
  last_classification: isString, last_output_tps: isNumber, last_output_tokens: isNumber,
  last_first_token_ms: isNumber, last_duration_ms: isNumber,
});
const eventValidator = hasShape({
  ts: isNumber, event: isString, node_id: isString, node_name: isString,
  account_id: isOptional(isString), request_id: isOptional(isString),
  reason: isString, classification: isString, output_tps: isNumber,
  cooldown_until: isOptional(isNumber),
});
const configValidator = hasShape({
  mode: isOneOf("active", "passive", "hybrid"), model: isString,
  node_ids: isArrayOf(isString), active_interval_seconds: isNumber, passive_poll_seconds: isNumber,
  soft_tps: isNumber, hard_tps: isNumber, consecutive_soft: isNumber, consecutive_errors: isNumber,
  quarantine_seconds: isNumber, min_healthy_nodes: isNumber, max_output_tokens: isNumber,
  fail_closed: isBoolean, min_generation_ms: isNumber,
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
const nodeSummaryValidator = hasShape({
  total: isNumber, quarantined: isNumber, quarantinedLeases: isNumber,
});

const decodeStatus = (value: unknown): QualityGuardStatus => {
  if (hasShape({ available: isBoolean })(value) && (value as QualityGuardStatus).available === false) {
    return value as QualityGuardStatus;
  }
  return createObjectDecoder<QualityGuardStatus>("quality guard", {
    available: isBoolean, editable: isOptional(isBoolean), startedAt: isNumber, updatedAt: isNumber, lastActiveCycleAt: isNumber,
    lastPassivePollAt: isNumber, activeProfileId: isOptional(isString),
    profiles: isOptional(isArrayOf(hasShape({
      id: isString, name: isString, built_in: isBoolean, match_mode: isString, has_expected: isBoolean,
      require_thinking: isBoolean,
    }))),
    config: configValidator, nodes: isRecordOf(nodeStateValidator), nodeSummary: isOptional(nodeSummaryValidator),
    protectedNodeIds: isOptional(isArrayOf(isString)),
    recentEvents: isArrayOf(eventValidator), statistics: isOptional(statisticsValidator),
  })(value);
};

const decodeQualityTest = createObjectDecoder<QualityTestResult>("quality test", {
  nodeId: isString, statusCode: isNumber, firstTokenMs: isNumber, durationMs: isNumber,
  outputTokens: isNumber, reasoningTokens: isNumber, visibleTokens: isNumber,
  outputTokensPerSecond: isNumber, generationMs: isNumber, expectedMatched: isBoolean,
  thinkingRequired: isBoolean,
});

export function getQualityGuardStatus(nodeIds?: string[]): Promise<QualityGuardStatus> {
  return apiRequest(qualityGuardStatusPath(nodeIds), {}, decodeStatus);
}

export function runQualityTest(nodeId: string, status: QualityGuardStatus, profileId?: string): Promise<QualityTestResult> {
  if (!status.config) throw new Error("Quality guard configuration is unavailable");
  return apiRequest(`/api/admin/v1/egress-quality-guard/nodes/${nodeId}/test`, {
    method: "POST",
    body: profileId ? { profileId } : {},
  }, decodeQualityTest);
}

const decodeProfile = createObjectDecoder<ProbeProfile>("probe profile", {
  id: isString, name: isString, built_in: isBoolean, prompt: isString,
  expected_text: isOptional(isString), match_mode: isString, require_thinking: isBoolean,
  max_output_tokens: isOptional(isNumber),
});

export function listProbeProfiles(): Promise<{ activeProfileId: string; items: ProbeProfile[] }> {
  return apiRequest("/api/admin/v1/egress-quality-guard/profiles", {}, createObjectDecoder("probe profiles", {
    activeProfileId: isString,
    items: isArrayOf(hasShape({
      id: isString, name: isString, built_in: isBoolean, prompt: isString,
      expected_text: isOptional(isString), match_mode: isString, require_thinking: isBoolean,
      max_output_tokens: isOptional(isNumber),
    })),
  }));
}

export function createProbeProfile(input: { name: string; prompt: string; expectedText?: string; matchMode: string; requireThinking?: boolean; active?: boolean }): Promise<ProbeProfile> {
  return apiRequest("/api/admin/v1/egress-quality-guard/profiles", { method: "POST", body: input }, decodeProfile);
}

export function updateProbeProfile(id: string, input: { name: string; prompt: string; expectedText?: string; matchMode: string; requireThinking?: boolean; active?: boolean }): Promise<ProbeProfile> {
  return apiRequest(`/api/admin/v1/egress-quality-guard/profiles/${id}`, { method: "PUT", body: input }, decodeProfile);
}

export function deleteProbeProfile(id: string): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/egress-quality-guard/profiles/${id}`, { method: "DELETE" }, createObjectDecoder("delete profile", { deleted: isBoolean }));
}

export function updateQualityGuardPolicy(policy: QualityGuardPolicy): Promise<{ saved: boolean }> {
  return apiRequest("/api/admin/v1/egress-quality-guard/config", { method: "PUT", body: policy }, createObjectDecoder("quality guard config update", { saved: isBoolean }));
}

export type DegradeWindow = "1h" | "6h" | "24h" | "7d";
export type DegradeClass = "buffered_burst" | "soft_tps" | "hard_tps" | "missing_thinking";

export type DegradeAccountDTO = {
  id: string;
  name: string;
  email: string;
  hits: number;
  maxTPS: number;
  classes: Partial<Record<DegradeClass, number>>;
  nodes: string[];
  last: string;
  enabled: boolean;
  found: boolean;
  bfs: number;
};

export type DegradeEventDTO = {
  id: string;
  requestId: string;
  accountId?: string;
  accountName: string;
  nodeName: string;
  outputTokens: number;
  tps: number;
  class: DegradeClass;
  createdAt: string;
  model: string;
};

export type DegradeSummaryDTO = {
  window: DegradeWindow;
  generatedAt: string;
  thresholds: { softTPS: number; hardTPS: number; minGenMs: number; minOutputTokens: number };
  totals: { hits: number; accounts: number; stillEnabled: number; disabled: number; deleted: number; hard: number; soft: number; burst: number; thinking: number; maxTPS: number };
  series: { label: string; count: number; severe: number }[];
  nodes: { name: string; hits: number; accounts: number; maxTPS: number }[];
  accounts: DegradeAccountDTO[];
  accountPage: { page: number; pageSize: number; total: number; hasMore: boolean };
  events: DegradeEventDTO[];
};

const degradeClassValidator = isOneOf("buffered_burst", "soft_tps", "hard_tps", "missing_thinking");

const decodeDegradeSummary = createObjectDecoder<DegradeSummaryDTO>("degrade accounts", {
  window: isOneOf("1h", "6h", "24h", "7d"),
  generatedAt: isString,
  thresholds: hasShape({ softTPS: isNumber, hardTPS: isNumber, minGenMs: isNumber, minOutputTokens: isNumber }),
  totals: hasShape({ hits: isNumber, accounts: isNumber, stillEnabled: isNumber, disabled: isNumber, deleted: isNumber, hard: isNumber, soft: isNumber, burst: isNumber, thinking: isNumber, maxTPS: isNumber }),
  series: isArrayOf(hasShape({ label: isString, count: isNumber, severe: isNumber })),
  nodes: isArrayOf(hasShape({ name: isString, hits: isNumber, accounts: isNumber, maxTPS: isNumber })),
  accounts: isArrayOf(hasShape({
    id: isString, name: isString, email: isString, hits: isNumber, maxTPS: isNumber,
    classes: isObject, nodes: isArrayOf(isString), last: isString, enabled: isBoolean, found: isBoolean, bfs: isNumber,
  })),
  accountPage: hasShape({ page: isNumber, pageSize: isNumber, total: isNumber, hasMore: isBoolean }),
  events: isArrayOf(hasShape({
    id: isString, requestId: isString, accountId: isOptional(isString), accountName: isString,
    nodeName: isString, outputTokens: isNumber, tps: isNumber, class: degradeClassValidator,
    createdAt: isString, model: isString,
  })),
});

export function getDegradeAccounts(input: {
  window: DegradeWindow;
  softTPS?: number;
  hardTPS?: number;
  failClosed?: boolean;
  minGenMs?: number;
  search?: string;
  status?: "enabled" | "disabled" | "deleted";
  class?: DegradeClass;
  minHits?: number;
  page: number;
  pageSize: number;
}): Promise<DegradeSummaryDTO> {
  const query = new URLSearchParams({ window: input.window, page: String(input.page), pageSize: String(input.pageSize) });
  if (input.softTPS) query.set("softTPS", String(input.softTPS));
  if (input.hardTPS) query.set("hardTPS", String(input.hardTPS));
  if (input.failClosed !== undefined) query.set("failClosed", String(input.failClosed));
  if (input.minGenMs) query.set("minGenMs", String(input.minGenMs));
  if (input.search) query.set("search", input.search);
  if (input.status) query.set("status", input.status);
  if (input.class) query.set("class", input.class);
  if (input.minHits && input.minHits > 1) query.set("minHits", String(input.minHits));
  return apiRequest(`/api/admin/v1/request-audits/degrade-accounts?${query}`, {}, decodeDegradeSummary);
}
