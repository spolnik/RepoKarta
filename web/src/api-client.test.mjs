import assert from "node:assert/strict";
import test from "node:test";

import {
  APIRequestError,
  apiFetch,
  getArtifactProgress,
  getHealth,
} from "./api-client.mjs";

test("the API boundary owns headers and validates load-bearing response shapes", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });
  /** @type {Headers | undefined} */
  let observedHeaders;
  globalThis.fetch = async (_input, init) => {
    observedHeaders = new Headers(init?.headers);
    return Response.json({ status: "ok", version: "0.95.0-dev" });
  };

  assert.deepEqual(await getHealth(), {
    status: "ok",
    version: "0.95.0-dev",
  });
  assert.equal(observedHeaders?.get("Accept"), "application/json");

  globalThis.fetch = async () => Response.json({ state: "ready" });
  await assert.rejects(
    getArtifactProgress(),
    (error) =>
      error instanceof APIRequestError &&
      error.message.includes("did not match the application contract"),
  );
});

test("the API boundary maps structured server errors once", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });
  globalThis.fetch = async () => Response.json(
    { error: { message: "permission denied" } },
    { status: 403 },
  );

  const response = await apiFetch("/api/protected");
  assert.equal(response.status, 403);
  await assert.rejects(
    getHealth(),
    (error) =>
      error instanceof APIRequestError &&
      error.status === 403 &&
      error.message === "permission denied",
  );
});
