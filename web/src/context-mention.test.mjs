import assert from "node:assert/strict";
import test from "node:test";

import { activeContextMention } from "./context-mention.mjs";

test("recognizes every structured context kind and keeps spaces in queries", () => {
  for (const kind of ["repository", "file", "directory", "symbol"]) {
    const value = `inspect @${kind}:payment service`;
    assert.deepEqual(activeContextMention(value, value.length), {
      kind,
      query: "payment service",
      start: 8,
      end: value.length
    });
  }
});

test("uses the mention nearest the caret", () => {
  const value = "@repository:first @symbol:HandleRequest";
  assert.deepEqual(activeContextMention(value, value.length), {
    kind: "symbol",
    query: "HandleRequest",
    start: 18,
    end: value.length
  });
});

test("rejects embedded, multiline, and absent mentions", () => {
  assert.equal(activeContextMention("email@symbol:Thing", 18), undefined);
  assert.equal(activeContextMention("@directory:one\ntwo", 18), undefined);
  assert.equal(activeContextMention("plain question", 14), undefined);
});
