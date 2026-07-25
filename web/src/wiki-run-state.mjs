/**
 * Keeps the primary Wiki action useful during a long generation run. While a
 * run is active the action navigates back to progress; it never starts a
 * second run.
 *
 * @param {boolean} generating
 * @param {boolean} providerReady
 * @param {{
 *   planReady: boolean,
 *   surveyReady: boolean,
 *   planStale: boolean,
 *   stale: number,
 *   pending: number,
 *   failed: number
 * }} site
 */
export function wikiPrimaryAction(generating, providerReady, site) {
  if (generating) {
    return {
      disabled: false,
      mode: "view",
      label: "View active run"
    };
  }

  let label = "Refresh all";
  if (!site.planReady) {
    label = site.surveyReady ? "Resume · build knowledge map" : "Build Deep Wiki";
  } else if (site.planStale) {
    label = "Refresh knowledge map";
  } else if (site.stale > 0) {
    label = `Refresh ${site.stale} stale`;
  } else if (site.pending + site.failed > 0) {
    label = `Generate ${site.pending + site.failed} pages`;
  }

  return {
    disabled: !providerReady,
    mode: "generate",
    label
  };
}
