export type MediaAssetDTO = {
  id: string;
  kind: string;
  mimeType: string;
  sizeBytes: number;
  sha256: string;
  createdAt: string;
  url: string;
};

export type MediaJobDTO = {
  id: string;
  model: string;
  prompt: string;
  status: "queued" | "in_progress" | "completed" | "failed";
  progress: number;
  seconds: number;
  size: string;
  quality: string;
  accountName: string;
  clientKeyName: string;
  createdAt: string;
  completedAt: string | null;
  errorMessage: string;
};

export type ImageStatsDTO = { totalImages: number; totalBytes: number };
export type VideoStatsDTO = { totalJobs: number; completed: number; failed: number; inProgress: number; queued: number };
