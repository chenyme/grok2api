import type { MediaAssetDTO, ImageStatsDTO, MediaJobDTO, VideoStatsDTO } from "@/features/media/types";
import { apiRequest, type PaginatedDTO } from "@/shared/api/client";
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

export function listImages(input: ListImagesInput): Promise<PaginatedDTO<MediaAssetDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.search) query.set("search", input.search);
  return apiRequest<PaginatedDTO<MediaAssetDTO>>(`/api/admin/v1/media/images?${query}`);
}

export function getImageStats(): Promise<ImageStatsDTO> {
  return apiRequest<ImageStatsDTO>("/api/admin/v1/media/images/stats");
}

export function listVideos(input: ListVideosInput): Promise<PaginatedDTO<MediaJobDTO>> {
  const query = new URLSearchParams({ page: String(input.page), pageSize: String(input.pageSize) });
  if (input.status) query.set("status", input.status);
  if (input.search) query.set("search", input.search);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  return apiRequest<PaginatedDTO<MediaJobDTO>>(`/api/admin/v1/media/videos?${query}`);
}

export function getVideoStats(): Promise<VideoStatsDTO> {
  return apiRequest<VideoStatsDTO>("/api/admin/v1/media/videos/stats");
}
