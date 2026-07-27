import assert from "node:assert/strict";
import test from "node:test";
import { applyQueryCompletion } from "./query-completion.mjs";

test("applies a completion without disturbing surrounding query text", () => {
  const result = applyQueryCompletion("needle repo:pay lang:go", {
    insert_text: "repository:payments",
    replace_start: 7,
    replace_end: 15
  });

  assert.deepEqual(result, {
    value: "needle repository:payments lang:go",
    cursor: 26
  });
});

test("uses browser UTF-16 offsets when non-BMP text precedes the edit", () => {
  const result = applyQueryCompletion("😀 repo:pay", {
    insert_text: "repository:payments",
    replace_start: 3,
    replace_end: 11
  });

  assert.deepEqual(result, {
    value: "😀 repository:payments",
    cursor: 22
  });
});
