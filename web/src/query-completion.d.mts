export type QueryCompletionEdit = {
  insert_text: string;
  replace_start: number;
  replace_end: number;
};

export function applyQueryCompletion(
  value: string,
  completion: QueryCompletionEdit
): { value: string; cursor: number };
