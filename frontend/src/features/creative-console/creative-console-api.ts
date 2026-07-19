export type ChatMessage = {
  role: "system" | "user" | "assistant";
  content: string;
};

export type ReasoningEffort = "auto" | "none" | "low" | "medium" | "high" | "xhigh";

export type ChatToolActivity = {
  id: string;
  type: string;
  name: string;
  status: "in_progress" | "completed" | "failed";
  detail: string;
};

export type ChatStreamSnapshot = {
  text: string;
  reasoning: string;
  tools: ChatToolActivity[];
};

export type ChatResponseResult = ChatStreamSnapshot;

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

export async function createChatResponse(input: {
  apiKey: string;
  model: string;
  messages: ChatMessage[];
  promptCacheKey?: string;
  reasoningEffort: ReasoningEffort;
  webSearch: boolean;
  xSearch: boolean;
  onUpdate?: (snapshot: ChatStreamSnapshot) => void;
  signal?: AbortSignal;
}): Promise<ChatResponseResult> {
  const body: Record<string, unknown> = {
    model: input.model,
    input: input.messages.map(({ role, content }) => ({ role, content })),
    stream: true,
    store: false,
  };
  if (input.promptCacheKey) body.prompt_cache_key = input.promptCacheKey;
  if (input.reasoningEffort === "auto") body.reasoning = { summary: "auto" };
  else if (input.reasoningEffort !== "none") body.reasoning = { effort: input.reasoningEffort, summary: "auto" };
  else body.reasoning = { effort: "none" };
  const tools: Array<{ type: string }> = [];
  if (input.webSearch) tools.push({ type: "web_search" });
  if (input.xSearch) tools.push({ type: "x_search" });
  if (tools.length > 0) body.tools = tools;
  return publicResponsesStream(input.apiKey, body, input.onUpdate, input.signal);
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

async function publicResponsesStream(apiKey: string, body: Record<string, unknown>, onUpdate?: (snapshot: ChatStreamSnapshot) => void, signal?: AbortSignal): Promise<ChatResponseResult> {
  const response = await fetch("/v1/responses", {
    method: "POST",
    headers: new Headers({ Accept: "text/event-stream", Authorization: `Bearer ${apiKey}`, "Content-Type": "application/json" }),
    body: JSON.stringify(body),
    signal,
  });
  if (!response.ok) {
    const responseText = await response.text();
    const payload = parseJSON(responseText);
    const error = readError(payload);
    throw new CreativeApiError(response.status, error.message ?? (responseText.trim() || response.statusText || `HTTP ${response.status}`), error.code);
  }
  if (!response.headers.get("content-type")?.includes("text/event-stream")) {
    const responseText = await response.text();
    const payload = parseJSON(responseText);
    const text = readResponseText(payload);
    const reasoning = readResponseReasoning(payload);
    const tools = readResponseTools(payload);
    if (!text && !reasoning && tools.length === 0) throw new CreativeApiError(response.status, "The Responses API did not return any displayable output", "invalid_response");
    return { text, reasoning, tools };
  }
  if (!response.body) throw new CreativeApiError(response.status, "The Responses API stream was empty", "invalid_response");

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let text = "";
  let reasoning = "";
  const tools = new Map<string, ChatToolActivity>();
  const snapshot = (): ChatStreamSnapshot => ({ text, reasoning, tools: Array.from(tools.values()) });
  const emit = () => onUpdate?.(snapshot());
  const applyEnvelope = (payload: unknown) => {
    const finalText = readResponseText(payload);
    const finalReasoning = readResponseReasoning(payload);
    const finalTools = readResponseTools(payload);
    if (finalText) text = finalText;
    if (finalReasoning) reasoning = finalReasoning;
    for (const tool of finalTools) tools.set(tool.id, tool);
  };
  const consume = (block: string) => {
    const data = block.split("\n").filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n");
    if (!data || data === "[DONE]") return;
    const payload = parseJSON(data);
    if (!isRecord(payload)) return;
    const type = typeof payload.type === "string" ? payload.type : "";
    if (type === "response.output_text.delta" && typeof payload.delta === "string") {
      text += payload.delta;
      emit();
      return;
    }
    if (type === "response.output_text.done" && typeof payload.text === "string") {
      text = payload.text;
      emit();
      return;
    }
    if ((type === "response.reasoning_summary_text.delta" || type === "response.reasoning_text.delta") && typeof payload.delta === "string") {
      reasoning += payload.delta;
      emit();
      return;
    }
    if ((type === "response.reasoning_summary_text.done" || type === "response.reasoning_text.done") && typeof payload.text === "string") {
      reasoning = payload.text;
      emit();
      return;
    }
    if (type === "response.output_item.added" || type === "response.output_item.done") {
      const item = isRecord(payload.item) ? payload.item : undefined;
      if (!item) return;
      if (item.type === "message") {
        const itemText = readContentText(item.content);
        if (type === "response.output_item.done" && itemText) text = itemText;
      } else if (item.type === "reasoning") {
        const itemReasoning = readReasoningItem(item);
        if (itemReasoning) reasoning = itemReasoning;
      } else {
        const tool = readToolItem(item, type === "response.output_item.done" ? "completed" : "in_progress");
        if (tool) tools.set(tool.id, tool);
      }
      emit();
      return;
    }
    if (type === "response.function_call_arguments.delta" || type === "response.custom_tool_call_input.delta") {
      updateToolDetail(tools, payload, typeof payload.delta === "string" ? payload.delta : "", true);
      emit();
      return;
    }
    if (type === "response.function_call_arguments.done" || type === "response.custom_tool_call_input.done") {
      const detail = typeof payload.arguments === "string" ? payload.arguments : typeof payload.input === "string" ? payload.input : "";
      updateToolDetail(tools, payload, detail, false);
      emit();
      return;
    }
    if (type === "response.created" || type === "response.in_progress" || type === "response.completed" || type === "response.incomplete") {
      const responsePayload = isRecord(payload.response) ? payload.response : undefined;
      if (type === "response.completed" || type === "response.incomplete") {
        applyEnvelope(responsePayload);
        emit();
      }
      if (type === "response.incomplete") {
        throw new CreativeApiError(response.status, readIncompleteReason(responsePayload) || "The response ended before completion", "incomplete_response");
      }
      return;
    }
    if (type === "response.failed" || type === "error") {
      const error = readError(isRecord(payload.response) ? payload.response : payload);
      throw new CreativeApiError(response.status, error.message ?? "The Responses API stream failed", error.code);
    }
  };

  while (true) {
    const { done, value } = await reader.read();
    buffer = (buffer + decoder.decode(value, { stream: !done })).replaceAll("\r\n", "\n");
    let boundary = buffer.indexOf("\n\n");
    while (boundary >= 0) {
      consume(buffer.slice(0, boundary));
      buffer = buffer.slice(boundary + 2);
      boundary = buffer.indexOf("\n\n");
    }
    if (done) break;
  }
  if (buffer.trim()) consume(buffer);
  if (!text.trim() && !reasoning.trim() && tools.size === 0) throw new CreativeApiError(response.status, "The Responses API did not return any displayable output", "invalid_response");
  return snapshot();
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

function readResponseText(payload: unknown): string {
  if (!isRecord(payload)) return "";
  if (typeof payload.output_text === "string" && payload.output_text.trim()) return payload.output_text.trim();
  if (!Array.isArray(payload.output)) return "";
  return payload.output.flatMap((item) => {
    if (!isRecord(item) || item.type !== "message") return [];
    return [readContentText(item.content)];
  }).filter(Boolean).join("\n").trim();
}

function readResponseReasoning(payload: unknown): string {
  if (!isRecord(payload) || !Array.isArray(payload.output)) return "";
  return payload.output.flatMap((item) => {
    if (!isRecord(item) || item.type !== "reasoning") return [];
    return [readReasoningItem(item)];
  }).filter(Boolean).join("\n").trim();
}

function readResponseTools(payload: unknown): ChatToolActivity[] {
  if (!isRecord(payload) || !Array.isArray(payload.output)) return [];
  return payload.output.flatMap((item) => {
    if (!isRecord(item) || item.type === "message" || item.type === "reasoning") return [];
    const tool = readToolItem(item, "completed");
    return tool ? [tool] : [];
  });
}

function readReasoningItem(item: Record<string, unknown>): string {
  const summary = readContentText(item.summary);
  return summary || readContentText(item.content);
}

function readToolItem(item: Record<string, unknown>, fallbackStatus: ChatToolActivity["status"]): ChatToolActivity | null {
  const type = typeof item.type === "string" ? item.type.trim() : "";
  if (!type) return null;
  const id = firstString(item.id, item.call_id) || `${type}-${firstString(item.name) || "tool"}`;
  const name = firstString(item.name) || toolNameFromType(type);
  const action = isRecord(item.action) ? item.action : undefined;
  const detail = firstString(item.arguments, item.input, action?.query, item.query);
  return { id, type, name, status: readToolStatus(item.status, fallbackStatus), detail };
}

function updateToolDetail(tools: Map<string, ChatToolActivity>, payload: Record<string, unknown>, detail: string, append: boolean): void {
  const id = firstString(payload.item_id, payload.call_id);
  if (!id) return;
  const current = tools.get(id) ?? { id, type: "function_call", name: "tool", status: "in_progress" as const, detail: "" };
  tools.set(id, { ...current, detail: append ? current.detail + detail : detail || current.detail });
}

function readToolStatus(value: unknown, fallback: ChatToolActivity["status"]): ChatToolActivity["status"] {
  if (value === "completed") return "completed";
  if (value === "failed" || value === "incomplete") return "failed";
  if (value === "in_progress" || value === "searching") return "in_progress";
  return fallback;
}

function toolNameFromType(type: string): string {
  if (type === "web_search_call" || type === "web_search") return "web_search";
  if (type === "x_search_call" || type === "x_search") return "x_search";
  return type.replace(/_call$/, "");
}

function readIncompleteReason(payload: unknown): string {
  if (!isRecord(payload) || !isRecord(payload.incomplete_details)) return "";
  const reason = firstString(payload.incomplete_details.reason);
  return reason ? `The response was incomplete: ${reason}` : "";
}

function firstString(...values: unknown[]): string {
  for (const value of values) {
    if (typeof value === "string" && value.trim()) return value.trim();
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

function parseJSON(value: string): unknown {
  if (!value.trim()) return null;
  try {
    return JSON.parse(value);
  } catch {
    return null;
  }
}

function isVideoStatus(value: unknown): value is VideoStatus["status"] {
  return value === "pending" || value === "done" || value === "failed";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
