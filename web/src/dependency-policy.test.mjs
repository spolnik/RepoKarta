import assert from "node:assert/strict";
import test from "node:test";

import { validateOverrides } from "../scripts/check-dependency-policy.mjs";

test("dependency overrides must be bounded and represented in the lockfile", () => {
  assert.deepEqual(
    validateOverrides(
      { "es-toolkit": "1.49.0" },
      { "node_modules/mermaid/node_modules/es-toolkit": { version: "1.49.0" } },
    ),
    [],
  );
  assert.deepEqual(
    validateOverrides(
      {
        unbounded: "*",
        missing: "2.0.0",
        mismatched: "~3.2.0",
      },
      {
        "node_modules/mismatched": { version: "3.3.0" },
      },
    ),
    [
      'override unbounded uses unbounded or non-registry spec "*"',
      "override missing (2.0.0) does not match package-lock.json (missing)",
      "override mismatched (~3.2.0) does not match package-lock.json (3.3.0)",
    ],
  );
});
