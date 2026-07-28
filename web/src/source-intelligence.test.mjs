import assert from "node:assert/strict";
import test from "node:test";

import {
  sourceHighlightLanguage,
  sourceSearchSummary,
  sourceSearchURL,
  sourceSelection
} from "./source-intelligence.mjs";

test("uses semantic references for a selected identifier", () => {
  assert.deepEqual(sourceSelection("  OrderStatus  "), {
    query: "OrderStatus",
    mode: "references"
  });
  assert.deepEqual(sourceSelection("orders.findById("), {
    query: "orders.findById(",
    mode: "zoekt"
  });
});

test("rejects multiline and oversized editor selections", () => {
  assert.equal(sourceSelection("first\nsecond"), undefined);
  assert.equal(sourceSelection("x".repeat(241)), undefined);
});

test("locks embedded search to the current repository", () => {
  const target = sourceSearchURL(42, "Order Status", "references");
  assert.equal(
    target,
    "/api/search?q=Order+Status&repo=42&mode=references&limit=50"
  );
});

test("summarizes reference precision and partial results", () => {
  assert.equal(
    sourceSearchSummary({
      match_count: 3,
      returned_files: 2,
      truncated: true,
      reference_index: { provider: "scip" }
    }),
    "3 matches · 2 files · scip references · partial result"
  );
});

test("resolves result syntax from declared languages and file extensions", () => {
  assert.equal(sourceHighlightLanguage("TypeScript", "src/order.ts"), "typescript");
  assert.equal(sourceHighlightLanguage("golang", "service.go"), "go");
  assert.equal(sourceHighlightLanguage("", "src/main.kt"), "kotlin");
  assert.equal(sourceHighlightLanguage(undefined, "README.md"), "markdown");
  assert.equal(sourceHighlightLanguage(undefined, "LICENSE"), "");
});
