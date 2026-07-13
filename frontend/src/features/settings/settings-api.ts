import { apiRequest } from "@/shared/api/client";
import type { SortOrder } from "@/shared/lib/table-sort";

export type SettingsConfigDTO = {
  providerBuild: { baseURL: string; clientVersion: string; clientIdentifier: string; tokenAuth: string; userAgent: string };
  providerWeb: {
    baseURL: string; quotaTimeout: string; chatTimeout: string; imageTimeout: string; videoTimeout: string;
    statsigMode: "manual" | "url"; statsigManualValue?: string; statsigManualConfigured: boolean; statsigSignerURL: string;
    mediaConcurrency: number; allowNSFW: boolean;
    recoveryBackoffBase: string; recoveryBackoffMax: string;
  };
  batch: { importConcurrency: number; conversionConcurrency: number; syncConcurrency: number; refreshConcurrency: number; randomDelay: string };
  media: {
    maxImageBytes: number; maxTotalBytes: number; cleanupThresholdPercent: number;
    cleanupInterval: string;
  };
  routing: { stickyTTL: string; cooldownBase: string; cooldownMax: string; maxAttempts: number };
  audit: { bufferSize: number; batchSize: number; flushInterval: string };
  clientKeyDefaults: { rpmLimit: number; maxConcurrent: number };
};

export type EgressNodeDTO = {
  id: string; name: string; scope: EgressScope; enabled: boolean;
  proxyConfigured: boolean; userAgent: string; cookieConfigured: boolean;
  health: number; failureCount: number; cooldownUntil?: string; lastError?: string;
};

export type EgressNodeInput = {
  name: string; scope: EgressScope; enabled: boolean; proxyURL?: string;
  clearProxyURL?: boolean; userAgent: string; cloudflareCookies?: string; clearCookies?: boolean;
};

export type EgressScope = "all" | "grok_build" | "grok_web" | "grok_web_asset";
export type EgressNodeListDTO = { items: EgressNodeDTO[]; defaultUserAgents: Record<EgressScope, string> };

export type SettingsSnapshotDTO = {
  config: SettingsConfigDTO;
  updatedAt: string;
  revision: string;
  restartRequired: string[];
};

export function getSettings(): Promise<SettingsSnapshotDTO> {
  return apiRequest<SettingsSnapshotDTO>("/api/admin/v1/settings");
}

export function updateSettings(revision: string, config: SettingsConfigDTO): Promise<SettingsSnapshotDTO> {
  return apiRequest<SettingsSnapshotDTO>("/api/admin/v1/settings", { method: "PUT", body: { revision, config } });
}

export function listEgressNodes(input?: { sortBy?: string; sortOrder?: SortOrder }): Promise<EgressNodeListDTO> {
  const query = new URLSearchParams();
  if (input?.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  const suffix = query.size > 0 ? `?${query}` : "";
  return apiRequest<EgressNodeListDTO>(`/api/admin/v1/egress-nodes${suffix}`);
}

export function createEgressNode(input: EgressNodeInput): Promise<EgressNodeDTO> {
  return apiRequest<EgressNodeDTO>("/api/admin/v1/egress-nodes", { method: "POST", body: input });
}

export function updateEgressNode(id: string, input: EgressNodeInput): Promise<EgressNodeDTO> {
  return apiRequest<EgressNodeDTO>(`/api/admin/v1/egress-nodes/${id}`, { method: "PUT", body: input });
}

export function deleteEgressNode(id: string): Promise<{ deleted: boolean }> {
  return apiRequest<{ deleted: boolean }>(`/api/admin/v1/egress-nodes/${id}`, { method: "DELETE" });
}
