import { ApiError, apiDownload, apiEventStream, apiRequest, type PaginatedDTO } from "@/shared/api/client";
import { i18n } from "@/shared/i18n";
import type { SortOrder } from "@/shared/lib/table-sort";

export type BillingDTO = {
  planCode?: string;
  planName?: string;
  monthlyLimit: number;
  used: number;
  remaining: number;
  onDemandCap: number;
  onDemandUsed: number;
  prepaidBalance: number;
  creditUsagePercent: number;
  isUnifiedBillingUser: boolean;
  topUpMethod?: string;
  usagePeriodType?: string;
  usagePeriodStart?: string;
  usagePeriodEnd?: string;
  billingPeriodStart?: string;
  billingPeriodEnd?: string;
  syncedAt: string;
};

export type QuotaDTO = {
  type: "free" | "paid" | "unknown";
  source: "unknown" | "upstreamBilling" | "upstreamExhaustion" | "responseModel" | "billingProfile";
  confidence: "estimated" | "observed" | "confirmed" | "";
  status: "active" | "waitingReset" | "probing";
  unit?: "tokens" | "credits";
  used: number;
  limit: number;
  remaining: number;
  usagePercent: number;
  limitKnown: boolean;
  windowHours?: number;
  observed: boolean;
  confirmed: boolean;
  periodStart?: string;
  periodEnd?: string;
  exhaustedAt?: string;
  nextProbeAt?: string;
  lastConfirmedAt?: string;
};

export type AccountDTO = {
  id: string;
  provider: string;
  authType: "oauth" | "sso";
  webTier?: "auto" | "basic" | "super" | "heavy";
  webTierSyncedAt?: string;
  name: string;
  email?: string;
  userId?: string;
  teamId?: string;
  enabled: boolean;
  authStatus: "active" | "reauthRequired";
  expiresAt?: string;
  refreshable: boolean;
  priority: number;
  maxConcurrent: number;
  minimumRemaining: number;
  failureCount: number;
  cooldownUntil?: string;
  lastError?: string;
  lastUsedAt?: string;
  linkedAccountId?: string;
  linkedAccountName?: string;
  linkedProvider?: "grok_build" | "grok_web";
  createdAt: string;
  billing?: BillingDTO;
  quota: QuotaDTO;
  quotaWindows?: Array<{ mode: string; remaining: number; total: number; usagePercent: number; breakdown?: Array<{ productCode: number; usagePercent: number }>; windowSeconds: number; resetAt?: string; syncedAt?: string; source: "default" | "estimated" | "upstream" }>;
};

export type AccountUpdateInput = {
  name: string;
  enabled: boolean;
  priority: number;
  maxConcurrent: number;
  minimumRemaining: number;
};

export type AccountSummaryDTO = {
  total: number;
  available: number;
  recovering: number;
  attention: number;
  providers: Record<"grok_build" | "grok_web", { total: number; available: number }>;
  recovery: { cooldown: number; waitingReset: number; probing: number };
  issues: { disabled: number; reauthRequired: number };
};

export type DeviceSessionDTO = {
  sessionId: string;
  userCode: string;
  verificationUri: string;
  verificationUriComplete?: string;
  intervalSeconds: number;
  expiresAt: string;
};

export type DevicePollDTO = {
  status: "pending" | "succeeded" | "syncFailed";
  account?: AccountDTO;
  synced?: number;
  syncFailed?: number;
};

type ListAccountsInput = {
  page: number;
  pageSize: number;
  search?: string;
  type?: string;
  status?: string;
  renewal?: string;
  provider: "grok_build" | "grok_web";
  sortBy?: string;
  sortOrder?: SortOrder;
};

export function listAccounts(input: ListAccountsInput): Promise<PaginatedDTO<AccountDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.search) query.set("search", input.search);
  if (input.type) query.set("type", input.type);
  if (input.status) query.set("status", input.status);
  if (input.renewal) query.set("renewal", input.renewal);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  query.set("provider", input.provider);
  return apiRequest<PaginatedDTO<AccountDTO>>(`/api/admin/v1/accounts?${query}`);
}

