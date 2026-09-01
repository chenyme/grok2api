import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  formatUSDTicks,
  formatUSDTicksWithEstimate,
  USD_TICKS_PER_DOLLAR,
  usdTicksToValue,
} from "./usd.ts";

describe("USD tick formatting", () => {
  it("uses ten billion ticks per US dollar", () => {
    assert.equal(USD_TICKS_PER_DOLLAR, 10_000_000_000);
    assert.equal(usdTicksToValue(200_000_000), 0.02);
    assert.equal(formatUSDTicks(200_000_000, 6), "$0.020000");
  });

  it("prefers reported cost over an estimate", () => {
    assert.equal(
      formatUSDTicksWithEstimate(200_000_000, 900_000_000, "estimated"),
      "$0.020000",
    );
  });

  it("labels estimate-only costs", () => {
    assert.equal(
      formatUSDTicksWithEstimate(0, 200_000_000, "estimated"),
      "$0.020000 (estimated)",
    );
  });

  it("normalizes missing and invalid costs to zero", () => {
    assert.equal(formatUSDTicksWithEstimate(0, 0, "estimated"), "$0");
    assert.equal(formatUSDTicksWithEstimate(-1, Number.NaN, "estimated"), "$0");
  });
});
