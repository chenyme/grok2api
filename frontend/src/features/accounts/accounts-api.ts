import { ApiError, apiDownload, apiEventStream, apiRequest, type PaginatedDTO } from "@/shared/api/client";
import { createObjectDecoder, createPaginatedDecoder, createValidatedDecoder, decodeBooleanResult, decodeCountResult, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isRecordOf, isString } from "@/shared/api/decoder";
import { i18n } from "@/shared/i18n";
import type { SortOrder } from "@/shared/lib/table-sort";

export type AccountProvider = "grok_build" | "grok_web" | "grok_console";

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
  history?: BillingHistoryDTO[];
  syncedAt: string;
};

export type BillingHistoryDTO = {
  year: number;
  month: number;
  includedUsed: number;
  onDemandUsed: number;
  totalUsed: number;
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
  provider: AccountProvider;
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
  refreshDueAt?: string;
  lastRefreshAt?: string;
  refreshFailureCount: number;
  lastRefreshErrorCode?: string;
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
  providers: Record<AccountProvider, { total: number; available: number }>;
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

const billingHistoryValidator = hasShape({
  year: isNumber, month: isNumber, includedUsed: isNumber, onDemandUsed: isNumber, totalUsed: isNumber,
});
const billingValidator = hasShape({
  planCode: isOptional(isString), planName: isOptional(isString), monthlyLimit: isNumber, used: isNumber, remaining: isNumber,
  onDemandCap: isNumber, onDemandUsed: isNumber, prepaidBalance: isNumber, creditUsagePercent: isNumber,
  isUnifiedBillingUser: isBoolean, topUpMethod: isOptional(isString), usagePeriodType: isOptional(isString),
  usagePeriodStart: isOptional(isString), usagePeriodEnd: isOptional(isString), billingPeriodStart: isOptional(isString),
  billingPeriodEnd: isOptional(isString), history: isOptional(isArrayOf(billingHistoryValidator)), syncedAt: isString,
});
const quotaValidator = hasShape({
  type: isOneOf("free", "paid", "unknown"), source: isOneOf("unknown", "upstreamBilling", "upstreamExhaustion", "responseModel", "billingProfile"),
  confidence: isOneOf("estimated", "observed", "confirmed", ""), status: isOneOf("active", "waitingReset", "probing"),
  unit: isOptional(isOneOf("tokens", "credits")), used: isNumber, limit: isNumber, remaining: isNumber, usagePercent: isNumber,
  limitKnown: isBoolean, windowHours: isOptional(isNumber), observed: isBoolean, confirmed: isBoolean,
  periodStart: isOptional(isString), periodEnd: isOptional(isString), exhaustedAt: isOptional(isString),
  nextProbeAt: isOptional(isString), lastConfirmedAt: isOptional(isString),
});
const quotaBreakdownValidator = hasShape({ productCode: isNumber, usagePercent: isNumber });
const quotaWindowValidator = hasShape({
  mode: isString, remaining: isNumber, total: isNumber, usagePercent: isNumber, breakdown: isOptional(isArrayOf(quotaBreakdownValidator)),
  windowSeconds: isNumber, resetAt: isOptional(isString), syncedAt: isOptional(isString), source: isOneOf("default", "estimated", "upstream"),
});
const accountValidator = hasShape({
  id: isString, provider: isOneOf("grok_build", "grok_web", "grok_console"), authType: isOneOf("oauth", "sso"), webTier: isOptional(isOneOf("auto", "basic", "super", "heavy")),
  webTierSyncedAt: isOptional(isString), name: isString, email: isOptional(isString), userId: isOptional(isString), teamId: isOptional(isString),
  enabled: isBoolean, authStatus: isOneOf("active", "reauthRequired"), expiresAt: isOptional(isString), refreshable: isBoolean,
  refreshDueAt: isOptional(isString), lastRefreshAt: isOptional(isString), refreshFailureCount: isNumber,
  lastRefreshErrorCode: isOptional(isString), priority: isNumber, maxConcurrent: isNumber, minimumRemaining: isNumber,
  failureCount: isNumber, cooldownUntil: isOptional(isString), lastError: isOptional(isString), lastUsedAt: isOptional(isString),
  linkedAccountId: isOptional(isString), linkedAccountName: isOptional(isString), linkedProvider: isOptional(isOneOf("grok_build", "grok_web")),
  createdAt: isString, billing: isOptional(billingValidator), quota: quotaValidator, quotaWindows: isOptional(isArrayOf(quotaWindowValidator)),
});
const decodeBilling = createValidatedDecoder<BillingDTO>("billing", billingValidator);
const decodeAccount = createValidatedDecoder<AccountDTO>("account", accountValidator);
const decodeAccountPage = createPaginatedDecoder<AccountDTO>(accountValidator);
const decodeAccountSummary = createObjectDecoder<AccountSummaryDTO>("account summary", {
  total: isNumber, available: isNumber, recovering: isNumber, attention: isNumber,
  providers: isRecordOf(hasShape({ total: isNumber, available: isNumber })),
  recovery: hasShape({ cooldown: isNumber, waitingReset: isNumber, probing: isNumber }),
  issues: hasShape({ disabled: isNumber, reauthRequired: isNumber }),
});
const decodeDeviceSession = createObjectDecoder<DeviceSessionDTO>("device session", {
  sessionId: isString, userCode: isString, verificationUri: isString, verificationUriComplete: isOptional(isString),
  intervalSeconds: isNumber, expiresAt: isString,
});
const decodeDevicePoll = createObjectDecoder<DevicePollDTO>("device poll", {
  status: isOneOf("pending", "succeeded", "syncFailed"), account: isOptional(accountValidator), synced: isOptional(isNumber), syncFailed: isOptional(isNumber),
});

type ListAccountsInput = {
  page: number;
  pageSize: number;
  search?: string;
  type?: string;
  status?: string;
  renewal?: string;
  provider: AccountProvider;
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
  return apiRequest(`/api/admin/v1/accounts?${query}`, {}, decodeAccountPage);
}

export function getAccountSummary(): Promise<AccountSummaryDTO> {
  return apiRequest("/api/admin/v1/accounts/summary", {}, decodeAccountSummary);
}

export function updateAccount(id: string, input: AccountUpdateInput): Promise<AccountDTO> {
  return apiRequest(`/api/admin/v1/accounts/${id}`, { method: "PATCH", body: input }, decodeAccount);
}

export function deleteAccount(id: string): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/accounts/${id}`, { method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function refreshAccountBilling(id: string): Promise<BillingDTO> {
  return apiRequest(`/api/admin/v1/accounts/${id}/refresh-billing`, { method: "POST" }, decodeBilling);
}

export function refreshAccountToken(id: string): Promise<AccountDTO> {
  return apiRequest(`/api/admin/v1/accounts/${id}/refresh-token`, { method: "POST" }, decodeAccount);
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

export type AccountSyncStrategy = "missing" | "all";
export type BuildConversionStrategy = AccountSyncStrategy;
export type WebConsoleSyncStrategy = AccountSyncStrategy;

export type BuildConversionInput =
  | { all: true; ids?: never; strategy?: BuildConversionStrategy }
  | { all?: false; ids: string[]; strategy?: BuildConversionStrategy };

export type WebConsoleSyncInput =
  | { all: true; ids?: never; strategy: WebConsoleSyncStrategy }
  | { all?: false; ids: string[]; strategy: WebConsoleSyncStrategy };

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

export type WebConsoleSyncResultDTO = AccountImportResultDTO & { skipped: number };

type AccountTaskStreamPayload = Partial<BuildConversionResultDTO & AccountTaskProgressDTO & AccountTokenRefreshResultDTO & AccountImportResultDTO> & {
  code?: string;
  message?: string;
};

const decodeAccountTaskStreamPayload = createObjectDecoder<AccountTaskStreamPayload>("account task event", {
  created: isOptional(isNumber), linked: isOptional(isNumber), skipped: isOptional(isNumber), failed: isOptional(isNumber),
  synced: isOptional(isNumber), syncFailed: isOptional(isNumber), completed: isOptional(isNumber), total: isOptional(isNumber),
  phase: isOptional(isOneOf("importing", "converting", "syncing")), updated: isOptional(isNumber), succeeded: isOptional(isNumber),
  code: isOptional(isString), message: isOptional(isString),
});

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
    if (pendingProgress && pendingProgress.phase !== value.phase && pendingProgress.completed === pendingProgress.total) {
      if (progressTimer !== undefined) window.clearTimeout(progressTimer);
      progressTimer = undefined;
      flushProgress();
    }
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
    await apiEventStream(path, {
      method: "POST",
      headers: { Accept: "text/event-stream" },
      body,
      signal,
    }, decodeAccountTaskStreamPayload, ({ event, data }) => {
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

export function refreshAllConsoleAccountQuotas(onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<AccountBatchResultDTO> {
  return runAccountTask("/api/admin/v1/accounts/console/refresh-quotas", undefined, ["succeeded", "failed"], onProgress, signal);
}

export function convertWebAccountsToBuild(input: BuildConversionInput, onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<BuildConversionResultDTO> {
  return runAccountTask("/api/admin/v1/accounts/web/convert-to-build", input, ["created", "linked", "skipped", "failed", "synced", "syncFailed"], onProgress, signal);
}

export function syncWebAccountsToConsole(input: WebConsoleSyncInput, onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<WebConsoleSyncResultDTO> {
  return runAccountTask("/api/admin/v1/accounts/web/sync-to-console", input, ["created", "updated", "skipped", "synced", "syncFailed"], onProgress, signal);
}

export function importAccounts(files: readonly File[], onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<AccountImportResultDTO> {
  const body = new FormData();
  files.forEach((file) => body.append("files", file, file.name));
  return runAccountTask("/api/admin/v1/accounts/import", body, ["created", "updated", "synced", "syncFailed"], onProgress, signal);
}

export function importWebAccounts(files: readonly File[], onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<AccountImportResultDTO> {
  const body = new FormData();
  files.forEach((file) => body.append("files", file, file.name));
  return runAccountTask("/api/admin/v1/accounts/web/import", body, ["created", "updated", "synced", "syncFailed"], onProgress, signal);
}

export function importConsoleAccounts(files: readonly File[], onProgress?: (value: AccountTaskProgressDTO) => void, signal?: AbortSignal): Promise<AccountImportResultDTO> {
  const body = new FormData();
  files.forEach((file) => body.append("files", file, file.name));
  return runAccountTask("/api/admin/v1/accounts/console/import", body, ["created", "updated", "synced", "syncFailed"], onProgress, signal);
}

export function refreshAccountQuota(id: string): Promise<AccountDTO> {
  return apiRequest(`/api/admin/v1/accounts/${id}/refresh-quota`, { method: "POST" }, decodeAccount);
}

export function exportAccounts(): Promise<Blob> {
  return apiDownload("/api/admin/v1/accounts/export");
}

export function updateAccountsEnabled(ids: string[], enabled: boolean, provider: AccountProvider): Promise<{ updated: number }> {
  return apiRequest("/api/admin/v1/accounts/batch", { method: "PATCH", body: { ids, enabled, provider } }, decodeCountResult<{ updated: number }>("updated"));
}

export function refreshAccountsQuota(ids: string[], provider: AccountProvider): Promise<{ succeeded: number; failed: number }> {
  return apiRequest("/api/admin/v1/accounts/batch/refresh-quotas", { method: "POST", body: { ids, provider } }, createObjectDecoder("account batch", { succeeded: isNumber, failed: isNumber }));
}

export function deleteAccounts(ids: string[], provider: AccountProvider): Promise<{ deleted: number }> {
  return apiRequest("/api/admin/v1/accounts", { method: "DELETE", body: { ids, provider } }, decodeCountResult<{ deleted: number }>("deleted"));
}

export function startDeviceAuthorization(): Promise<DeviceSessionDTO> {
  return apiRequest("/api/admin/v1/accounts/device/start", { method: "POST" }, decodeDeviceSession);
}

export function pollDeviceAuthorization(sessionId: string, signal: AbortSignal): Promise<DevicePollDTO> {
  return apiRequest(`/api/admin/v1/accounts/device/${sessionId}/poll`, { method: "POST", signal }, decodeDevicePoll);
}
