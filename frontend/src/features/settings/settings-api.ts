import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, decodeBooleanResult, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";
import type { SortOrder } from "@/shared/lib/table-sort";

export type SettingsConfigDTO = {
  server: { maxConcurrentRequests: number };
  providerBuild: { baseURL: string; fallbackBaseURL: string; clientVersion: string; clientIdentifier: string; tokenAuth: string; tokenAuthConfigured: boolean; userAgent: string };
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
  routing: { stickyTTL: string; cooldownBase: string; cooldownMax: string; capacityWait: string; maxAttempts: number; preferFreeBuild: boolean };
  audit: { bufferSize: number; batchSize: number; flushInterval: string };
  clientKeyDefaults: { rpmLimit: number; maxConcurrent: number };
  accounts: {
    autoCleanReauthEnabled: boolean;
    autoCleanReauthInterval: string;
    autoCleanReauthMinAge: string;
    autoCleanIncludeDisabled: boolean;
  };
};

export type EgressNodeDTO = {
  id: string; name: string; scope: EgressScope; enabled: boolean;
  proxyConfigured: boolean; userAgent: string; cookieConfigured: boolean;
  accountBoundProxy: boolean;
  health: number; failureCount: number; cooldownUntil?: string; lastError?: string;
};

export type EgressNodeInput = {
  name: string; scope: EgressScope; enabled: boolean; proxyURL?: string;
  clearProxyURL?: boolean; userAgent: string; cloudflareCookies?: string; clearCookies?: boolean;
};

export type EgressScope = "grok_build" | "grok_web" | "grok_console" | "grok_web_asset";
export type EgressNodeListDTO = { items: EgressNodeDTO[]; defaultUserAgents: Record<EgressScope, string> };

export type SettingsSnapshotDTO = {
  config: SettingsConfigDTO;
  recommendedProviderBuild: { clientVersion: string; userAgent: string };
  updatedAt: string;
  revision: string;
  restartRequired: string[];
};

const settingsConfigValidator = hasShape({
  server: hasShape({ maxConcurrentRequests: isNumber }),
  providerBuild: hasShape({ baseURL: isString, fallbackBaseURL: isString, clientVersion: isString, clientIdentifier: isString, tokenAuth: isString, tokenAuthConfigured: isBoolean, userAgent: isString }),
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
  routing: hasShape({ stickyTTL: isString, cooldownBase: isString, cooldownMax: isString, capacityWait: isString, maxAttempts: isNumber, preferFreeBuild: isBoolean }),
  audit: hasShape({ bufferSize: isNumber, batchSize: isNumber, flushInterval: isString }),
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
function withAccountsDefaults(snapshot: SettingsSnapshotDTO): SettingsSnapshotDTO {
  const accounts = snapshot.config.accounts ?? defaultAccountsConfig();
  return {
    ...snapshot,
    config: {
      ...snapshot.config,
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
const decodeSettingsSnapshot = (value: unknown) => withAccountsDefaults(decodeSettingsSnapshotRaw(value));
const egressNodeValidator = hasShape({
  id: isString, name: isString, scope: isOneOf("grok_build", "grok_web", "grok_console", "grok_web_asset"), enabled: isBoolean,
  proxyConfigured: isBoolean, userAgent: isString, cookieConfigured: isBoolean, accountBoundProxy: isBoolean, health: isNumber, failureCount: isNumber,
  cooldownUntil: isOptional(isString), lastError: isOptional(isString),
});
const decodeEgressNode = createObjectDecoder<EgressNodeDTO>("egress node", {
  id: isString, name: isString, scope: isOneOf("grok_build", "grok_web", "grok_console", "grok_web_asset"), enabled: isBoolean,
  proxyConfigured: isBoolean, userAgent: isString, cookieConfigured: isBoolean, accountBoundProxy: isBoolean, health: isNumber, failureCount: isNumber,
  cooldownUntil: isOptional(isString), lastError: isOptional(isString),
});
const decodeEgressNodeList = createObjectDecoder<EgressNodeListDTO>("egress node list", {
  items: isArrayOf(egressNodeValidator),
  defaultUserAgents: hasShape({ grok_build: isString, grok_web: isString, grok_console: isString, grok_web_asset: isString }),
});

export function getSettings(): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", {}, decodeSettingsSnapshot);
}

export function updateSettings(revision: string, config: SettingsConfigDTO): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", { method: "PUT", body: { revision, config } }, decodeSettingsSnapshot);
}

export function listEgressNodes(input?: { sortBy?: string; sortOrder?: SortOrder }): Promise<EgressNodeListDTO> {
  const query = new URLSearchParams();
  if (input?.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  const suffix = query.size > 0 ? `?${query}` : "";
  return apiRequest(`/api/admin/v1/egress-nodes${suffix}`, {}, decodeEgressNodeList);
}

export function createEgressNode(input: EgressNodeInput): Promise<EgressNodeDTO> {
  return apiRequest("/api/admin/v1/egress-nodes", { method: "POST", body: input }, decodeEgressNode);
}

export function updateEgressNode(id: string, input: EgressNodeInput): Promise<EgressNodeDTO> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}`, { method: "PUT", body: input }, decodeEgressNode);
}

export function deleteEgressNode(id: string): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}`, { method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function refreshEgressClearance(id: string): Promise<{ refreshed: boolean }> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}/refresh-clearance`, { method: "POST" }, decodeBooleanResult<{ refreshed: boolean }>("refreshed"));
}
