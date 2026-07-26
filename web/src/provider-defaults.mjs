const CLAUDE_PROVIDER_IDS = new Set(["claude", "anthropic-api"]);

/**
 * Returns the recommended model for a newly selected provider. The caller
 * still verifies that the provider actually advertised this model.
 *
 * @param {string | undefined} providerID
 */
export function recommendedProviderModel(providerID) {
  if (CLAUDE_PROVIDER_IDS.has(providerID ?? "")) {
    return "claude-opus-5";
  }
  if (providerID === "codex") {
    return "gpt-5.6-sol";
  }
  return "";
}

/**
 * Returns the recommended effort for a newly selected provider. Existing user
 * preferences take precedence in the UI.
 *
 * @param {string | undefined} providerID
 */
export function recommendedProviderEffort(providerID) {
  if (CLAUDE_PROVIDER_IDS.has(providerID ?? "")) {
    return "medium";
  }
  return "";
}
