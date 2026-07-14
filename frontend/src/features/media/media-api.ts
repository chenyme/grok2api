import type { MediaAssetDTO, ImageStatsDTO, MediaJobDTO, VideoStatsDTO } from "@/features/media/types";
import { apiRequest, type PaginatedDTO } from "@/shared/api/client";
import {
  createObjectDecoder,
  createPaginatedDecoder,
  hasShape,
  isNumber,
  isString,
  isOneOf,
} from "@/shared/api/decoder";
import type { SortOrder } from "@/shared/lib/table-sort";

export type ListImagesInput = {
  page: number;
  pageSize: number;
  search?: string;
};

export type ListVideosInput = {
  page: number;
  pageSize: number;
  status?: MediaJobDTO["status"] | "";
  search?: string;
  sortBy?: string;
  sortOrder?: SortOrder;
};

const mediaAssetShape = {
  id: isString,
  kind: isString,
  mimeType: isString,
  sizeBytes: isNumber,
  sha256: isString,
  createdAt: isString,
  url: isString,
};

const mediaJobShape = {
  id: isString,
  model: isString,
  prompt: isString,
  status: isOneOf("queued", "in_progress", "completed", "failed"),
  progress: isNumber,
  seconds: isNumber,
  size: isString,
  quality: isString,
  accountName: isString,
  clientKeyName: isString,
  createdAt: isString,
  completedAt: (value: unknown) => value === null || isString(value),
  errorMessage: isString,
};

const decodeImageStats = createObjectDecoder<ImageStatsDTO>("image stats", {
  totalImages: isNumber,
  totalBytes: isNumber,
});
const decodeVideoStats = createObjectDecoder<VideoStatsDTO>("video stats", {
  totalJobs: isNumber,
  completed: isNumber,
  failed: isNumber,
  inProgress: isNumber,
  queued: isNumber,
});

export function listImages(input: ListImagesInput): Promise<PaginatedDTO<MediaAssetDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.search) query.set("search", input.search);
  return apiRequest(`/api/admin/v1/media/images?${query}`, {}, createPaginatedDecoder(hasShape(mediaAssetShape)));
}

export function getImageStats(): Promise<ImageStatsDTO> {
  return apiRequest("/api/admin/v1/media/images/stats", {}, decodeImageStats);
}

export function listVideos(input: ListVideosInput): Promise<PaginatedDTO<MediaJobDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.status) query.set("status", input.status);
  if (input.search) query.set("search", input.search);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  return apiRequest(`/api/admin/v1/media/videos?${query}`, {}, createPaginatedDecoder(hasShape(mediaJobShape)));
}

export function getVideoStats(): Promise<VideoStatsDTO> {
  return apiRequest("/api/admin/v1/media/videos/stats", {}, decodeVideoStats);
}
