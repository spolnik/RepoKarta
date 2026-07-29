/**
 * @typedef {import("./generated/api-contract").APIErrorResponse} APIErrorResponse
 * @typedef {import("./generated/api-contract").ArtifactProgressResponse} ArtifactProgressResponse
 * @typedef {import("./generated/api-contract").DependencyRefreshProgressResponse} DependencyRefreshProgressResponse
 * @typedef {import("./generated/api-contract").HealthResponse} HealthResponse
 * @typedef {import("./generated/api-contract").ProviderStatusesResponse} ProviderStatusesResponse
 * @typedef {import("./generated/api-contract").WikiPageResponse} WikiPageResponse
 * @typedef {import("./generated/api-contract").WikiSiteResponse} WikiSiteResponse
 */

export class APIRequestError extends Error {
  /**
   * @param {string} message
   * @param {number} status
   */
  constructor(message, status) {
    super(message);
    this.name = "APIRequestError";
    this.status = status;
  }
}

/**
 * The sole browser fetch boundary. It applies common request headers while
 * preserving endpoint-specific headers, signals, bodies, and cache controls.
 *
 * @param {RequestInfo | URL} input
 * @param {RequestInit} [init]
 */
export function apiFetch(input, init = {}) {
  const headers = new Headers(init.headers);
  if (!headers.has("Accept")) {
    headers.set("Accept", "application/json");
  }
  return globalThis.fetch(input, { ...init, headers });
}

/**
 * @param {Response} response
 * @param {string} [fallback]
 */
export async function requireAPIResponse(response, fallback = "") {
  if (response.ok) {
    return response;
  }
  let message = fallback || `Request failed (${response.status})`;
  try {
    const payload = /** @type {APIErrorResponse} */ (await response.clone().json());
    if (isRecord(payload.error) && typeof payload.error.message === "string") {
      message = payload.error.message;
    }
  } catch {
    const body = (await response.text()).trim();
    if (body) {
      message = body;
    }
  }
  throw new APIRequestError(message, response.status);
}

/**
 * @template T
 * @param {RequestInfo | URL} input
 * @param {(value: unknown) => value is T} guard
 * @param {RequestInit} [init]
 * @returns {Promise<T>}
 */
export async function apiJSON(input, guard, init = {}) {
  const response = await requireAPIResponse(await apiFetch(input, init));
  const value = /** @type {unknown} */ (await response.json());
  if (!guard(value)) {
    throw new APIRequestError(
      `Response from ${String(input)} did not match the application contract`,
      response.status,
    );
  }
  return value;
}

/** @param {unknown} value */
function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

/** @param {unknown} value */
function isHealthResponse(value) {
  return isRecord(value) &&
    value.status === "ok" &&
    typeof value.version === "string";
}

/** @param {unknown} value */
function isArtifactProgressResponse(value) {
  return isRecord(value) &&
    typeof value.state === "string" &&
    Number.isInteger(value.requested_repositories) &&
    Number.isInteger(value.ready_repositories) &&
    Number.isInteger(value.pending_repositories);
}

/** @param {unknown} value */
function isDependencyRefreshProgressResponse(value) {
  return isRecord(value) &&
    typeof value.state === "string" &&
    Number.isInteger(value.total) &&
    Number.isInteger(value.completed) &&
    Number.isInteger(value.failed) &&
    Number.isInteger(value.skipped);
}

/** @param {unknown} value */
function isProviderStatusesResponse(value) {
  return isRecord(value) &&
    Array.isArray(value.providers) &&
    value.providers.every((provider) =>
      isRecord(provider) &&
      typeof provider.id === "string" &&
      typeof provider.name === "string" &&
      typeof provider.available === "boolean" &&
      typeof provider.authenticated === "boolean"
    );
}

/** @param {unknown} value */
function isWikiPageResponse(value) {
  return isRecord(value) &&
    Number.isInteger(value.repository_id) &&
    typeof value.slug === "string" &&
    typeof value.title === "string" &&
    typeof value.status === "string" &&
    Array.isArray(value.supporting_files) &&
    Array.isArray(value.citations);
}

/** @param {unknown} value */
function isWikiSiteResponse(value) {
  return isRecord(value) &&
    Number.isInteger(value.repository_id) &&
    typeof value.repository === "string" &&
    typeof value.revision === "string" &&
    Array.isArray(value.pages) &&
    value.pages.every(isWikiPageResponse);
}

/** @returns {Promise<HealthResponse>} */
export function getHealth() {
  return apiJSON("/healthz", isHealthResponse, { cache: "no-store" });
}

/** @returns {Promise<ArtifactProgressResponse>} */
export function getArtifactProgress() {
  return apiJSON("/api/artifacts/progress", isArtifactProgressResponse, {
    cache: "no-store",
  });
}

/**
 * @param {string} endpoint
 * @returns {Promise<DependencyRefreshProgressResponse>}
 */
export function getDependencyRefreshProgress(endpoint) {
  return apiJSON(endpoint, isDependencyRefreshProgressResponse, {
    cache: "no-store",
  });
}

/** @returns {Promise<ProviderStatusesResponse>} */
export function getProviderStatuses() {
  return apiJSON("/api/providers", isProviderStatusesResponse, {
    cache: "no-store",
  });
}

/**
 * @param {number} repositoryID
 * @returns {Promise<WikiSiteResponse>}
 */
export function getWikiSite(repositoryID) {
  return apiJSON(
    `/api/wiki?repository=${repositoryID}`,
    isWikiSiteResponse,
    { cache: "no-store" },
  );
}

/**
 * @param {number} repositoryID
 * @param {string} slug
 * @returns {Promise<WikiPageResponse>}
 */
export function getWikiPage(repositoryID, slug) {
  return apiJSON(
    `/api/wiki/${repositoryID}/${encodeURIComponent(slug)}`,
    isWikiPageResponse,
    { cache: "no-store" },
  );
}
