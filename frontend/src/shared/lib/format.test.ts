import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { formatTokenMillions } from "./format.ts";

describe("formatTokenMillions", () => {
  it("uses a stable M suffix for large values", () => {
    assert.equal(formatTokenMillions(0, "en"), "0");
    assert.equal(formatTokenMillions(999, "en"), "999");
    assert.equal(formatTokenMillions(1_000, "en"), "0.001M");
    assert.equal(formatTokenMillions(500_000, "en"), "0.5M");
    assert.equal(formatTokenMillions(1_000_000, "en"), "1M");
    assert.equal(formatTokenMillions(1_234_567, "en"), "1.23M");
  });

  it("normalizes invalid and negative values", () => {
    assert.equal(formatTokenMillions(Number.NaN, "en"), "0");
    assert.equal(formatTokenMillions(-10, "en"), "0");
  });
});
