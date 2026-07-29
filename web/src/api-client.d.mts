import type {
  ArtifactProgressResponse,
  DependencyRefreshProgressResponse,
  HealthResponse,
  ProviderStatusesResponse,
  WikiPageResponse,
  WikiSiteResponse
} from "./generated/api-contract";

export class APIRequestError extends Error {
  status: number;
  constructor(message: string, status: number);
}

export function apiFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response>;
export function requireAPIResponse(response: Response, fallback?: string): Promise<Response>;
export function apiJSON<T>(
  input: RequestInfo | URL,
  guard: (value: unknown) => value is T,
  init?: RequestInit
): Promise<T>;
export function getHealth(): Promise<HealthResponse>;
export function getArtifactProgress(): Promise<ArtifactProgressResponse>;
export function getDependencyRefreshProgress(
  endpoint: string
): Promise<DependencyRefreshProgressResponse>;
export function getProviderStatuses(): Promise<ProviderStatusesResponse>;
export function getWikiSite(repositoryID: number): Promise<WikiSiteResponse>;
export function getWikiPage(repositoryID: number, slug: string): Promise<WikiPageResponse>;
