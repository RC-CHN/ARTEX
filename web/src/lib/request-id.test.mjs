import { newRequestId } from "./request-id.ts";
import assert from "node:assert/strict";
import { webcrypto } from "node:crypto";
import test from "node:test";

test("request identifiers work on secure, ordinary HTTP and legacy origins", () => {
  const original = Object.getOwnPropertyDescriptor(globalThis, "crypto");
  try {
    for (const crypto of [webcrypto, { getRandomValues: webcrypto.getRandomValues.bind(webcrypto) }, undefined]) {
      Object.defineProperty(globalThis, "crypto", { configurable: true, value: crypto });
      const ids = new Set(Array.from({ length: 1000 }, newRequestId));
      assert.equal(ids.size, 1000);
      for (const id of ids) assert.match(id, /^[A-Za-z0-9-]{16,128}$/);
    }
  } finally {
    if (original) Object.defineProperty(globalThis, "crypto", original);
    else delete globalThis.crypto;
  }
});
