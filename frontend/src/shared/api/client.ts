import { runtimeConfig } from "@/shared/config/runtime-config";
import { i18n } from "@/shared/i18n";

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;

  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

let accessToken: string | null = null;
let refreshPromise: Promise<RefreshResult> | null = null;
const sessionInvalidatedListeners = new Set<() => void>();
const refreshLockName = "grok2api:admin-session-refresh";
const maxEventStreamBufferCharacters = 1 << 20;
const eventStreamInactivityTimeoutMs = 60_000;

export type RefreshResult = "refreshed" | "invalid" | "unavailable";

export function setAccessToken(token: string | null): void {
  accessToken = token;
}

export function subscribeSessionInvalidated(listener: () => void): () => void {
  sessionInvalidatedListeners.add(listener);
  return () => sessionInvalidatedListeners.delete(listener);
}

function invalidateSession(): void {
  accessToken = null;
  sessionInvalidatedListeners.forEach((listener) => listener());
}

function localizedErrorMessage(code: string, fallback: string): string {
  const key = `apiErrors.${code}`;
  return i18n.exists(key) ? i18n.t(key) : fallback;
}

async function parseResponse<T>(response: Response): Promise<T> {
  const payload: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const error = readErrorEnvelope(payload);
    const code = error.code ?? "requestFailed";
    throw new ApiError(response.status, code, localizedErrorMessage(code, error.message ?? `HTTP ${response.status}`), error.requestId);
  }
  if (!isRecord(payload) || !("data" in payload)) {
    throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
  }
  return payload.data as T;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function readErrorEnvelope(payload: unknown): { code?: string; message?: string; requestId?: string } {
  if (!isRecord(payload) || !isRecord(payload.error)) return {};
  return {
    code: typeof payload.error.code === "string" ? payload.error.code : undefined,
    message: typeof payload.error.message === "string" ? payload.error.message : undefined,
    requestId: typeof payload.error.requestId === "string" ? payload.error.requestId : undefined,
  };
}

