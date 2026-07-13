import { apiRequest, type PaginatedDTO } from "@/shared/api/client";
import type { SortOrder } from "@/shared/lib/table-sort";

export type ClientKeyDTO = {
  id: string;
  name: string;
  prefix: string;
  enabled: boolean;
  expiresAt?: string;
  rpmLimit: number;
  maxConcurrent: number;
  billingLimitUsdTicks: number;
  billedUsageUsdTicks: number;
  allowedModelIds: string[];
  lastUsedAt?: string;
};

export type ClientKeyInput = {
  name: string;
  enabled: boolean;
  expiresAt: string;
  rpmLimit: number;
  maxConcurrent: number;
  billingLimitUsdTicks: number;
  allowedModelIds: string[];
};

export type CreateKeyResponseDTO = { key: ClientKeyDTO; secret: string };

type ListClientKeysInput = {
  page: number;
  pageSize: number;
  search?: string;
  status?: string;
  modelScope?: string;
  sortBy?: string;
  sortOrder?: SortOrder;
};

export function listClientKeys(input: ListClientKeysInput): Promise<PaginatedDTO<ClientKeyDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.search) query.set("search", input.search);
  if (input.status) query.set("status", input.status);
  if (input.modelScope) query.set("modelScope", input.modelScope);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  return apiRequest<PaginatedDTO<ClientKeyDTO>>(`/api/admin/v1/client-keys?${query}`);
}

export function createClientKey(input: ClientKeyInput): Promise<CreateKeyResponseDTO> {
  return apiRequest<CreateKeyResponseDTO>("/api/admin/v1/client-keys", { method: "POST", body: input });
}

export function getClientKeySecret(id: string): Promise<{ secret: string }> {
  return apiRequest<{ secret: string }>(`/api/admin/v1/client-keys/${id}/secret`);
}

export function updateClientKey(id: string, input: ClientKeyInput): Promise<ClientKeyDTO> {
  return apiRequest<ClientKeyDTO>(`/api/admin/v1/client-keys/${id}`, { method: "PATCH", body: input });
}

export function deleteClientKey(id: string): Promise<{ deleted: boolean }> {
  return apiRequest<{ deleted: boolean }>(`/api/admin/v1/client-keys/${id}`, { method: "DELETE" });
}

export function updateClientKeysEnabled(ids: string[], enabled: boolean): Promise<{ updated: number }> {
  return apiRequest<{ updated: number }>("/api/admin/v1/client-keys/batch", { method: "PATCH", body: { ids, enabled } });
}

export function deleteClientKeys(ids: string[]): Promise<{ deleted: number }> {
  return apiRequest<{ deleted: number }>("/api/admin/v1/client-keys", { method: "DELETE", body: { ids } });
}
