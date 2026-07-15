import assert from "node:assert/strict";
import test from "node:test";

import {
  createChatCompletion,
  createPublicApiBaseURL,
  createVideo,
  generateImage,
  getVideo,
} from "./creative-console-api.ts";

type FetchCall = {
  input: string;
  init?: RequestInit;
};

function jsonResponse(payload: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(payload), {
    status: init.status ?? 200,
    headers: { "Content-Type": "application/json", ...init.headers },
  });
}

async function withFetchMock<T>(handler: (input: string, init?: RequestInit) => Response | Promise<Response>, run: () => Promise<T>): Promise<T> {
  const previousFetch = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => handler(String(input), init)) as typeof fetch;
  try {
    return await run();
  } finally {
    globalThis.fetch = previousFetch;
  }
}

function readBody(call: FetchCall): Record<string, unknown> {
  const body = call.init?.body;
  assert.equal(typeof body, "string");
  return JSON.parse(body as string) as Record<string, unknown>;
}

test("createPublicApiBaseURL does not duplicate a case-insensitive v1 suffix", () => {
  assert.equal(createPublicApiBaseURL("https://api.example.com/V1/"), "https://api.example.com/V1");
});

test("createPublicApiBaseURL falls back to the browser origin for an empty configured value", () => {
  const previousWindow = globalThis.window;
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: { location: { origin: "https://panel.example.com" } },
  });
  try {
    assert.equal(createPublicApiBaseURL(""), "https://panel.example.com/v1");
  } finally {
    Object.defineProperty(globalThis, "window", { configurable: true, value: previousWindow });
  }
});

test("createChatCompletion uses the OpenAI endpoint, bearer key, and non-streaming body", async () => {
  const calls: FetchCall[] = [];
  const text = await withFetchMock((input, init) => {
    calls.push({ input, init });
    return jsonResponse({ choices: [{ message: { content: "pong" } }] });
  }, () => createChatCompletion({
    publicApiBaseURL: "https://api.example.com/root/",
    apiKey: "test-key",
    model: "grok-chat",
    messages: [{ role: "user", content: "ping" }],
  }));

  assert.equal(text, "pong");
  assert.equal(calls.length, 1);
  assert.equal(calls[0]?.input, "https://api.example.com/root/v1/chat/completions");
  assert.equal(calls[0]?.init?.method, "POST");
  const headers = new Headers(calls[0]?.init?.headers);
  assert.equal(headers.get("Authorization"), "Bearer test-key");
  assert.equal(headers.get("Content-Type"), "application/json");
  assert.deepEqual(readBody(calls[0]!), {
    model: "grok-chat",
    messages: [{ role: "user", content: "ping" }],
    stream: false,
  });
});

test("generateImage uses the generation contract and resolves relative media URLs", async () => {
  const calls: FetchCall[] = [];
  const images = await withFetchMock((input, init) => {
    calls.push({ input, init });
    return jsonResponse({ data: [{ url: "/media/generated.png", revised_prompt: "refined" }] });
  }, () => generateImage({
    publicApiBaseURL: "https://api.example.com/root",
    apiKey: "test-key",
    model: "grok-image",
    prompt: "draw a raven",
    count: 2,
    aspectRatio: "3:2",
    resolution: "2k",
  }));

  assert.deepEqual(images, [{ url: "https://api.example.com/media/generated.png", revisedPrompt: "refined" }]);
  assert.equal(calls[0]?.input, "https://api.example.com/root/v1/images/generations");
  assert.deepEqual(readBody(calls[0]!), {
    model: "grok-image",
    prompt: "draw a raven",
    n: 2,
    aspect_ratio: "3:2",
    resolution: "2k",
    response_format: "url",
    stream: false,
  });
});

test("generateImage resolves path-relative media URLs under the configured API base path", async () => {
  const images = await withFetchMock(
    () => jsonResponse({ data: [{ url: "media/generated.png" }] }),
    () => generateImage({
      publicApiBaseURL: "https://api.example.com/root",
      apiKey: "test-key",
      model: "grok-image",
      prompt: "draw a raven",
      count: 1,
      aspectRatio: "1:1",
      resolution: "1k",
    }),
  );
  assert.equal(images[0]?.url, "https://api.example.com/root/media/generated.png");
});

test("createVideo sends the official image.url shape and getVideo polls the official endpoint", async () => {
  const calls: FetchCall[] = [];
  const result = await withFetchMock((input, init) => {
    calls.push({ input, init });
    if (calls.length === 1) return jsonResponse({ request_id: "job/1" });
    return jsonResponse({ status: "done", progress: 100, video: { url: "videos/result.mp4", duration: 8 } });
  }, async () => {
    const requestId = await createVideo({
      publicApiBaseURL: "https://api.example.com/root",
      apiKey: "test-key",
      model: "grok-video",
      prompt: "animate it",
      imageURL: "https://images.example.com/input.png",
      duration: 8,
      aspectRatio: "16:9",
      resolution: "720p",
    });
    const status = await getVideo({
      publicApiBaseURL: "https://api.example.com/root",
      apiKey: "test-key",
      requestId,
    });
    return { requestId, status };
  });

  assert.equal(result.requestId, "job/1");
  assert.deepEqual(readBody(calls[0]!), {
    model: "grok-video",
    prompt: "animate it",
    duration: 8,
    aspect_ratio: "16:9",
    resolution: "720p",
    image: { url: "https://images.example.com/input.png" },
  });
  assert.equal(calls[1]?.input, "https://api.example.com/root/v1/videos/job%2F1");
  assert.equal(calls[1]?.init?.method, "GET");
  assert.equal(result.status.status, "done");
  assert.equal(result.status.video?.url, "https://api.example.com/root/videos/result.mp4");
});

test("public API errors preserve the sanitized OpenAI message and code", async () => {
  await withFetchMock(() => jsonResponse({ error: { code: "invalid_api_key", message: "bad key" } }, { status: 401 }), async () => {
    await assert.rejects(
      () => createChatCompletion({
        publicApiBaseURL: "https://api.example.com",
        apiKey: "bad-key",
        model: "grok-chat",
        messages: [{ role: "user", content: "ping" }],
      }),
      (error: unknown) => {
        if (!(error instanceof Error)) return false;
        assert.equal(error.message, "bad key");
        assert.equal((error as Error & { status?: number }).status, 401);
        assert.equal((error as Error & { code?: string }).code, "invalid_api_key");
        return true;
      },
    );
  });
});
