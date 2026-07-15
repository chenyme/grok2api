import assert from "node:assert/strict";
import test from "node:test";

import {
  CreativeApiError,
  createPublicApiBaseURL,
  readChatText,
  readError,
  readImages,
  readVideoRequestID,
  readVideoStatus,
} from "./creative-console-api-parsers.ts";
import { listAllPaginatedItems } from "./creative-console-pagination.ts";

test("createPublicApiBaseURL normalizes trailing slashes and an existing v1 suffix", () => {
  assert.equal(createPublicApiBaseURL("https://api.example.com/"), "https://api.example.com/v1");
  assert.equal(createPublicApiBaseURL("https://api.example.com/base/v1/"), "https://api.example.com/base/v1");
  assert.equal(createPublicApiBaseURL("   "), "");
});

test("readVideoRequestID trims a valid request ID and rejects malformed payloads", () => {
  assert.equal(readVideoRequestID({ request_id: " job-1 " }), "job-1");
  assert.equal(readVideoRequestID({ request_id: 1 }), "");
});

test("readChatText supports string and content-part responses", () => {
  assert.equal(readChatText({ choices: [{ message: { content: " hello " } }] }), "hello");
  assert.equal(readChatText({ choices: [{ message: { content: [{ text: "first" }, { content: "second" }] } }] }), "first\nsecond");
});

test("readImages supports URL and base64 results", () => {
  assert.deepEqual(readImages({ data: [{ url: "/media/a.png", revised_prompt: "revised" }, { b64_json: "YWJj" }] }), [
    { url: "/media/a.png", revisedPrompt: "revised" },
    { url: "data:image/png;base64,YWJj", revisedPrompt: undefined },
  ]);
});

test("readVideoStatus parses pending, done, and failed polling responses", () => {
  assert.deepEqual(readVideoStatus({ status: "pending", model: "video-model", progress: 42 }), {
    status: "pending",
    model: "video-model",
    progress: 42,
  });
  assert.deepEqual(readVideoStatus({ status: "done", progress: 100, video: { url: "/media/video.mp4", duration: 8, respect_moderation: true } }), {
    status: "done",
    model: undefined,
    progress: 100,
    video: { url: "/media/video.mp4", duration: 8, respectModeration: true },
  });
  assert.deepEqual(readVideoStatus({ status: "failed", error: { code: "service_unavailable", message: "try later" } }), {
    status: "failed",
    model: undefined,
    progress: 0,
    error: { code: "service_unavailable", message: "try later" },
  });
});

test("readVideoStatus rejects malformed polling responses", () => {
  assert.throws(() => readVideoStatus({ status: "completed" }), (error: unknown) => {
    if (!(error instanceof CreativeApiError)) return false;
    assert.equal(error.code, "invalid_response");
    return true;
  });
});

test("readError supports OpenAI-style and flat errors", () => {
  assert.deepEqual(readError({ error: { code: "invalid_request", message: "bad input" } }), { code: "invalid_request", message: "bad input" });
  assert.deepEqual(readError({ code: "fallback", message: "flat" }), { code: "fallback", message: "flat" });
});

test("listAllPaginatedItems follows total across pages", async () => {
  const requestedPages: number[] = [];
  const items = await listAllPaginatedItems(async (page, pageSize) => {
    requestedPages.push(page);
    const allItems = [1, 2, 3, 4, 5];
    const start = (page - 1) * pageSize;
    return { items: allItems.slice(start, start + pageSize), page, pageSize, total: allItems.length };
  }, { pageSize: 2 });
  assert.deepEqual(items, [1, 2, 3, 4, 5]);
  assert.deepEqual(requestedPages, [1, 2, 3]);
});

test("listAllPaginatedItems stops on a short page even when total is stale", async () => {
  let calls = 0;
  const items = await listAllPaginatedItems(async (page, pageSize) => {
    calls += 1;
    return { items: page === 1 ? [1, 2] : [], page, pageSize, total: 100 };
  });
  assert.deepEqual(items, [1, 2]);
  assert.equal(calls, 2);
});

test("listAllPaginatedItems continues when the server caps page size below the requested size", async () => {
  const requestedPages: number[] = [];
  const items = await listAllPaginatedItems(async (page) => {
    requestedPages.push(page);
    return { items: page === 1 ? [1, 2] : [3, 4], page, pageSize: 2, total: 4 };
  }, { pageSize: 100 });

  assert.deepEqual(items, [1, 2, 3, 4]);
  assert.deepEqual(requestedPages, [1, 2]);
});

test("listAllPaginatedItems stops on an empty page and respects its safety bound", async () => {
  let calls = 0;
  const emptyStopped = await listAllPaginatedItems(async (page, pageSize) => {
    calls += 1;
    return { items: [], page, pageSize, total: 10 };
  }, { pageSize: 1 });
  assert.deepEqual(emptyStopped, []);
  assert.equal(calls, 1);

  calls = 0;
  const bounded = await listAllPaginatedItems(async (page, pageSize) => {
    calls += 1;
    return { items: [page], page, pageSize, total: 100 };
  }, { pageSize: 1, maxPages: 2 });
  assert.deepEqual(bounded, [1, 2]);
  assert.equal(calls, 2);
});
