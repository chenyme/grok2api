import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  buildAuditUsageView,
  MISSING_AUDIT_USAGE_PLACEHOLDER,
  type AuditUsageInput,
  type AuditUsageLabels,
} from "./audit-usage.ts";

const labels: AuditUsageLabels = {
  input: "输入",
  output: "输出",
  cached: "缓存",
  reasoning: "推理",
  mediaInput: "媒体输入",
  mediaOutput: "媒体输出",
  imageCount: (count) => `${count} 张`,
  secondsCount: (count) => `${count} 秒`,
};

function formatNumber(value: number): string {
  return new Intl.NumberFormat("en-US", { maximumFractionDigits: 2 }).format(value);
}

function audit(overrides: Partial<AuditUsageInput> = {}): AuditUsageInput {
  return {
    operation: "responses",
    usageSource: "upstream",
    mediaInputImages: 0,
    mediaOutputImages: 0,
    mediaOutputSeconds: 0,
    inputTokens: 0,
    cachedInputTokens: 0,
    outputTokens: 0,
    reasoningTokens: 0,
    totalTokens: 0,
    durationMs: 1200,
    ...overrides,
  };
}

function values(items: Array<{ key: string; value: string }> | undefined, key: string): string | undefined {
  return items?.find((item) => item.key === key)?.value;
}

describe("buildAuditUsageView", () => {
  it("keeps the token grid when a chat request has media input", () => {
    const view = buildAuditUsageView(audit({
      mediaInputImages: 10,
      inputTokens: 39229,
      cachedInputTokens: 128,
      outputTokens: 275,
      reasoningTokens: 269,
      totalTokens: 39832,
    }), formatNumber, labels);

    assert.equal(view.mode, "metrics");
    assert.equal(values(view.mediaItems, "mediaInput"), "10 张");
    assert.equal(view.mediaItems?.find((item) => item.key === "mediaOutput")?.label, "媒体输出");
    assert.equal(values(view.mediaItems, "mediaOutput"), "0 张");
    assert.equal(values(view.tokenItems, "input"), "39,229");
    assert.equal(values(view.tokenItems, "cached"), "128");
    assert.equal(values(view.tokenItems, "output"), "275");
    assert.equal(values(view.tokenItems, "reasoning"), "269");
  });

  it("uses a dash when token usage is missing on a media request", () => {
    const view = buildAuditUsageView(audit({
      mediaInputImages: 10,
      usageSource: "none",
    }), formatNumber, labels);

    assert.equal(view.mode, "metrics");
    assert.equal(values(view.mediaItems, "mediaInput"), "10 张");
    assert.equal(values(view.tokenItems, "input"), MISSING_AUDIT_USAGE_PLACEHOLDER);
    assert.equal(values(view.tokenItems, "cached"), MISSING_AUDIT_USAGE_PLACEHOLDER);
    assert.equal(values(view.tokenItems, "output"), MISSING_AUDIT_USAGE_PLACEHOLDER);
    assert.equal(values(view.tokenItems, "reasoning"), MISSING_AUDIT_USAGE_PLACEHOLDER);
  });

  it("still shows zero token counts when usage was reported", () => {
    const view = buildAuditUsageView(audit({
      mediaInputImages: 2,
      usageSource: "estimated",
    }), formatNumber, labels);

    assert.equal(values(view.tokenItems, "input"), "0");
    assert.equal(values(view.tokenItems, "output"), "0");
  });

  it("keeps tokens next to video media counts", () => {
    const view = buildAuditUsageView(audit({
      operation: "video",
      mediaInputImages: 3,
      mediaOutputSeconds: 12,
      inputTokens: 80,
      outputTokens: 16,
      totalTokens: 96,
    }), formatNumber, labels);

    assert.equal(values(view.mediaItems, "mediaOutput"), "12 秒");
    assert.equal(values(view.tokenItems, "input"), "80");
    assert.equal(values(view.tokenItems, "output"), "16");
  });

  it("does not invent media rows for plain chat", () => {
    const view = buildAuditUsageView(audit({
      inputTokens: 41332,
      cachedInputTokens: 39552,
      outputTokens: 267,
      reasoningTokens: 195,
      totalTokens: 41332,
    }), formatNumber, labels);

    assert.equal(view.mediaItems, undefined);
    assert.equal(values(view.tokenItems, "input"), "41,332");
    assert.equal(values(view.tokenItems, "reasoning"), "195");
  });

  it("keeps compaction and voice-style rows without a token grid", () => {
    assert.equal(buildAuditUsageView(audit({ operation: "compaction" }), formatNumber, labels).mode, "compaction");
    assert.equal(buildAuditUsageView(audit({ operation: "tts", durationMs: 1540 }), formatNumber, labels).mode, "duration");
  });
});
