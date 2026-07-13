import type { ModelRouteDTO } from "@/entities/model/types";
import { apiRequest, type PaginatedDTO } from "@/shared/api/client";
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

export function listModels(input: ListModelsInput): Promise<PaginatedDTO<ModelRouteDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.search) query.set("search", input.search);
  if (input.status) query.set("status", input.status);
  if (input.provider) query.set("provider", input.provider);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  return apiRequest<PaginatedDTO<ModelRouteDTO>>(`/api/admin/v1/models?${query}`);
}

export function syncModels(): Promise<{ synced: number }> {
  return apiRequest<{ synced: number }>("/api/admin/v1/models/sync", { method: "POST" });
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
  return apiRequest<{ items: ModelAccountOptionDTO[] }>(`/api/admin/v1/models/accounts?provider=${provider}`);
}

export function createModel(input: CreateModelInput): Promise<ModelRouteDTO> {
  return apiRequest<ModelRouteDTO>("/api/admin/v1/models", { method: "POST", body: input });
}

export function updateModel(id: string, input: { publicId: string; enabled: boolean; accountIds: string[] }): Promise<ModelRouteDTO> {
  return apiRequest<ModelRouteDTO>(`/api/admin/v1/models/${id}`, { method: "PATCH", body: input });
}

export function deleteModel(id: string): Promise<{ deleted: boolean }> {
  return apiRequest<{ deleted: boolean }>(`/api/admin/v1/models/${id}`, { method: "DELETE" });
}

export function deleteModels(ids: string[]): Promise<{ deleted: number }> {
  return apiRequest<{ deleted: number }>("/api/admin/v1/models", { method: "DELETE", body: { ids } });
}

export function updateModelsEnabled(ids: string[], enabled: boolean): Promise<{ updated: number }> {
  return apiRequest<{ updated: number }>("/api/admin/v1/models/batch", { method: "PATCH", body: { ids, enabled } });
}
