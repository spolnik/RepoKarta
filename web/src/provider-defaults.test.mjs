import assert from "node:assert/strict";
import test from "node:test";

import {
  recommendedProviderEffort,
  recommendedProviderModel
} from "./provider-defaults.mjs";

test("Claude harnesses default to Opus 5 with medium effort", () => {
  for (const provider of ["claude", "anthropic-api"]) {
    assert.equal(recommendedProviderModel(provider), "claude-opus-5");
    assert.equal(recommendedProviderEffort(provider), "medium");
  }
});

test("Codex keeps its model default without changing Chat effort policy", () => {
  assert.equal(recommendedProviderModel("codex"), "gpt-5.6-sol");
  assert.equal(recommendedProviderEffort("codex"), "");
});
