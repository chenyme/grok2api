import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, isBoolean, isOneOf, isString, type ValueValidator } from "@/shared/api/decoder";

export type SystemInfoDTO = {
  publicApiBaseURL: string;
};

const decodeSystemInfo = createObjectDecoder<SystemInfoDTO>("system info", { publicApiBaseURL: isString });

export function getSystemInfo(): Promise<SystemInfoDTO> {
  return apiRequest("/api/admin/v1/system", {}, decodeSystemInfo);
}

export type UpdateStatus = "unchecked" | "up_to_date" | "update_available" | "check_failed";

export type VersionInfoDTO = {
  currentVersion: string;
  latestVersion: string;
  updateAvailable: boolean;
  status: UpdateStatus;
  checkedAt: string | null;
  releaseUrl: string;
  releaseNotes: string;
  error: string;
};

const isNullableString: ValueValidator = (value) => value === null || isString(value);
const decodeVersionInfo = createObjectDecoder<VersionInfoDTO>("version info", {
  currentVersion: isString,
  latestVersion: isString,
  updateAvailable: isBoolean,
  status: isOneOf("unchecked", "up_to_date", "update_available", "check_failed"),
  checkedAt: isNullableString,
  releaseUrl: isString,
  releaseNotes: isString,
  error: isString,
});

export function getVersionInfo(): Promise<VersionInfoDTO> {
  return apiRequest("/api/admin/v1/system/version", {}, decodeVersionInfo);
}

export function checkForUpdates(): Promise<VersionInfoDTO> {
  return apiRequest("/api/admin/v1/system/update/check", { method: "POST" }, decodeVersionInfo);
}
