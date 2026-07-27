const repositoryScopedPaths = new Set([
  "/",
  "/dependencies",
  "/insights",
  "/maps",
  "/repositories",
  "/search",
  "/wiki"
]);

/**
 * Converts one same-origin RepoKarta URL into a stable context selector.
 * Permission, revision, and object validation remain server-side.
 *
 * @param {string} value
 * @param {string} baseURL
 * @returns {{
 *   kind: "repository" | "file",
 *   repository_id: number,
 *   revision?: string,
 *   path?: string
 * } | undefined}
 */
export function parseRepoKartaContextURL(value, baseURL) {
  let base;
  let candidate;
  try {
    base = new URL(baseURL);
    candidate = new URL(value.trim(), base);
  } catch {
    return undefined;
  }
  if (!value.trim() || candidate.origin !== base.origin) {
    return undefined;
  }

  const pathname = candidate.pathname.replace(/\/+$/, "") || "/";
  const sourceMatch = /^\/source\/([1-9]\d*)$/.exec(pathname);
  if (sourceMatch) {
    const repositoryID = positiveInteger(sourceMatch[1]);
    if (!repositoryID) {
      return undefined;
    }
    const revision = candidate.searchParams.get("rev")?.trim() || "";
    const path = candidate.searchParams.get("path")?.trim() || "";
    return {
      kind: path ? "file" : "repository",
      repository_id: repositoryID,
      ...(revision ? { revision } : {}),
      ...(path ? { path } : {})
    };
  }

  if (!repositoryScopedPaths.has(pathname)) {
    return undefined;
  }
  const repositoryID = positiveInteger(
    candidate.searchParams.get("repository") || candidate.searchParams.get("repo") || ""
  );
  if (!repositoryID) {
    return undefined;
  }
  const revision = (
    candidate.searchParams.get("rev") ||
    candidate.searchParams.get("revision") ||
    ""
  ).trim();
  return {
    kind: "repository",
    repository_id: repositoryID,
    ...(revision ? { revision } : {})
  };
}

function positiveInteger(value) {
  if (!/^[1-9]\d*$/.test(value)) {
    return undefined;
  }
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) ? parsed : undefined;
}
