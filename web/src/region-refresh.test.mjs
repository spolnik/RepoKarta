import assert from "node:assert/strict";
import test from "node:test";

import { replaceRenderedRegions } from "./region-refresh.mjs";

test("targeted completion refresh replaces requested regions and preserves missing ones", () => {
  const replacements = [];
  const current = {
    querySelector: (selector) =>
      selector === "#ready"
        ? { replaceWith: (value) => replacements.push(value) }
        : selector === "#preserved"
          ? { replaceWith: (value) => replacements.push(value) }
          : null,
  };
  const freshReady = { id: "fresh-ready" };
  const fresh = {
    querySelector: (selector) => selector === "#ready" ? freshReady : null,
  };
  assert.deepEqual(
    replaceRenderedRegions(
      /** @type {unknown as ParentNode} */ (current),
      /** @type {unknown as ParentNode} */ (fresh),
      ["#ready", "#preserved"],
    ),
    {
      replaced: ["#ready"],
      missing: ["#preserved"],
    },
  );
  assert.deepEqual(replacements, [freshReady]);
});
