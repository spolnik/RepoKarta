import assert from "node:assert/strict";
import test from "node:test";

import { parseRepoKartaContextURL } from "./context-url.mjs";

const baseURL = "https://repo.example.com/chat";

test("pinned source URLs become file contexts", () => {
  assert.deepEqual(
    parseRepoKartaContextURL(
      "https://repo.example.com/source/42?rev=abc123&path=internal%2Fapp%2Fapp.go&lines=1-20#L7",
      baseURL
    ),
    {
      kind: "file",
      repository_id: 42,
      revision: "abc123",
      path: "internal/app/app.go"
    }
  );
});

test("repository-scoped pages become repository contexts", () => {
  for (const [value, repositoryID] of [
    ["/source/7", 7],
    ["/maps?repository=8&view=dependencies", 8],
    ["/wiki?repository=9&page=architecture", 9],
    ["/search?repo=10&q=Run", 10],
    ["/repositories?repository=11", 11]
  ]) {
    assert.deepEqual(parseRepoKartaContextURL(value, baseURL), {
      kind: "repository",
      repository_id: repositoryID
    });
  }
});

test("only one supported same-origin RepoKarta URL is converted", () => {
  for (const value of [
    "",
    "read https://repo.example.com/maps?repository=8",
    "https://other.example.com/maps?repository=8",
    "https://repo.example.com/admin?repository=8",
    "https://repo.example.com/maps?repository=0",
    "https://repo.example.com/maps?repository=not-a-number",
    "https://repo.example.com/maps"
  ]) {
    assert.equal(parseRepoKartaContextURL(value, baseURL), undefined);
  }
});
