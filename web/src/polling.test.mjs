import assert from "node:assert/strict";
import test from "node:test";

import { boundedPollRetry } from "./polling.mjs";

test("poll retries back off and stop at a bounded failure count", () => {
  assert.deepEqual(boundedPollRetry(1), { retry: true, delayMS: 500 });
  assert.deepEqual(boundedPollRetry(4), { retry: true, delayMS: 4000 });
  assert.deepEqual(boundedPollRetry(5), { retry: true, delayMS: 8000 });
  assert.deepEqual(boundedPollRetry(6), { retry: false, delayMS: 8000 });
});
