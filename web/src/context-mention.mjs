const contextKinds = ["repository", "file", "directory", "symbol"];

/**
 * Finds the structured context mention that owns the current caret.
 *
 * @param {string} value
 * @param {number} caret
 * @returns {{
 *   kind: "repository" | "file" | "directory" | "symbol",
 *   query: string,
 *   start: number,
 *   end: number
 * } | undefined}
 */
export function activeContextMention(value, caret) {
  const boundedCaret = Math.max(0, Math.min(caret, value.length));
  const beforeCaret = value.slice(0, boundedCaret);
  const mentions = contextKinds
    .map((kind) => ({
      kind,
      prefix: `@${kind}:`,
      start: beforeCaret.lastIndexOf(`@${kind}:`)
    }))
    .filter((mention) => mention.start >= 0)
    .sort((left, right) => right.start - left.start);
  const mention = mentions[0];
  if (!mention) {
    return undefined;
  }
  const previousBoundary = mention.start === 0 || /\s/.test(beforeCaret[mention.start - 1] ?? "");
  if (!previousBoundary || beforeCaret.slice(mention.start + mention.prefix.length).includes("\n")) {
    return undefined;
  }
  return {
    kind: mention.kind,
    query: beforeCaret.slice(mention.start + mention.prefix.length),
    start: mention.start,
    end: boundedCaret
  };
}
