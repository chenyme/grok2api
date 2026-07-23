import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, decodeBooleanResult, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isRecordOf, isString } from "@/shared/api/decoder";
import type { EgressScope } from "@/entities/egress";

// Egress 类型和 API 已提升到 entities/egress，此处保留 re-export 以兼容 settings 内部引用。
export { assignEgressAccounts, createEgressNode, deleteEgressNode, deleteEgressNodes, listEgressNodes, refreshEgressClearance, unassignEgressAccounts, updateEgressNode } from "@/entities/egress";
export type { EgressNodeDTO, EgressNodeInput, EgressNodeListDTO, EgressScope } from "@/entities/egress";

export type SettingsConfigDTO = {
  server: { maxConcurrentRequests: number };
  providerBuild: { baseURL: string; fallbackBaseURL: string; clientVersion: string; clientIdentifier: string; tokenAuth: string; tokenAuthConfigured: boolean; userAgent: string; responseHeaderTimeout: string };
  providerWeb: {
    baseURL: string; quotaTimeout: string; chatTimeout: string; imageTimeout: string; videoTimeout: string;
    statsigMode: "manual" | "url"; statsigManualValue?: string; statsigManualConfigured: boolean; statsigSignerURL: string;
    clearanceMode: "manual" | "flaresolverr"; flareSolverrURL: string; clearanceTimeout: string; clearanceRefresh: string;
    mediaConcurrency: number; allowNSFW: boolean;
    recoveryBackoffBase: string; recoveryBackoffMax: string;
  };
  providerConsole: { baseURL: string; chatTimeout: string };
  batch: { importConcurrency: number; conversionConcurrency: number; syncConcurrency: number; refreshConcurrency: number; randomDelay: string };
  media: {
    maxImageBytes: number; maxTotalBytes: number; cleanupThresholdPercent: number;
    cleanupInterval: string;
  };
  frontend: { publicApiBaseURL: string };
  routing: {
    stickyTTL: string; cooldownBase: string; cooldownMax: string; capacityWait: string; maxAttempts: number; preferFreeBuild: boolean;
    segmentedSelector: { enabled: boolean; minCandidates: number; windowSize: number };
  };
  audit: { bufferSize: number; batchSize: number; flushInterval: string; commitDelayMS: number };
  clientKeyDefaults: { rpmLimit: number; maxConcurrent: number };
  accounts: {
    autoCleanReauthEnabled: boolean;
    autoCleanReauthInterval: string;
    autoCleanReauthMinAge: string;
    autoCleanIncludeDisabled: boolean;
  };
};

export type EgressFallbackMode = "none" | "direct" | "fixed";
export type EgressFallbackConfigDTO = { mode: EgressFallbackMode; nodeId?: string };
export type EgressSourceDTO = {
  id: string; name: string; scope: EgressScope; enabled: boolean; urlConfigured: boolean;
  refreshIntervalSeconds: number; defaultAccountCapacity: number;
  lastSyncedAt?: string; nextSyncAt?: string; lastSyncImported: number; lastSyncError?: string;
};
export type EgressSourceInput = {
  name: string; scope: EgressScope; enabled: boolean; url?: string; clearUrl?: boolean;
  refreshIntervalSeconds: number; defaultAccountCapacity: number;
};
export type EgressOperationsConfigDTO = {
  probeIntervalSeconds: number; autoAssignEnabled: boolean; autoBalanceEnabled: boolean;
  assignmentIntervalSeconds: number; fallbacks: Record<EgressScope, EgressFallbackConfigDTO>; updatedAt: string;
};
export type EgressImportResultDTO = { imported: number; skipped: number };
export type EgressProbeResultDTO = { status: "unknown" | "healthy" | "unhealthy"; testedAt: string; latencyMs: number; exitIp?: string; error?: string };
export type EgressProbeBatchResultDTO = { requested: number; healthy: number; unhealthy: number };
export type EgressRebalanceResultDTO = { assigned: number; rebalanced: number; unplaced: number };

export type SettingsSnapshotDTO = {
  config: SettingsConfigDTO;
  recommendedProviderBuild: { clientVersion: string; userAgent: string };
  updatedAt: string;
  revision: string;
  restartRequired: string[];
};

