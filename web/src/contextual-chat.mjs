const supportedModes = new Set([
  "context",
  "dependencies",
  "insights",
  "maps",
  "mcp",
  "search",
  "source",
  "wiki"
]);

/**
 * Keeps contextual-chat URLs readable and prevents arbitrary page data from
 * becoming a mode identifier.
 *
 * @param {string | undefined | null} value
 * @returns {string}
 */
export function normaliseContextMode(value) {
  const mode = value?.trim().toLowerCase() || "";
  return supportedModes.has(mode) ? mode : "context";
}

/**
 * Produces a shareable chat launch URL. The context URL has already been
 * validated by parseRepoKartaContextURL before it reaches this helper.
 *
 * @param {string} baseURL
 * @param {{
 *   mode?: string,
 *   prompt: string,
 *   contextURL?: string
 * }} launch
 * @returns {string}
 */
export function buildContextualChatURL(baseURL, launch) {
  const destination = new URL("/chat", baseURL);
  destination.searchParams.set("mode", normaliseContextMode(launch.mode));
  if (launch.contextURL?.trim()) {
    destination.searchParams.set("context_url", launch.contextURL.trim());
  }
  destination.searchParams.set("prompt", launch.prompt.trim());
  destination.searchParams.set("autostart", "true");
  return destination.toString();
}
