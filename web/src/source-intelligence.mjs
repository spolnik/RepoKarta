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

const languageAliases = new Map([
  ["c++", "cpp"],
  ["c#", "csharp"],
  ["cs", "csharp"],
  ["golang", "go"],
  ["gradle", "groovy"],
  ["js", "javascript"],
  ["jsx", "javascript"],
  ["kt", "kotlin"],
  ["kts", "kotlin"],
  ["md", "markdown"],
  ["py", "python"],
  ["rb", "ruby"],
  ["rs", "rust"],
  ["sh", "bash"],
  ["shell", "bash"],
  ["ts", "typescript"],
  ["tsx", "typescript"],
  ["yml", "yaml"]
]);

const extensionLanguages = new Map([
  ["bash", "bash"],
  ["c", "c"],
  ["cc", "cpp"],
  ["cpp", "cpp"],
  ["cs", "csharp"],
  ["css", "css"],
  ["go", "go"],
  ["gradle", "groovy"],
  ["groovy", "groovy"],
  ["htm", "xml"],
  ["html", "xml"],
  ["ini", "ini"],
  ["java", "java"],
  ["js", "javascript"],
  ["json", "json"],
  ["jsx", "javascript"],
  ["kt", "kotlin"],
  ["kts", "kotlin"],
  ["md", "markdown"],
  ["mjs", "javascript"],
  ["php", "php"],
  ["properties", "ini"],
  ["py", "python"],
  ["rb", "ruby"],
  ["rs", "rust"],
  ["sh", "bash"],
  ["sql", "sql"],
  ["svg", "xml"],
  ["toml", "ini"],
  ["ts", "typescript"],
  ["tsx", "typescript"],
  ["txt", "plaintext"],
  ["xml", "xml"],
  ["yaml", "yaml"],
  ["yml", "yaml"]
]);

export function sourceHighlightLanguage(language, path) {
  const declared = String(language ?? "").trim().toLowerCase();
  if (declared) {
    return languageAliases.get(declared) ?? declared;
  }
  const fileName = String(path ?? "").split(/[\\/]/).pop() ?? "";
  const extension = fileName.includes(".")
    ? fileName.slice(fileName.lastIndexOf(".") + 1).toLowerCase()
    : "";
  return extensionLanguages.get(extension) ?? "";
}