const settingsConfigValidator = hasShape({
  server: hasShape({ maxConcurrentRequests: isNumber }),
  providerBuild: hasShape({ baseURL: isString, fallbackBaseURL: isString, clientVersion: isString, clientIdentifier: isString, tokenAuth: isString, tokenAuthConfigured: isBoolean, userAgent: isString, responseHeaderTimeout: isString }),
  providerWeb: hasShape({
    baseURL: isString, quotaTimeout: isString, chatTimeout: isString, imageTimeout: isString, videoTimeout: isString,
    statsigMode: isOneOf("manual", "url"), statsigManualValue: isOptional(isString), statsigManualConfigured: isBoolean,
    statsigSignerURL: isString, clearanceMode: isOneOf("manual", "flaresolverr"), flareSolverrURL: isString,
    clearanceTimeout: isString, clearanceRefresh: isString, mediaConcurrency: isNumber, allowNSFW: isBoolean, recoveryBackoffBase: isString, recoveryBackoffMax: isString,
  }),
  providerConsole: hasShape({ baseURL: isString, chatTimeout: isString }),
  batch: hasShape({ importConcurrency: isNumber, conversionConcurrency: isNumber, syncConcurrency: isNumber, refreshConcurrency: isNumber, randomDelay: isString }),
  media: hasShape({ maxImageBytes: isNumber, maxTotalBytes: isNumber, cleanupThresholdPercent: isNumber, cleanupInterval: isString }),
  frontend: hasShape({ publicApiBaseURL: isString }),
  routing: hasShape({
    stickyTTL: isString, cooldownBase: isString, cooldownMax: isString, capacityWait: isString, maxAttempts: isNumber, preferFreeBuild: isBoolean,
    segmentedSelector: isOptional(hasShape({ enabled: isBoolean, minCandidates: isNumber, windowSize: isNumber })),
  }),
  audit: hasShape({ bufferSize: isNumber, batchSize: isNumber, flushInterval: isString, commitDelayMS: isOptional(isNumber) }),
  clientKeyDefaults: hasShape({ rpmLimit: isNumber, maxConcurrent: isNumber }),
  // 旧后端可无 accounts；decode 后由 withAccountsDefaults 补默认关闭策略。
  accounts: isOptional(hasShape({
    autoCleanReauthEnabled: isBoolean,
    autoCleanReauthInterval: isString,
    autoCleanReauthMinAge: isString,
    autoCleanIncludeDisabled: isBoolean,
  })),
});
const defaultAccountsConfig = (): SettingsConfigDTO["accounts"] => ({
  autoCleanReauthEnabled: false,
  autoCleanReauthInterval: "10m",
  autoCleanReauthMinAge: "1h",
  autoCleanIncludeDisabled: false,
});
function withSettingsDefaults(snapshot: SettingsSnapshotDTO): SettingsSnapshotDTO {
  const accounts = snapshot.config.accounts ?? defaultAccountsConfig();
  const segmentedSelector = snapshot.config.routing.segmentedSelector ?? { enabled: false, minCandidates: 3000, windowSize: 64 };
  return {
    ...snapshot,
    config: {
      ...snapshot.config,
      audit: {
        ...snapshot.config.audit,
        commitDelayMS: snapshot.config.audit.commitDelayMS ?? 5,
      },
      routing: {
        ...snapshot.config.routing,
        segmentedSelector: {
          enabled: segmentedSelector.enabled ?? false,
          minCandidates: segmentedSelector.minCandidates || 3000,
          windowSize: segmentedSelector.windowSize || 64,
        },
      },
      accounts: {
        autoCleanReauthEnabled: accounts.autoCleanReauthEnabled ?? false,
        autoCleanReauthInterval: accounts.autoCleanReauthInterval || "10m",
        autoCleanReauthMinAge: accounts.autoCleanReauthMinAge || "1h",
        autoCleanIncludeDisabled: accounts.autoCleanIncludeDisabled ?? false,
      },
    },
  };
}
const decodeSettingsSnapshotRaw = createObjectDecoder<SettingsSnapshotDTO>("settings", {
  config: settingsConfigValidator,
  recommendedProviderBuild: hasShape({ clientVersion: isString, userAgent: isString }),
  updatedAt: isString,
  revision: isString,
  restartRequired: isArrayOf(isString),
});
const decodeSettingsSnapshot = (value: unknown) => withSettingsDefaults(decodeSettingsSnapshotRaw(value));
const egressSourceValidator = hasShape({
  id: isString, name: isString, scope: isOneOf("grok_build", "grok_web", "grok_console", "grok_web_asset"), enabled: isBoolean, urlConfigured: isBoolean,
  refreshIntervalSeconds: isNumber, defaultAccountCapacity: isNumber, lastSyncedAt: isOptional(isString), nextSyncAt: isOptional(isString),
  lastSyncImported: isNumber, lastSyncError: isOptional(isString),
});
const decodeEgressSource = createObjectDecoder<EgressSourceDTO>("egress source", {
  id: isString, name: isString, scope: isOneOf("grok_build", "grok_web", "grok_console", "grok_web_asset"), enabled: isBoolean, urlConfigured: isBoolean,
  refreshIntervalSeconds: isNumber, defaultAccountCapacity: isNumber, lastSyncedAt: isOptional(isString), nextSyncAt: isOptional(isString),
  lastSyncImported: isNumber, lastSyncError: isOptional(isString),
});
const decodeEgressSourceList = createObjectDecoder<{ items: EgressSourceDTO[] }>("egress source list", { items: isArrayOf(egressSourceValidator) });
const decodeEgressImportResult = createObjectDecoder<EgressImportResultDTO>("egress import result", { imported: isNumber, skipped: isNumber });
const decodeEgressProbeBatchResult = createObjectDecoder<EgressProbeBatchResultDTO>("egress probe result", { requested: isNumber, healthy: isNumber, unhealthy: isNumber });
const decodeEgressRebalanceResult = createObjectDecoder<EgressRebalanceResultDTO>("egress rebalance result", { assigned: isNumber, rebalanced: isNumber, unplaced: isNumber });
const egressFallbackConfigValidator = hasShape({ mode: isOneOf("none", "direct", "fixed"), nodeId: isOptional(isString) });
const decodeEgressOperationsConfig = createObjectDecoder<EgressOperationsConfigDTO>("egress operations config", {
  probeIntervalSeconds: isNumber, autoAssignEnabled: isBoolean, autoBalanceEnabled: isBoolean, assignmentIntervalSeconds: isNumber,
  fallbacks: isRecordOf(egressFallbackConfigValidator), updatedAt: isString,
});

