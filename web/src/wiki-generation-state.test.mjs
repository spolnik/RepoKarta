import assert from "node:assert/strict";
import test from "node:test";

import {
  createWikiGenerationState,
  reduceWikiGeneration,
  wikiGenerationProgress,
} from "./wiki-generation-state.mjs";

test("Wiki generation stages and page targets transition through one reducer", () => {
  let state = reduceWikiGeneration(createWikiGenerationState(), { type: "reset" });
  state = reduceWikiGeneration(state, {
    type: "stage",
    stage: "survey",
    state: "reused",
  });
  state = reduceWikiGeneration(state, {
    type: "stage",
    stage: "plan",
    state: "done",
  });
  assert.deepEqual(wikiGenerationProgress(state), {
    kind: "stages",
    done: 2,
    total: 3,
    percentage: 67,
  });

  state = reduceWikiGeneration(state, {
    type: "targets",
    targets: [
      { slug: "architecture", number: "1", title: "Architecture" },
      { slug: "operations", number: "2", title: "Operations" },
    ],
  });
  state = reduceWikiGeneration(state, {
    type: "target",
    index: 0,
    state: "done",
  });
  assert.deepEqual(wikiGenerationProgress(state), {
    kind: "pages",
    done: 1,
    total: 2,
    percentage: 50,
  });
  assert.equal(state.targets[1].state, "queued");
});

test("Wiki generation cancellation and failure are explicit terminal states", () => {
  const running = reduceWikiGeneration(createWikiGenerationState(), { type: "reset" });
  assert.equal(reduceWikiGeneration(running, { type: "cancel" }).status, "cancelled");
  assert.equal(reduceWikiGeneration(running, { type: "fail" }).status, "failed");
});
