import type { ModelRouteDTO } from "@/entities/model/types";
import { apiRequest, type PaginatedDTO } from "@/shared/api/client";
import { createObjectDecoder, createPaginatedDecoder, decodeBooleanResult, decodeCountResult, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";
import type { SortOrder } from "@/shared/lib/table-sort";

type ListModelsInput = {
  page: number;
  pageSize: number;
  search?: string;
  status?: string;
  provider?: "grok_build" | "grok_web" | "grok_console" | "";
  sortBy?: string;
  sortOrder?: SortOrder;
};

const modelRouteValidator = hasShape({
  id: isString,
  publicId: isString,
  provider: isOneOf("grok_build", "grok_web", "grok_console"),
  upstreamModel: isString,
  capability: isOneOf("responses", "chat", "image", "image_edit", "video"),
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
  capability: isOneOf("responses", "chat", "image", "image_edit", "video"), origin: isOneOf("catalog", "discovered", "manual"),
  enabled: isBoolean, accountIds: isArrayOf(isString), bindingMode: isBoolean, supportedAccounts: isNumber,
  syncedAccounts: isNumber, totalAccounts: isNumber, capabilityKnown: isBoolean, available: isBoolean, lastSyncedAt: isOptional(isString),
});
const decodeModelPage = createPaginatedDecoder<ModelRouteDTO>(modelRouteValidator);
const modelAccountValidator = hasShape({ id: isString, name: isString });
const decodeModelAccounts = createObjectDecoder<{ items: ModelAccountOptionDTO[] }>("model accounts", { items: isArrayOf(modelAccountValidator) });

export function listModels(input: ListModelsInput): Promise<PaginatedDTO<ModelRouteDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.search) query.set("search", input.search);
  if (input.status) query.set("status", input.status);
  if (input.provider) query.set("provider", input.provider);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  return apiRequest(`/api/admin/v1/models?${query}`, {}, decodeModelPage);
}

export function syncModels(): Promise<{ synced: number }> {
  return apiRequest("/api/admin/v1/models/sync", { method: "POST" }, decodeCountResult<{ synced: number }>("synced"));
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
