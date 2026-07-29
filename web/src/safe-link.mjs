/**
 * Resolve a browser link and admit only HTTP(S). Relative application URLs are
 * allowed because they resolve against an HTTP(S) page origin.
 *
 * @param {unknown} value
 * @param {string | URL} base
 * @returns {URL | null}
 */
export function safeHTTPURL(value, base) {
  if (typeof value !== "string" || !value.trim()) {
    return null;
  }
  try {
    const resolved = new URL(value, base);
    return resolved.protocol === "http:" || resolved.protocol === "https:"
      ? resolved
      : null;
  } catch {
    return null;
  }
}

/**
 * @param {HTMLAnchorElement} anchor
 * @param {unknown} value
 * @param {string | URL} base
 */
export function setSafeHTTPLink(anchor, value, base) {
  const resolved = safeHTTPURL(value, base);
  if (!resolved) {
    anchor.removeAttribute("href");
    anchor.removeAttribute("target");
    anchor.removeAttribute("rel");
    anchor.setAttribute("aria-disabled", "true");
    anchor.dataset.unsafeLink = "true";
    return false;
  }
  anchor.href = resolved.href;
  anchor.removeAttribute("aria-disabled");
  delete anchor.dataset.unsafeLink;
  return true;
}
