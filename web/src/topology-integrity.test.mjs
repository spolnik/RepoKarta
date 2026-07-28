import assert from "node:assert/strict";
import test from "node:test";

import { filterTopologyConnections } from "./topology-integrity.mjs";

test("keeps valid topology while hiding every dangling connection", () => {
  const valid = { id: "valid", source: "checkout", target: "orders" };
  const missingSource = { id: "missing-source", source: "deployment", target: "orders" };
  const missingTarget = { id: "missing-target", source: "checkout", target: "payments" };
  const missingBoth = { id: "missing-both", source: "unknown-a", target: "unknown-b" };

  const result = filterTopologyConnections(
    [{ id: "checkout" }, { id: "orders" }],
    [valid, missingSource, missingTarget, missingBoth]
  );

  assert.deepEqual(result.connections, [valid]);
  assert.equal(result.hiddenCount, 3);
});

test("accepts an empty component or connection set", () => {
  assert.deepEqual(filterTopologyConnections([], []), {
    connections: [],
    hiddenCount: 0
  });
  assert.deepEqual(
    filterTopologyConnections([], [{ source: "missing", target: "missing" }]),
    { connections: [], hiddenCount: 1 }
  );
});
