import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, isString } from "@/shared/api/decoder";

export type SystemInfoDTO = {
  publicApiBaseURL: string;
};

const decodeSystemInfo = createObjectDecoder<SystemInfoDTO>("system info", { publicApiBaseURL: isString });

export function getSystemInfo(): Promise<SystemInfoDTO> {
  return apiRequest("/api/admin/v1/system", {}, decodeSystemInfo);
}