export function getSettings(): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", {}, decodeSettingsSnapshot);
}

export function updateSettings(revision: string, config: SettingsConfigDTO): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", { method: "PUT", body: { revision, config } }, decodeSettingsSnapshot);
}

export function testEgressNode(id: string): Promise<EgressProbeResultDTO> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}/test`, { method: "POST" }, createObjectDecoder<EgressProbeResultDTO>("egress probe", { status: isOneOf("unknown", "healthy", "unhealthy"), testedAt: isString, latencyMs: isNumber, exitIp: isOptional(isString), error: isOptional(isString) }));
}

export function testEgressNodes(ids?: string[]): Promise<EgressProbeBatchResultDTO> {
  return apiRequest("/api/admin/v1/egress-nodes/test", { method: "POST", body: { ids: ids ?? [] } }, decodeEgressProbeBatchResult);
}

export function listEgressSources(): Promise<{ items: EgressSourceDTO[] }> {
  return apiRequest("/api/admin/v1/egress-sources", {}, decodeEgressSourceList);
}

export function createEgressSource(input: EgressSourceInput): Promise<EgressSourceDTO> {
  return apiRequest("/api/admin/v1/egress-sources", { method: "POST", body: input }, decodeEgressSource);
}

export function updateEgressSource(id: string, input: EgressSourceInput): Promise<EgressSourceDTO> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}`, { method: "PUT", body: input }, decodeEgressSource);
}

export function deleteEgressSource(id: string): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}`, { method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function syncEgressSource(id: string): Promise<EgressImportResultDTO> {
  return apiRequest(`/api/admin/v1/egress-sources/${id}/sync`, { method: "POST" }, decodeEgressImportResult);
}

export function importEgressText(input: { name: string; scope: EgressScope; accountCapacity: number; content: string }): Promise<EgressImportResultDTO> {
  return apiRequest("/api/admin/v1/egress-imports", { method: "POST", body: input }, decodeEgressImportResult);
}

export function getEgressOperationsConfig(): Promise<EgressOperationsConfigDTO> {
  return apiRequest("/api/admin/v1/egress-operations", {}, decodeEgressOperationsConfig);
}

export function updateEgressOperationsConfig(input: Omit<EgressOperationsConfigDTO, "updatedAt">): Promise<EgressOperationsConfigDTO> {
  return apiRequest("/api/admin/v1/egress-operations", { method: "PUT", body: input }, decodeEgressOperationsConfig);
}

export function rebalanceEgressAccounts(): Promise<EgressRebalanceResultDTO> {
  return apiRequest("/api/admin/v1/egress-operations/rebalance", { method: "POST" }, decodeEgressRebalanceResult);
}