async function requestRefresh(): Promise<RefreshResult> {
  try {
    const response = await fetch(`${runtimeConfig.apiBaseUrl}/api/admin/v1/auth/refresh`, {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    if (response.status === 401) {
      invalidateSession();
      return "invalid";
    }
    const tokens = await parseResponse<AuthTokensDTO>(response);
    setAccessToken(tokens.accessToken);
    return "refreshed";
  } catch {
    return "unavailable";
  }
}

async function requestRefreshWithBrowserLock(): Promise<RefreshResult> {
  if (!("locks" in navigator)) {
    return requestRefresh();
  }
  try {
    return await navigator.locks.request(refreshLockName, requestRefresh);
  } catch {
    return "unavailable";
  }
}

export async function refreshAccessToken(): Promise<RefreshResult> {
  if (!refreshPromise) {
    refreshPromise = requestRefreshWithBrowserLock()
      .finally(() => {
        refreshPromise = null;
      });
  }
  return refreshPromise;
}

type RequestOptions = Omit<RequestInit, "body"> & {
  body?: BodyInit | object;
  authenticated?: boolean;
  retryAuth?: boolean;
};

async function sendApiRequest(path: string, options: RequestOptions): Promise<Response> {
  const { authenticated = true, retryAuth, body, headers, ...requestInit } = options;
  void retryAuth;
  const requestHeaders = new Headers(headers);
  let requestBody: BodyInit | undefined;

  if (body instanceof FormData || typeof body === "string" || body instanceof Blob) {
    requestBody = body;
  } else if (body !== undefined) {
    requestHeaders.set("Content-Type", "application/json");
    requestBody = JSON.stringify(body);
  }
  if (authenticated && accessToken) {
    requestHeaders.set("Authorization", `Bearer ${accessToken}`);
  }

  return fetch(`${runtimeConfig.apiBaseUrl}${path}`, {
    ...requestInit,
    body: requestBody,
    credentials: "include",
    headers: requestHeaders,
  });
}

export async function apiRequest<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { authenticated = true, retryAuth = true } = options;
  const response = await sendApiRequest(path, options);

  if (response.status === 401 && authenticated && retryAuth) {
    const refreshResult = await refreshAccessToken();
    if (refreshResult === "refreshed") {
      return apiRequest<T>(path, { ...options, retryAuth: false });
    }
    if (refreshResult === "unavailable") {
      throw new ApiError(503, "sessionRefreshUnavailable", localizedErrorMessage("sessionRefreshUnavailable", "Unable to refresh the session. Please retry."));
    }
  }

  return parseResponse<T>(response);
}

export type ApiStreamEvent<T> = {
  event: string;
  data: T;
};

// apiEventStream 使用现有管理员鉴权发起 POST SSE，并正确处理任意分块边界。
export async function apiEventStream<T>(path: string, options: RequestOptions, onEvent: (value: ApiStreamEvent<T>) => void): Promise<void> {
  const { authenticated = true, retryAuth = true } = options;
  const response = await sendApiRequest(path, options);
  if (response.status === 401 && authenticated && retryAuth) {
    const refreshResult = await refreshAccessToken();
    if (refreshResult === "refreshed") {
      return apiEventStream(path, { ...options, retryAuth: false }, onEvent);
    }
    if (refreshResult === "unavailable") {
      throw new ApiError(503, "sessionRefreshUnavailable", localizedErrorMessage("sessionRefreshUnavailable", "Unable to refresh the session. Please retry."));
    }
  }
  if (!response.ok) {
    await parseResponse<never>(response);
  }
  if (!response.body) {
    throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
  }
  const contentType = response.headers.get("Content-Type")?.toLowerCase() ?? "";
  if (!contentType.startsWith("text/event-stream")) {
    await response.body.cancel().catch(() => undefined);
    throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  const dispatch = (block: string) => {
    let event = "message";
    const data: string[] = [];
    block.split("\n").forEach((line) => {
      const normalized = line.endsWith("\r") ? line.slice(0, -1) : line;
      if (normalized.startsWith("event:")) event = normalized.slice(6).trim();
      if (normalized.startsWith("data:")) data.push(normalized.slice(5).trimStart());
    });
    if (data.length === 0) return;
    let payload: T;
    try {
      payload = JSON.parse(data.join("\n")) as T;
    } catch {
      throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
    }
    onEvent({ event, data: payload });
  };

  try {
    for (;;) {
      const { done, value } = await readEventStreamChunk(reader, response.status);
      buffer += decoder.decode(value, { stream: !done });
      buffer = buffer.replaceAll("\r\n", "\n");
      let boundary = buffer.indexOf("\n\n");
      while (boundary >= 0) {
        if (boundary > maxEventStreamBufferCharacters) {
          throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
        }
        dispatch(buffer.slice(0, boundary));
        buffer = buffer.slice(boundary + 2);
        boundary = buffer.indexOf("\n\n");
      }
      if (buffer.length > maxEventStreamBufferCharacters) {
        throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
      }
      if (done) break;
    }
    if (buffer.trim()) dispatch(buffer);
  } catch (error) {
    await reader.cancel().catch(() => undefined);
    throw error;
  } finally {
    reader.releaseLock();
  }
}

async function readEventStreamChunk(reader: ReadableStreamDefaultReader<Uint8Array>, status: number): Promise<ReadableStreamReadResult<Uint8Array>> {
  let timeout = 0;
  const inactivity = new Promise<never>((_, reject) => {
    timeout = window.setTimeout(() => {
      reject(new ApiError(status, "streamTimeout", localizedErrorMessage("streamTimeout", "The progress stream stopped responding")));
    }, eventStreamInactivityTimeoutMs);
  });
  try {
    return await Promise.race([reader.read(), inactivity]);
  } finally {
    window.clearTimeout(timeout);
  }
}

export async function apiDownload(path: string, retryAuth = true): Promise<Blob> {
  const headers = new Headers();
  if (accessToken) headers.set("Authorization", `Bearer ${accessToken}`);
  const response = await fetch(`${runtimeConfig.apiBaseUrl}${path}`, {
    credentials: "include",
    headers,
  });
  if (response.status === 401 && retryAuth) {
    const refreshResult = await refreshAccessToken();
    if (refreshResult === "refreshed") return apiDownload(path, false);
    if (refreshResult === "unavailable") {
      throw new ApiError(503, "sessionRefreshUnavailable", localizedErrorMessage("sessionRefreshUnavailable", "Unable to refresh the session. Please retry."));
    }
  }
  if (!response.ok) {
    await parseResponse<never>(response);
    throw new ApiError(response.status, "requestFailed", localizedErrorMessage("requestFailed", "The request failed"));
  }
  return response.blob();
}

export type AdminDTO = {
  id: string;
  username: string;
};

export type AuthTokensDTO = {
  accessToken: string;
  accessTokenExpiresAt: string;
  refreshTokenExpiresAt: string;
};

export type LoginResponseDTO = {
  admin: AdminDTO;
  tokens: AuthTokensDTO;
};

export type PaginatedDTO<T> = {
  items: T[];
  page: number;
  pageSize: number;
  total: number;
};
