import assert from "node:assert/strict";
import test from "node:test";

import { wikiPrimaryAction } from "./wiki-run-state.mjs";

const completeSite = {
  planReady: true,
  surveyReady: true,
  planStale: false,
  stale: 0,
  pending: 0,
  failed: 0
};

test("an active Wiki run becomes a navigation action, never another generation trigger", () => {
  assert.deepEqual(wikiPrimaryAction(true, true, completeSite), {
    disabled: false,
    mode: "view",
    label: "View active run"
  });
});

test("the ordinary generation action still follows provider and site state", () => {
  assert.deepEqual(wikiPrimaryAction(false, false, completeSite), {
    disabled: true,
    mode: "generate",
    label: "Refresh all"
  });
  assert.equal(
    wikiPrimaryAction(false, true, { ...completeSite, pending: 7, failed: 1 }).label,
    "Generate 8 pages"
  );
});
