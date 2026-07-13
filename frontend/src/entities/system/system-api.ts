import { apiRequest } from "@/shared/api/client";

export type SystemInfoDTO = {
  publicApiBaseURL: string;
};

export function getSystemInfo(): Promise<SystemInfoDTO> {
  return apiRequest<SystemInfoDTO>("/api/admin/v1/system");
}
