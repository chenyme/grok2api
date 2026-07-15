import {
  CreativeApiError,
  createPublicApiBaseURL as createPublicApiBaseURLFromString,
  creativeApiInternals,
  readChatText,
  readError,
  readImages,
  readVideoRequestID,
  readVideoStatus,
  type ChatMessage,
  type ImageResult,
  type VideoStatus,
} from "./creative-console-api-parsers.ts";

export { CreativeApiError, creativeApiInternals };
export type { ChatMessage, ImageResult, VideoStatus };

type RequestOptions = {
  method?: "GET" | "POST";
  body?: Record<string, unknown>;
  signal?: AbortSignal;
};

export function createPublicApiBaseURL(publicApiBaseURL = browserPublicApiBaseURL()): string {
  return createPublicApiBaseURLFromString(publicApiBaseURL.trim() || browserPublicApiBaseURL());
}

function browserPublicApiBaseURL(): string {
  if (typeof window === "undefined") return "";
  const apiBaseURL = window.__GROK2API_RUNTIME_CONFIG__?.apiBaseUrl?.replace(/\/$/, "") ?? "";
  const publicApiBaseURL = window.__GROK2API_RUNTIME_CONFIG__?.publicApiBaseUrl?.replace(/\/$/, "") ?? "";
  const developmentApiBaseURL = typeof __GROK2API_DEV_API_TARGET__ === "string" ? __GROK2API_DEV_API_TARGET__.replace(/\/$/, "") : "";
  return publicApiBaseURL || apiBaseURL || developmentApiBaseURL || window.location.origin;
}

export async function createChatCompletion(input: {
  publicApiBaseURL: string;
  apiKey: string;
  model: string;
  messages: ChatMessage[];
  signal?: AbortSignal;
}): Promise<string> {
  const payload = await publicApiRequest(
    input.publicApiBaseURL,
    input.apiKey,
    "/chat/completions",
    { method: "POST", body: { model: input.model, messages: input.messages, stream: false }, signal: input.signal },
  );
  const text = readChatText(payload);
  if (!text) throw new CreativeApiError(200, "The chat response did not contain assistant text", "invalid_response");
  return text;
}

export async function generateImage(input: {
  publicApiBaseURL: string;
  apiKey: string;
  model: string;
  prompt: string;
  count: number;
  aspectRatio: string;
  resolution: string;
  signal?: AbortSignal;
}): Promise<ImageResult[]> {
  const payload = await publicApiRequest(
    input.publicApiBaseURL,
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
  return images.map((image) => ({ ...image, url: resolveMediaURL(input.publicApiBaseURL, image.url) }));
}

export async function createVideo(input: {
  publicApiBaseURL: string;
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
    input.publicApiBaseURL,
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
  publicApiBaseURL: string;
  apiKey: string;
  requestId: string;
  signal?: AbortSignal;
}): Promise<VideoStatus> {
  const payload = await publicApiRequest(
    input.publicApiBaseURL,
    input.apiKey,
    `/videos/${encodeURIComponent(input.requestId)}`,
    { method: "GET", signal: input.signal },
  );
  const status = readVideoStatus(payload);
  return status.video ? { ...status, video: { ...status.video, url: resolveMediaURL(input.publicApiBaseURL, status.video.url) } } : status;
}

async function publicApiRequest(publicApiBaseURL: string, apiKey: string, path: string, options: RequestOptions): Promise<unknown> {
  const baseURL = createPublicApiBaseURL(publicApiBaseURL);
  if (!baseURL) throw new CreativeApiError(0, "The public API base URL is not configured", "invalid_base_url");
  const headers = new Headers({ Accept: "application/json", Authorization: `Bearer ${apiKey}` });
  let body: string | undefined;
  if (options.body) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(options.body);
  }
  const response = await fetch(`${baseURL}${path}`, {
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

function resolveMediaURL(publicApiBaseURL: string, value: string): string {
  const url = value.trim();
  if (!url || url.startsWith("data:") || url.startsWith("blob:")) return url;
  try {
    const browserOrigin = typeof window === "undefined" ? undefined : window.location.origin;
    const configuredBaseURL = publicApiBaseURL.trim() || browserPublicApiBaseURL();
    const baseURL = new URL(configuredBaseURL, browserOrigin).toString().replace(/\/+$/, "");
    const relativeBaseURL = url.startsWith("/")
      ? `${new URL(baseURL).origin}/`
      : `${baseURL}/`;
    return new URL(url, relativeBaseURL).toString();
  } catch {
    return url;
  }
}
