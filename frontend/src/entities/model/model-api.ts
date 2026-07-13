import type { ModelRouteDTO } from "@/entities/model/types";
import { apiRequest, type PaginatedDTO } from "@/shared/api/client";
import type { SortOrder } from "@/shared/lib/table-sort";

type ListModelsInput = {
  page: number;
  pageSize: number;
  search?: string;
  status?: string;
  provider?: "grok_build" | "grok_web" | "";
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

export function updateModel(id: string, input: { publicId: string; enabled: boolean }): Promise<ModelRouteDTO> {
  return apiRequest<ModelRouteDTO>(`/api/admin/v1/models/${id}`, { method: "PATCH", body: input });
}

export function updateModelsEnabled(ids: string[], enabled: boolean): Promise<{ updated: number }> {
  return apiRequest<{ updated: number }>("/api/admin/v1/models/batch", { method: "PATCH", body: { ids, enabled } });
}
