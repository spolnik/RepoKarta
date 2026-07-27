export type ContextMention = {
  kind: "repository" | "file" | "directory" | "symbol";
  query: string;
  start: number;
  end: number;
};

export function activeContextMention(
  value: string,
  caret: number
): ContextMention | undefined;
