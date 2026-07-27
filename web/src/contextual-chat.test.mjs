import assert from "node:assert/strict";
import test from "node:test";

import { buildContextualChatURL, normaliseContextMode } from "./contextual-chat.mjs";

test("contextual chat URLs preserve the mode, context, and submitted question", () => {
  const result = new URL(buildContextualChatURL("https://repo.example.com/maps?repository=8", {
    mode: "maps",
    contextURL: "https://repo.example.com/maps?repository=8&view=dependencies",
    prompt: "  What depends on this module?  "
  }));

  assert.equal(result.pathname, "/chat");
  assert.equal(result.searchParams.get("mode"), "maps");
  assert.equal(
    result.searchParams.get("context_url"),
    "https://repo.example.com/maps?repository=8&view=dependencies"
  );
  assert.equal(result.searchParams.get("prompt"), "What depends on this module?");
  assert.equal(result.searchParams.get("autostart"), "true");
});

test("contextual chat URLs can start fleet-wide conversations without a repository context", () => {
  const result = new URL(buildContextualChatURL("https://repo.example.com/insights", {
    mode: "insights",
    prompt: "Compare the riskiest repositories"
  }));

  assert.equal(result.searchParams.has("context_url"), false);
  assert.equal(result.searchParams.get("mode"), "insights");
});

test("unknown modes are normalised to a generic context", () => {
  assert.equal(normaliseContextMode(" SOURCE "), "source");
  assert.equal(normaliseContextMode("project"), "project");
  assert.equal(normaliseContextMode("admin"), "context");
  assert.equal(normaliseContextMode(""), "context");
});
