/**
 * Replace only named regions from a freshly rendered document. Missing regions
 * remain visible and are reported rather than blanking the current page.
 *
 * @param {ParentNode} current
 * @param {ParentNode} fresh
 * @param {string[]} selectors
 */
export function replaceRenderedRegions(current, fresh, selectors) {
  const replaced = [];
  const missing = [];
  for (const selector of selectors) {
    const existing = current.querySelector(selector);
    const replacement = fresh.querySelector(selector);
    if (!existing || !replacement) {
      missing.push(selector);
      continue;
    }
    existing.replaceWith(replacement);
    replaced.push(selector);
  }
  return { replaced, missing };
}
