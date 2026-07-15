import assert from "node:assert/strict";
import test from "node:test";

import { createCreativeMessageId } from "./creative-console-id.ts";

test("createCreativeMessageId works when randomUUID is unavailable on an HTTP origin", () => {
  const originalCrypto = globalThis.crypto;
  Object.defineProperty(globalThis, "crypto", {
    configurable: true,
    value: { getRandomValues: originalCrypto.getRandomValues.bind(originalCrypto) },
  });

  try {
    const first = createCreativeMessageId();
    const second = createCreativeMessageId();
    assert.match(first, /^creative-/);
    assert.notEqual(first, second);
  } finally {
    Object.defineProperty(globalThis, "crypto", { configurable: true, value: originalCrypto });
  }
});

test("createCreativeMessageId uses randomUUID when available", () => {
  const originalCrypto = globalThis.crypto;
  Object.defineProperty(globalThis, "crypto", {
    configurable: true,
    value: { randomUUID: () => "known-uuid" },
  });

  try {
    assert.equal(createCreativeMessageId(), "known-uuid");
  } finally {
    Object.defineProperty(globalThis, "crypto", { configurable: true, value: originalCrypto });
  }
});
