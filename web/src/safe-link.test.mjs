import assert from "node:assert/strict";
import test from "node:test";

import { safeHTTPURL, setSafeHTTPLink } from "./safe-link.mjs";

test("server-provided links accept only HTTP(S) and safe relative URLs", () => {
  assert.equal(
    safeHTTPURL("/source/7?path=main.go", "https://repo.example/chat")?.href,
    "https://repo.example/source/7?path=main.go",
  );
  assert.equal(safeHTTPURL("javascript:alert(1)", "https://repo.example"), null);
  assert.equal(safeHTTPURL("data:text/html,pwned", "https://repo.example"), null);
  assert.equal(safeHTTPURL("http://[", "https://repo.example"), null);
});

test("unsafe anchors lose navigation and are visibly disabled to assistive technology", () => {
  const attributes = new Map([["href", "javascript:alert(1)"]]);
  const anchor = {
    dataset: {},
    removeAttribute: (name) => attributes.delete(name),
    setAttribute: (name, value) => attributes.set(name, value),
    set href(value) {
      attributes.set("href", value);
    },
  };
  assert.equal(
    setSafeHTTPLink(
      /** @type {unknown as HTMLAnchorElement} */ (anchor),
      "javascript:alert(1)",
      "https://repo.example",
    ),
    false,
  );
  assert.equal(attributes.has("href"), false);
  assert.equal(attributes.get("aria-disabled"), "true");
});
