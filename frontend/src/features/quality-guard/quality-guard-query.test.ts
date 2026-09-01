import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { qualityGuardStatusPath } from "./quality-guard-query.ts";

describe("qualityGuardStatusPath", () => {
  it("keeps the legacy full-state request when no node filter is supplied", () => {
    assert.equal(qualityGuardStatusPath(), "/api/admin/v1/egress-quality-guard");
  });

  it("requests no node details for an empty page", () => {
    assert.equal(qualityGuardStatusPath([]), "/api/admin/v1/egress-quality-guard?nodeId=");
  });

  it("limits status details to the current page node IDs", () => {
    assert.equal(qualityGuardStatusPath(["12", "34"]), "/api/admin/v1/egress-quality-guard?nodeId=12&nodeId=34");
  });
});