export function getAccountSummary(): Promise<AccountSummaryDTO> {
  return apiRequest<AccountSummaryDTO>("/api/admin/v1/accounts/summary");
}

export function updateAccount(id: string, input: AccountUpdateInput): Promise<AccountDTO> {
  return apiRequest<AccountDTO>(`/api/admin/v1/accounts/${id}`, { method: "PATCH", body: input });
}

export function deleteAccount(id: string): Promise<{ deleted: boolean }> {
  return apiRequest<{ deleted: boolean }>(`/api/admin/v1/accounts/${id}`, { method: "DELETE" });
}

export function refreshAccountBilling(id: string): Promise<AccountDTO> {
  return apiRequest<AccountDTO>(`/api/admin/v1/accounts/${id}/refresh-billing`, { method: "POST" });
}

export function refreshAccountToken(id: string): Promise<AccountDTO> {
  return apiRequest<AccountDTO>(`/api/admin/v1/accounts/${id}/refresh-token`, { method: "POST" });
}

export type AccountBatchResultDTO = { succeeded: number; failed: number };
export type AccountTokenRefreshResultDTO = AccountBatchResultDTO & { skipped: number };

export type BuildConversionResultDTO = {
  created: number;
  linked: number;
  skipped: number;
  failed: number;
  synced: number;
  syncFailed: number;
};

export type BuildConversionInput =
  | { all: true; ids?: never }
  | { all?: false; ids: string[] };

export type AccountTaskProgressDTO = {
  completed: number;
  total: number;
  phase?: "importing" | "converting" | "syncing";
};

export type AccountImportResultDTO = {
  created: number;
  updated: number;
  synced: number;
  syncFailed: number;
};

type AccountTaskStreamPayload = Partial<BuildConversionResultDTO & AccountTaskProgressDTO & AccountTokenRefreshResultDTO & AccountImportResultDTO> & {
  code?: string;
  message?: string;
};

function hasNumericResult(value: AccountTaskStreamPayload, fields: string[]): boolean {
  return fields.every((field) => {
    const item = value[field as keyof AccountTaskStreamPayload];
    return typeof item === "number" && Number.isInteger(item) && item >= 0;
  });
}

