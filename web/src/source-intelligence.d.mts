export type SourceSelection = {
  query: string;
  mode: "references" | "zoekt";
};

export function sourceSelection(value: unknown): SourceSelection | undefined;
export function sourceSearchURL(
  repositoryID: number,
  query: string,
  mode: string
): string;
export function sourceSearchSummary(payload: unknown): string;
