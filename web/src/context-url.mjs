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
 *   kind: "repository" | "file" | "directory" | "symbol",
 *   repository_id: number,
 *   revision?: string,
 *   path?: string,
 *   symbol?: string,
 *   symbol_kind?: string,
 *   line?: number
 * } | {
 *   named_context_id: string
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
  const namedContextMatch = /^\/contexts\/([^/]+)$/.exec(pathname);
  if (namedContextMatch) {
    try {
      return { named_context_id: decodeURIComponent(namedContextMatch[1]) };
    } catch {
      return undefined;
    }
  }
  if (pathname === "/contexts") {
    const repositoryID = positiveInteger(candidate.searchParams.get("repository") || "");
    const kind = candidate.searchParams.get("kind")?.trim().toLowerCase() || "";
    if (!repositoryID || !["repository", "file", "directory", "symbol"].includes(kind)) {
      return undefined;
    }
    const revision = candidate.searchParams.get("revision")?.trim() || "";
    const path = candidate.searchParams.get("path")?.trim() || "";
    const symbol = candidate.searchParams.get("symbol")?.trim() || "";
    const symbolKind = candidate.searchParams.get("symbol_kind")?.trim() || "";
    const line = positiveInteger(candidate.searchParams.get("line") || "");
    return {
      kind,
      repository_id: repositoryID,
      ...(revision ? { revision } : {}),
      ...(path ? { path } : {}),
      ...(symbol ? { symbol } : {}),
      ...(symbolKind ? { symbol_kind: symbolKind } : {}),
      ...(line ? { line } : {})
    };
  }
  const sourceMatch = /^\/source\/([1-9]\d*)$/.exec(pathname);
  if (sourceMatch) {
    const repositoryID = positiveInteger(sourceMatch[1]);
    if (!repositoryID) {
      return undefined;
    }
    const revision = candidate.searchParams.get("rev")?.trim() || "";
    const path = candidate.searchParams.get("path")?.trim() || "";
    const focusLine = positiveInteger(candidate.searchParams.get("focus")?.split("-", 1)[0] || "");
    const hashLine = /^#L([1-9]\d*)$/.exec(candidate.hash)?.[1] || "";
    const line = focusLine || positiveInteger(hashLine);
    return {
      kind: path ? "file" : "repository",
      repository_id: repositoryID,
      ...(revision ? { revision } : {}),
      ...(path ? { path } : {}),
      ...(path && line ? { line } : {})
    };
  }
  const projectMatch = /^\/projects\/([1-9]\d*)$/.exec(pathname);
  if (projectMatch) {
    const repositoryID = positiveInteger(projectMatch[1]);
    if (!repositoryID) {
      return undefined;
    }
    const revision = candidate.searchParams.get("rev")?.trim() || "";
    const directory = candidate.searchParams.get("path")?.trim() || "";
    return {
      kind: directory ? "directory" : "repository",
      repository_id: repositoryID,
      ...(revision ? { revision } : {}),
      ...(directory ? { path: directory } : {})
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
