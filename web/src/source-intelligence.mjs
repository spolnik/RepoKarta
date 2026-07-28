const identifierPattern = /^[\p{L}_$][\p{L}\p{N}_$]*$/u;

export function sourceSelection(value) {
  const query = String(value ?? "").trim();
  if (!query || query.length > 240 || query.includes("\n")) {
    return undefined;
  }
  return {
    query,
    mode: identifierPattern.test(query) ? "references" : "zoekt"
  };
}

export function sourceSearchURL(repositoryID, query, mode) {
  const parameters = new URLSearchParams({
    q: query.trim(),
    repo: String(repositoryID),
    mode: mode === "references" ? "references" : "zoekt",
    limit: "50"
  });
  return `/api/search?${parameters.toString()}`;
}

export function sourceSearchSummary(payload) {
  const count = Number(payload?.match_count ?? 0);
  const files = Number(payload?.returned_files ?? 0);
  const provider = payload?.reference_index?.provider;
  const parts = [
    `${count} ${count === 1 ? "match" : "matches"}`,
    `${files} ${files === 1 ? "file" : "files"}`
  ];
  if (provider) {
    parts.push(`${provider} references`);
  }
  if (payload?.truncated) {
    parts.push("partial result");
  }
  return parts.join(" · ");
}
