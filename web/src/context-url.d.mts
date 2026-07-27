export type PastedContextSelector = {
  kind: "repository" | "file" | "directory" | "symbol";
  repository_id: number;
  revision?: string;
  path?: string;
  symbol?: string;
  symbol_kind?: string;
  line?: number;
};

export function parseRepoKartaContextURL(
  value: string,
  baseURL: string
): PastedContextSelector | { named_context_id: string } | undefined;
