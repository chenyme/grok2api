import type { ModelRouteDTO, ModelRouteGroupDTO } from "@/entities/model/types";
import { ApiError, apiEventStream, apiRequest, type PaginatedDTO } from "@/shared/api/client";
import { createObjectDecoder, createPaginatedDecoder, decodeBooleanResult, decodeCountResult, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";
import { i18n } from "@/shared/i18n";
import type { SortOrder } from "@/shared/lib/table-sort";

export type ListModelsInput = {
  page: number;
  pageSize: number;
  search?: string;
  status?: string;
  provider?: "grok_build" | "grok_web" | "grok_console" | "";
  providerScope?: Array<"grok_build" | "grok_web" | "grok_console">;
  tierScope?: Array<"free" | "super">;
  activeScope?: boolean;
  sortBy?: string;
  sortOrder?: SortOrder;
};

const modelRouteValidator = hasShape({
  id: isString,
  publicId: isString,
  provider: isOneOf("grok_build", "grok_web", "grok_console"),
  upstreamModel: isString,
  capability: isOneOf("responses", "chat", "image", "image_edit", "video", "tts", "stt", "realtime"),
  origin: isOneOf("catalog", "discovered", "manual"),
  enabled: isBoolean,
  accountIds: isArrayOf(isString),
  bindingMode: isBoolean,
  supportedAccounts: isNumber,
  syncedAccounts: isNumber,
  totalAccounts: isNumber,
  capabilityKnown: isBoolean,
  available: isBoolean,
  lastSyncedAt: isOptional(isString),
});
const decodeModelRoute = createObjectDecoder<ModelRouteDTO>("model route", {
  id: isString, publicId: isString, provider: isOneOf("grok_build", "grok_web", "grok_console"), upstreamModel: isString,
  capability: isOneOf("responses", "chat", "image", "image_edit", "video", "tts", "stt", "realtime"), origin: isOneOf("catalog", "discovered", "manual"),
  enabled: isBoolean, accountIds: isArrayOf(isString), bindingMode: isBoolean, supportedAccounts: isNumber,
  syncedAccounts: isNumber, totalAccounts: isNumber, capabilityKnown: isBoolean, available: isBoolean, lastSyncedAt: isOptional(isString),
});
const decodeModelPage = createPaginatedDecoder<ModelRouteDTO>(modelRouteValidator);
const modelRouteGroupValidator = hasShape({
  key: isString,
  routes: (value) => Array.isArray(value) && value.length > 0 && value.every(modelRouteValidator),
  endpointCapabilities: isArrayOf(isOneOf("completions", "responses", "messages", "image", "image_edit", "video", "tts", "stt", "realtime")),
});
const decodeModelGroupPage = createPaginatedDecoder<ModelRouteGroupDTO>(modelRouteGroupValidator);
const modelAccountValidator = hasShape({ id: isString, name: isString });
const decodeModelAccounts = createObjectDecoder<{ items: ModelAccountOptionDTO[] }>("model accounts", { items: isArrayOf(modelAccountValidator) });

export function listModels(input: ListModelsInput): Promise<PaginatedDTO<ModelRouteDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.search) query.set("search", input.search);
  if (input.status) query.set("status", input.status);
  if (input.provider) query.set("provider", input.provider);
  for (const value of input.providerScope ?? []) query.append("providerScope", value);
  for (const value of input.tierScope ?? []) query.append("tierScope", value);
  if (input.activeScope) query.set("activeScope", "true");
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  return apiRequest(`/api/admin/v1/models?${query}`, {}, decodeModelPage);
}

export function listModelGroups(input: ListModelsInput): Promise<PaginatedDTO<ModelRouteGroupDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.search) query.set("search", input.search);
  if (input.status) query.set("status", input.status);
  if (input.provider) query.set("provider", input.provider);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  return apiRequest(`/api/admin/v1/models/groups?${query}`, {}, decodeModelGroupPage);
}

type ModelSyncEventDTO = {
  synced?: number;
  completed?: number;
  total?: number;
  code?: string;
  message?: string;
};

const decodeModelSyncEvent = createObjectDecoder<ModelSyncEventDTO>("model sync event", {
  synced: isOptional(isNumber),
  completed: isOptional(isNumber),
  total: isOptional(isNumber),
  code: isOptional(isString),
  message: isOptional(isString),
});

export type ModelSyncProgressDTO = { completed: number; total: number };

export async function syncModels(onProgress?: (progress: ModelSyncProgressDTO) => void): Promise<{ synced: number }> {
  let result: { synced: number } | undefined;
  await apiEventStream("/api/admin/v1/models/sync", {
    method: "POST",
    headers: { Accept: "text/event-stream" },
  }, decodeModelSyncEvent, ({ event, data }) => {
    if (event === "progress" && typeof data.completed === "number" && typeof data.total === "number" && data.total > 0) {
      onProgress?.({ completed: Math.min(Math.max(0, data.completed), data.total), total: data.total });
      return;
    }
    if (event === "complete" && typeof data.synced === "number") {
      result = { synced: data.synced };
      return;
    }
    if (event === "error") {
      const code = data.code ?? "modelSyncFailed";
      const message = i18n.exists(`apiErrors.${code}`) ? i18n.t(`apiErrors.${code}`) : (data.message ?? i18n.t("apiErrors.requestFailed"));
      throw new ApiError(502, code, message);
    }
  });
  if (!result) {
    throw new ApiError(502, "invalidResponse", i18n.t("apiErrors.invalidResponse"));
  }
  return result;
}

export type ModelAccountOptionDTO = { id: string; name: string };

export type CreateModelInput = {
  publicId: string;
  provider: ModelRouteDTO["provider"];
  upstreamModel: string;
  capability: ModelRouteDTO["capability"];
  enabled: boolean;
  accountIds: string[];
};

export function listModelAccountOptions(provider: ModelRouteDTO["provider"]): Promise<{ items: ModelAccountOptionDTO[] }> {
  return apiRequest(`/api/admin/v1/models/accounts?provider=${provider}`, {}, decodeModelAccounts);
}

export function createModel(input: CreateModelInput): Promise<ModelRouteDTO> {
  return apiRequest("/api/admin/v1/models", { method: "POST", body: input }, decodeModelRoute);
}

export function updateModel(id: string, input: { publicId: string; enabled: boolean; accountIds: string[] }): Promise<ModelRouteDTO> {
  return apiRequest(`/api/admin/v1/models/${id}`, { method: "PATCH", body: input }, decodeModelRoute);
}

export function deleteModel(id: string): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/models/${id}`, { method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function deleteModels(ids: string[]): Promise<{ deleted: number }> {
  return apiRequest("/api/admin/v1/models", { method: "DELETE", body: { ids } }, decodeCountResult<{ deleted: number }>("deleted"));
}

export function updateModelsEnabled(ids: string[], enabled: boolean): Promise<{ updated: number }> {
  return apiRequest("/api/admin/v1/models/batch", { method: "PATCH", body: { ids, enabled } }, decodeCountResult<{ updated: number }>("updated"));
}