async function runAccountTask<T>(path: string, body: BodyInit | object | undefined, resultFields: string[], onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<T> {
  let result: T | undefined;
  let pendingProgress: AccountTaskProgressDTO | undefined;
  let progressTimer: number | undefined;
  let lastProgressAt = 0;
  const flushProgress = () => {
    if (!pendingProgress || !onProgress) return;
    const value = pendingProgress;
    pendingProgress = undefined;
    lastProgressAt = performance.now();
    onProgress(value);
  };
  const reportProgress = (value: AccountTaskProgressDTO) => {
    pendingProgress = value;
    const delay = Math.max(0, 100 - (performance.now() - lastProgressAt));
    if (delay === 0) {
      if (progressTimer !== undefined) window.clearTimeout(progressTimer);
      progressTimer = undefined;
      flushProgress();
    } else if (progressTimer === undefined) {
      progressTimer = window.setTimeout(() => {
        progressTimer = undefined;
        flushProgress();
      }, delay);
    }
  };
  try {
    await apiEventStream<AccountTaskStreamPayload>(path, {
      method: "POST",
      headers: { Accept: "text/event-stream" },
      body,
      signal,
    }, ({ event, data }) => {
      if (event === "progress" && typeof data.completed === "number" && typeof data.total === "number") {
        const phase = data.phase === "importing" || data.phase === "converting" || data.phase === "syncing" ? data.phase : undefined;
        reportProgress({ completed: data.completed, total: data.total, phase });
        return;
      }
      if (event === "complete") {
        flushProgress();
        if (hasNumericResult(data, resultFields)) result = data as T;
        return;
      }
      if (event === "error") {
        const code = data.code ?? "accountConversionFailed";
        throw new ApiError(502, code, i18n.exists(`apiErrors.${code}`) ? i18n.t(`apiErrors.${code}`) : (data.message ?? i18n.t("apiErrors.requestFailed")));
      }
    });
  } finally {
    if (progressTimer !== undefined) window.clearTimeout(progressTimer);
    flushProgress();
  }
  if (!result) {
    throw new ApiError(502, "invalidResponse", i18n.t("apiErrors.invalidResponse"));
  }
  return result;
}

export function refreshAllAccountBilling(onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<AccountBatchResultDTO> {
  return runAccountTask("/api/admin/v1/accounts/refresh-billing", undefined, ["succeeded", "failed"], onProgress, signal);
}

export function refreshAllAccountTokens(onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<AccountTokenRefreshResultDTO> {
  return runAccountTask("/api/admin/v1/accounts/refresh-tokens", undefined, ["succeeded", "failed", "skipped"], onProgress, signal);
}

export function refreshAllWebAccountQuotas(onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<AccountBatchResultDTO> {
  return runAccountTask("/api/admin/v1/accounts/web/refresh-quotas", undefined, ["succeeded", "failed"], onProgress, signal);
}

export function convertWebAccountsToBuild(input: BuildConversionInput, onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<BuildConversionResultDTO> {
  return runAccountTask("/api/admin/v1/accounts/web/convert-to-build", input, ["created", "linked", "skipped", "failed", "synced", "syncFailed"], onProgress, signal);
}

export function importAccounts(file: File, onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<AccountImportResultDTO> {
  const body = new FormData();
  body.append("file", file);
  return runAccountTask("/api/admin/v1/accounts/import", body, ["created", "updated", "synced", "syncFailed"], onProgress, signal);
}

export function importWebAccounts(file: File, onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<AccountImportResultDTO> {
  const body = new FormData();
  body.append("file", file);
  return runAccountTask("/api/admin/v1/accounts/web/import", body, ["created", "updated", "synced", "syncFailed"], onProgress, signal);
}

export function refreshAccountQuota(id: string): Promise<AccountDTO> {
  return apiRequest<AccountDTO>(`/api/admin/v1/accounts/${id}/refresh-quota`, { method: "POST" });
}

export function exportAccounts(): Promise<Blob> {
  return apiDownload("/api/admin/v1/accounts/export");
}

export function updateAccountsEnabled(ids: string[], enabled: boolean, provider: "grok_build" | "grok_web"): Promise<{ updated: number }> {
  return apiRequest<{ updated: number }>("/api/admin/v1/accounts/batch", { method: "PATCH", body: { ids, enabled, provider } });
}

export function refreshAccountsBilling(ids: string[], provider: "grok_build" | "grok_web"): Promise<{ succeeded: number; failed: number }> {
  return apiRequest<{ succeeded: number; failed: number }>("/api/admin/v1/accounts/batch/refresh-billing", { method: "POST", body: { ids, provider } });
}

export function deleteAccounts(ids: string[], provider: "grok_build" | "grok_web"): Promise<{ deleted: number }> {
  return apiRequest<{ deleted: number }>("/api/admin/v1/accounts", { method: "DELETE", body: { ids, provider } });
}

export function startDeviceAuthorization(): Promise<DeviceSessionDTO> {
  return apiRequest<DeviceSessionDTO>("/api/admin/v1/accounts/device/start", { method: "POST" });
}

export function pollDeviceAuthorization(sessionId: string, signal: AbortSignal): Promise<DevicePollDTO> {
  return apiRequest<DevicePollDTO>(`/api/admin/v1/accounts/device/${sessionId}/poll`, { method: "POST", signal });
}
