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
  video?: { url: string; duration?: number; respectModeration?: boolean };
  error?: { code?: string; message: string };
};

class CreativeApiError extends Error {
  readonly status: number;
  readonly code?: string;

  constructor(status: number, message: string, code?: string) {
    super(message);
    this.name = "CreativeApiError";
    this.status = status;
    this.code = code;
  }
}

type RequestOptions = {
  method?: "GET" | "POST";
  body?: Record<string, unknown>;
  signal?: AbortSignal;
};

export async function createChatCompletion(input: {
  apiKey: string;
  model: string;
  messages: ChatMessage[];
  signal?: AbortSignal;
}): Promise<string> {
  const payload = await publicApiRequest(
    input.apiKey,
    "/chat/completions",
    { method: "POST", body: { model: input.model, messages: input.messages, stream: false }, signal: input.signal },
  );
  const text = readChatText(payload);
  if (!text) throw new CreativeApiError(200, "The chat response did not contain assistant text", "invalid_response");
  return text;
}

export async function generateImage(input: {
  apiKey: string;
  model: string;
  prompt: string;
  count: number;
  aspectRatio: string;
  resolution: string;
  signal?: AbortSignal;
}): Promise<ImageResult[]> {
  const payload = await publicApiRequest(
    input.apiKey,
    "/images/generations",
    {
      method: "POST",
      body: {
        model: input.model,
        prompt: input.prompt,
        n: input.count,
        aspect_ratio: input.aspectRatio,
        resolution: input.resolution,
        response_format: "url",
        stream: false,
      },
      signal: input.signal,
    },
  );
  const images = readImages(payload);
  if (images.length === 0) throw new CreativeApiError(200, "The image response did not contain any images", "invalid_response");
  return images.map((image) => ({ ...image, url: resolveMediaURL(image.url) }));
}

export async function createVideo(input: {
  apiKey: string;
  model: string;
  prompt: string;
  imageURL?: string;
  duration: number;
  aspectRatio: string;
  resolution: string;
  signal?: AbortSignal;
}): Promise<string> {
  const body: Record<string, unknown> = {
    model: input.model,
    prompt: input.prompt,
    duration: input.duration,
    aspect_ratio: input.aspectRatio,
    resolution: input.resolution,
  };
  if (input.imageURL) body.image = { url: input.imageURL };
  const payload = await publicApiRequest(
    input.apiKey,
    "/videos/generations",
    { method: "POST", body, signal: input.signal },
  );
  const requestId = readVideoRequestID(payload);
  if (!requestId) {
    throw new CreativeApiError(200, "The video response did not contain a request ID", "invalid_response");
  }
  return requestId;
}

export async function getVideo(input: {
  apiKey: string;
  requestId: string;
  signal?: AbortSignal;
}): Promise<VideoStatus> {
  const payload = await publicApiRequest(
    input.apiKey,
    `/videos/${encodeURIComponent(input.requestId)}`,
    { method: "GET", signal: input.signal },
  );
  const status = readVideoStatus(payload);
  return status.video ? { ...status, video: { ...status.video, url: resolveMediaURL(status.video.url) } } : status;
}

async function publicApiRequest(apiKey: string, path: string, options: RequestOptions): Promise<unknown> {
  const headers = new Headers({ Accept: "application/json", Authorization: `Bearer ${apiKey}` });
  let body: string | undefined;
  if (options.body) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(options.body);
  }
  const response = await fetch(`/v1${path}`, {
    method: options.method ?? "GET",
    headers,
    body,
    signal: options.signal,
  });
  const responseText = await response.text();
  let payload: unknown = null;
  if (responseText) {
    try {
      payload = JSON.parse(responseText);
    } catch {
      payload = null;
    }
  }
  if (!response.ok) {
    const error = readError(payload);
    const fallback = responseText.trim() || response.statusText || `HTTP ${response.status}`;
    throw new CreativeApiError(response.status, error.message ?? fallback, error.code);
  }
  if (payload === null) throw new CreativeApiError(response.status, "The API returned a non-JSON response", "invalid_response");
  return payload;
}

function resolveMediaURL(value: string): string {
  const url = value.trim();
  if (!url || url.startsWith("data:") || url.startsWith("blob:")) return url;
  try {
    const browserOrigin = typeof window === "undefined" ? "http://localhost" : window.location.origin;
    const resolved = new URL(url, `${browserOrigin}/`);
    if (resolved.pathname.startsWith("/v1/media/images/")) {
      return `${resolved.pathname}${resolved.search}${resolved.hash}`;
    }
    return resolved.origin === browserOrigin ? `${resolved.pathname}${resolved.search}${resolved.hash}` : resolved.toString();
  } catch {
    return url;
  }
}

function readVideoRequestID(payload: unknown): string {
  return isRecord(payload) && typeof payload.request_id === "string" ? payload.request_id.trim() : "";
}

function readChatText(payload: unknown): string {
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
  return content.map((item) => {
    if (typeof item === "string") return item;
    if (!isRecord(item)) return "";
    return typeof item.text === "string" ? item.text : typeof item.content === "string" ? item.content : "";
  }).filter(Boolean).join("\n").trim();
}

function readImages(payload: unknown): ImageResult[] {
  if (!isRecord(payload) || !Array.isArray(payload.data)) return [];
  return payload.data.flatMap((item) => {
    if (!isRecord(item)) return [];
    const url = typeof item.url === "string" && item.url.trim()
      ? item.url
      : typeof item.b64_json === "string" && item.b64_json.trim()
        ? `data:image/png;base64,${item.b64_json}`
        : "";
    return url ? [{ url, revisedPrompt: typeof item.revised_prompt === "string" ? item.revised_prompt : undefined }] : [];
  });
}

function readVideoStatus(payload: unknown): VideoStatus {
  if (!isRecord(payload) || !isVideoStatus(payload.status)) {
    throw new CreativeApiError(200, "The video status response was invalid", "invalid_response");
  }
  const result: VideoStatus = {
    status: payload.status,
    model: typeof payload.model === "string" ? payload.model : undefined,
    progress: typeof payload.progress === "number" && Number.isFinite(payload.progress)
      ? Math.max(0, Math.min(100, payload.progress))
      : payload.status === "done" ? 100 : 0,
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

function readError(payload: unknown): { code?: string; message?: string } {
  if (!isRecord(payload)) return {};
  const error = isRecord(payload.error) ? payload.error : payload;
  return {
    code: typeof error.code === "string" ? error.code : undefined,
    message: typeof error.message === "string" ? error.message : undefined,
  };
}

function isVideoStatus(value: unknown): value is VideoStatus["status"] {
  return value === "pending" || value === "done" || value === "failed";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
