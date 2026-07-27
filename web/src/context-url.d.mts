export type PastedContextSelector = {
  kind: "repository" | "file";
  repository_id: number;
  revision?: string;
  path?: string;
};

export function parseRepoKartaContextURL(
  value: string,
  baseURL: string
): PastedContextSelector | undefined;
