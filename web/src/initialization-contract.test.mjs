import assert from "node:assert/strict";
import test from "node:test";

import { collectRequiredElements } from "./initialization-contract.mjs";

test("one initialization table owns lookups and cardinality diagnostics", () => {
  const nodes = {
    "#required": { id: "required" },
    ".many": [{ id: "one" }, { id: "two" }],
  };
  const root = {
    querySelector: (selector) => nodes[selector] ?? null,
    querySelectorAll: (selector) => nodes[selector] ?? [],
  };
  const result = collectRequiredElements(
    /** @type {unknown as ParentNode} */ (root),
    [
      { key: "required", selector: "#required" },
      { key: "many", selector: ".many", all: true, min: 1, max: 2 },
      { key: "missing", selector: "#missing" },
    ],
  );
  assert.equal(result.values.required, nodes["#required"]);
  assert.deepEqual(result.values.many, nodes[".many"]);
  assert.deepEqual(result.mismatches, [{
    key: "missing",
    selector: "#missing",
    expected: "1",
    actual: 0,
    valid: false,
  }]);
  assert.equal(result.valid, false);
});
