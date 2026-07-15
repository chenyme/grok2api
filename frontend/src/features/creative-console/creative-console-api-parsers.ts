export type ChatMessage = {
  role: "system" | "user" | "assistant";
  content: string;
};

export type ImageResult = {
  url: string;
  revisedPrompt?: string;
};

export type VideoStatus = {
  status: "pending" | "done" | "failed";
  model?: string;
  progress: number;
  video?: {
    url: string;
    duration?: number;
    respectModeration?: boolean;
  };
  error?: {
    code?: string;
    message: string;
  };
};

export class CreativeApiError extends Error {
  readonly status: number;
  readonly code?: string;

  constructor(status: number, message: string, code?: string) {
    super(message);
    this.name = "CreativeApiError";
    this.status = status;
    this.code = code;
  }
}

export function createPublicApiBaseURL(publicApiBaseURL: string): string {
  const normalized = publicApiBaseURL.trim().replace(/\/+$/, "");
  if (!normalized) return "";
  return normalized.toLowerCase().endsWith("/v1") ? normalized : `${normalized}/v1`;
}

export function readVideoRequestID(payload: unknown): string {
  if (!isRecord(payload) || typeof payload.request_id !== "string") return "";
  return payload.request_id.trim();
}

export function readChatText(payload: unknown): string {
  if (!isRecord(payload) || !Array.isArray(payload.choices)) return "";
  for (const choice of payload.choices) {
    if (!isRecord(choice) || !isRecord(choice.message)) continue;
    const text = readContentText(choice.message.content);
    if (text) return text;
  }
  return "";
}

function readContentText(content: unknown): string {
  if (typeof content === "string") return content.trim();
  if (!Array.isArray(content)) return "";
  return content
    .map((item) => {
      if (typeof item === "string") return item;
      if (!isRecord(item)) return "";
      return typeof item.text === "string" ? item.text : typeof item.content === "string" ? item.content : "";
    })
    .filter(Boolean)
    .join("\n")
    .trim();
}

export function readImages(payload: unknown): ImageResult[] {
  if (!isRecord(payload) || !Array.isArray(payload.data)) return [];
  return payload.data.flatMap((item) => {
    if (!isRecord(item)) return [];
    const url = typeof item.url === "string" && item.url.trim()
      ? item.url
      : typeof item.b64_json === "string" && item.b64_json.trim()
        ? `data:image/png;base64,${item.b64_json}`
        : "";
    if (!url) return [];
    return [{ url, revisedPrompt: typeof item.revised_prompt === "string" ? item.revised_prompt : undefined }];
  });
}

export function readVideoStatus(payload: unknown): VideoStatus {
  if (!isRecord(payload) || !isVideoStatus(payload.status)) {
    throw new CreativeApiError(200, "The video status response was invalid", "invalid_response");
  }
  const progress = typeof payload.progress === "number" && Number.isFinite(payload.progress)
    ? Math.max(0, Math.min(100, payload.progress))
    : payload.status === "done" ? 100 : 0;
  const result: VideoStatus = {
    status: payload.status,
    model: typeof payload.model === "string" ? payload.model : undefined,
    progress,
  };
  if (isRecord(payload.video) && typeof payload.video.url === "string") {
    result.video = {
      url: payload.video.url,
      duration: typeof payload.video.duration === "number" ? payload.video.duration : undefined,
      respectModeration: typeof payload.video.respect_moderation === "boolean" ? payload.video.respect_moderation : undefined,
    };
  }
  if (isRecord(payload.error) && typeof payload.error.message === "string") {
    result.error = {
      code: typeof payload.error.code === "string" ? payload.error.code : undefined,
      message: payload.error.message,
    };
  }
  return result;
}

export function readError(payload: unknown): { code?: string; message?: string } {
  if (!isRecord(payload)) return {};
  if (isRecord(payload.error)) {
    return {
      code: typeof payload.error.code === "string" ? payload.error.code : undefined,
      message: typeof payload.error.message === "string" ? payload.error.message : undefined,
    };
  }
  return {
    code: typeof payload.code === "string" ? payload.code : undefined,
    message: typeof payload.message === "string" ? payload.message : undefined,
  };
}

function isVideoStatus(value: unknown): value is VideoStatus["status"] {
  return value === "pending" || value === "done" || value === "failed";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export const creativeApiInternals = { createPublicApiBaseURL, readChatText, readImages, readVideoRequestID, readVideoStatus, readError };
