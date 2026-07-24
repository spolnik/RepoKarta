import assert from "node:assert/strict";
import test from "node:test";

import { createFrameBatcher } from "./frame-batcher.mjs";

test("coalesces thousands of deltas into one write per frame", () => {
  const frames = [];
  const cancelled = [];
  const writes = [];
  let nextHandle = 0;
  const batcher = createFrameBatcher({
    schedule(callback) {
      frames.push(callback);
      return ++nextHandle;
    },
    cancel(handle) {
      cancelled.push(handle);
    },
    write(text) {
      writes.push(text);
    }
  });

  for (let index = 0; index < 25_000; index++) {
    batcher.append("x");
  }
  assert.equal(frames.length, 1);
  assert.deepEqual(writes, []);

  frames.shift()();
  assert.equal(writes.length, 1);
  assert.equal(writes[0].length, 25_000);

  batcher.append("next");
  assert.equal(frames.length, 1);
  assert.equal(batcher.finish(), 2);
  assert.deepEqual(writes, ["x".repeat(25_000), "next"]);
  assert.deepEqual(cancelled, [2]);
});

test("cancel discards queued text and future deltas", () => {
  const frames = [];
  const writes = [];
  const batcher = createFrameBatcher({
    schedule(callback) {
      frames.push(callback);
      return frames.length;
    },
    cancel() {},
    write(text) {
      writes.push(text);
    }
  });

  batcher.append("discarded");
  batcher.cancel();
  batcher.append("ignored");
  frames.shift()();
  assert.deepEqual(writes, []);
});
